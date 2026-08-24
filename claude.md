# Proxy de debug HTTP

## Objectif

Fournir un proxy HTTP de debug local, livré en image Docker légère, qui capture chaque requête relayée vers une URL cible et l'expose dans une interface web consultable en temps réel.

L'outil cible un usage de debug sur poste de développement, sans authentification, et n'est pas conçu pour être exposé au-delà de la machine hôte.

## Principes de conception

- Simplicité avant tout : pas de framework front, pas d'ORM, dépendances Go minimales.
- Image finale petite : binaire Go statique, base minimale.
- Transparence du proxy : le trafic relayé n'est jamais modifié, à l'exception d'un en-tête de corrélation ajouté à la réponse.
- Séparation entre la capture (octets bruts, agnostique au contenu) et l'interprétation (décodeurs par format).
- Persistance optionnelle : l'historique survit à un redémarrage si un volume est monté, mais rien ne l'exige.

## Architecture

```
cmd/debugproxy/
    main.go            — lit la configuration, démarre le proxy et le serveur web
internal/
    proxy/
        proxy.go       — httputil.ReverseProxy avec Director et ModifyResponse
        capture.go     — capture requête/réponse, détection de format, accumulation du streaming
        replay.go      — envoi d'une requête composée depuis l'interface
    store/
        store.go       — API unique (Put, Update, Finalize, Get, List), arbitre mémoire/base
        memory.go      — requêtes en cours, map[ID]*Entry protégée par mutex
        sqlite.go      — requêtes terminées, une ligne par Entry
        blobs.go       — corps volumineux écrits sur disque, référencés par chemin
        events.go      — canal de notification vers le diffuseur d'événements
    web/
        server.go      — API REST, flux d'événements, service des fichiers statiques
        static/        — HTML/CSS/JS de l'interface, sans framework
```

Le paquet `store` expose une API unique qui consulte d'abord la mémoire, puis la base si l'entrée n'y est plus. Le reste du code ignore où vit la donnée.

## Cycle de vie d'une requête

1. Le proxy intercepte la requête, lit son corps, puis le réinjecte dans `Request.Body` pour que le relais reste possible.
2. Le proxy crée une `Entry` de statut `pending` et la remet au `store`, qui lui assigne un identifiant et publie un événement `created`.
3. Le proxy relaie la requête et capture la réponse au fil de l'eau : accumulation ligne par ligne pour les flux streamés, lecture complète pour les autres.
4. Chaque fragment reçu publie un événement `delta` ; chaque changement d'état publie un événement `updated`.
5. À la clôture, l'entrée passe en `done` ou `error` : les corps dépassant le seuil de taille partent sur disque, la ligne part en base, l'entrée en mémoire est supprimée.

Une requête dont la réponse n'arrive jamais reste `pending` indéfiniment. Un balayage périodique clôt en `error` toute entrée dont l'inactivité dépasse `PENDING_TIMEOUT`, pour éviter une accumulation silencieuse en mémoire.

## Schéma de l'`Entry`

Type Go partagé par la mémoire et la base :

- `id` : identifiant unique, également utilisé dans l'URL de détail et le nom des fichiers de corps.
- `method`, `url` : méthode et URL relayées.
- `status` : `pending`, `done`, ou `error`.
- `status_code` : code de réponse HTTP, une fois connu.
- `error` : message d'erreur réseau si le backend n'a jamais répondu.
- `request_format`, `response_format` : `json`, `xml`, `ndjson`, `sse`, ou `raw`.
- `is_ollama` : vrai si le path correspond à un endpoint Ollama connu.
- `is_replay` : vrai si la requête a été émise depuis l'interface plutôt que reçue d'un client.
- `request_headers`, `response_headers` : sérialisés en JSON.
- `request_body`, `response_body` : contenu inline sous le seuil de taille, sinon `NULL`.
- `request_body_path`, `response_body_path` : chemin du fichier si le corps est passé sur disque, sinon `NULL`.
- `started_at` : horodatage de réception de la requête par le proxy.
- `finished_at` : horodatage de clôture.
- `ttfb_ms` : latence jusqu'au premier octet reçu du backend.
- `stream_ms` : durée entre le premier et le dernier fragment, `NULL` si la réponse n'est pas streamée.
- `total_ms` : durée totale, de la réception de la requête à la clôture.

## Stockage

### Mémoire

Uniquement les requêtes `pending` : `map[ID]*Entry` protégée par un `sync.RWMutex`.

Un flux streamé long accumule son corps en mémoire jusqu'à la clôture. Au-delà de `MAX_INLINE_BODY_SIZE`, l'accumulation bascule vers un fichier ouvert en écriture, et seul un préfixe reste en mémoire pour l'aperçu.

### Base SQLite

Historique des requêtes terminées.

```sql
CREATE TABLE requests (
    id                  TEXT PRIMARY KEY,
    method              TEXT NOT NULL,
    url                 TEXT NOT NULL,
    status              TEXT NOT NULL,
    status_code         INTEGER,
    error               TEXT,
    request_format      TEXT,
    response_format     TEXT,
    is_ollama           INTEGER NOT NULL DEFAULT 0,
    is_replay           INTEGER NOT NULL DEFAULT 0,
    request_headers     TEXT,
    request_body        BLOB,
    request_body_path   TEXT,
    response_headers    TEXT,
    response_body       BLOB,
    response_body_path  TEXT,
    started_at          INTEGER NOT NULL,
    finished_at         INTEGER,
    ttfb_ms             INTEGER,
    stream_ms           INTEGER,
    total_ms            INTEGER
);

CREATE INDEX idx_requests_started_at ON requests (started_at DESC);
```

`PRAGMA journal_mode=WAL` évite les blocages entre l'écriture (clôture d'une entrée) et la lecture (interface consultant la liste).

Le driver doit être une implémentation pure Go, sans `cgo`, pour préserver la compilation statique et le build multi-architecture.

### Fichiers de corps

Les corps dépassant `MAX_INLINE_BODY_SIZE` sont écrits sous `${DATA_DIR}/blobs/<id>-request` ou `${DATA_DIR}/blobs/<id>-response`. Pas de hiérarchie de sous-dossiers : le volume de fichiers reste faible pour un usage de debug local.

### Rétention

L'historique est plafonné à `MAX_ENTRIES` requêtes. À chaque insertion, les entrées les plus anciennes au-delà du plafond sont supprimées de la base, et leurs fichiers de corps supprimés dans la même opération.

Un fichier orphelin peut subsister après un arrêt brutal du conteneur. Au démarrage, le `store` supprime les fichiers de `blobs/` dont l'identifiant n'apparaît dans aucune ligne.

## Détection de format

Fonction `detectFormat(contentType string, body []byte) Format` dans `capture.go`, avec cette priorité :

1. En-tête `Content-Type`.
2. À défaut, inspection des premiers octets non blancs du corps.

Formats reconnus : `json`, `xml`, `ndjson`, `sse`, `raw`.

Le format déterminé côté réponse pilote la stratégie de capture : les formats `ndjson` et `sse` déclenchent une accumulation fragment par fragment et l'émission d'événements `delta` ; les autres sont lus jusqu'à la fin de la réponse.

## Détection Ollama

Le drapeau `is_ollama` est levé quand le path correspond à un endpoint connu : `/api/generate`, `/api/chat`, `/api/embeddings`. Il sert uniquement à l'affichage — badge dans la liste, onglet d'aperçu du message dans la vue détaillée.

Ce drapeau ne pilote pas la capture. Un appel Ollama avec `"stream": false` renvoie un objet JSON unique, et les endpoints compatibles OpenAI (`/v1/chat/completions`) renvoient du `sse` plutôt que du `ndjson`. La stratégie de capture dépend donc du format détecté sur la réponse, jamais du path.

Pour l'aperçu, le décodeur Ollama concatène les champs `response` (endpoint `/api/generate`) ou `message.content` (endpoint `/api/chat`) de chaque fragment, et reconstruit ainsi le message tel que l'utilisateur final le verrait.

## Corrélation avec la page de détails

Le proxy ajoute à chaque réponse relayée un en-tête `X-Debug-Url` contenant l'URL absolue de la page de détails de l'entrée.

L'en-tête est retenu plutôt que le cookie : les clients concernés sont majoritairement des programmes — Open WebUI, `curl`, un SDK — qui exposent les en-têtes de réponse dans leurs logs mais ignorent les cookies. Un en-tête reste par ailleurs visible dans `curl -v` et dans l'onglet réseau d'un navigateur, sans effet de bord sur la session du client.

Facultatif : quand `SET_DEBUG_COOKIE` est activé, la même URL est également posée en cookie, pour les cas où le client est un navigateur et où la lecture des en-têtes est peu pratique.

## Interface web

### Page d'accueil

Liste des requêtes, mise à jour automatiquement par le flux d'événements. Chaque ligne affiche la méthode, le path, le code de statut, la durée totale, et un badge pour `is_ollama` ou `is_replay`. Une entrée `pending` affiche un indicateur de progression plutôt qu'une durée.

Un clic ouvre un aperçu occupant la moitié de l'écran, dans l'esprit d'une interface de messagerie. La liste reste visible et continue de se mettre à jour.

### Vue détaillée

Requête et réponse présentées en deux volets :

- Chaque valeur — en-tête, paramètre de requête, champ du corps — est copiable individuellement.
- Le corps est consultable en brut et en vue interactive dépliable pour les formats `json` et `xml`.
- Les métriques `ttfb_ms`, `stream_ms`, et `total_ms` sont affichées ensemble, pour distinguer une latence backend d'une génération lente.
- Pour une entrée `is_ollama`, un onglet affiche le message reconstruit à partir des fragments, mis à jour en direct pendant le streaming.
- Un bouton exporte la requête en commande `curl`.

### Rejeu par édition

Pas d'action de rejeu distincte. Depuis une entrée existante, un formulaire pré-rempli (méthode, URL, en-têtes, corps) s'ouvre en édition, et sa validation envoie la requête.

La requête émise est capturée comme n'importe quelle autre et apparaît dans la liste avec `is_replay` à vrai. Elle est envoyée par le serveur, pas par le navigateur, ce qui évite toute contrainte de partage de ressources entre origines et permet de viser une URL arbitraire plutôt que la seule cible configurée.

## Contrat d'API

### Routes REST

- `GET /api/requests` : liste des entrées, métadonnées seules, sans corps. Paramètres `limit` et `before` (curseur sur `started_at`).
- `GET /api/requests/{id}` : entrée complète. Les corps sous le seuil sont inline ; au-delà, la réponse porte leur taille et l'URL du corps brut.
- `GET /api/requests/{id}/body/{side}` : corps brut, avec `side` valant `request` ou `response`. Sert le contenu inline ou le fichier, en restituant le `Content-Type` d'origine.
- `POST /api/requests` : envoie une requête composée dans l'interface. Corps `{method, url, headers, body}`, réponse `{id}` de l'entrée créée.
- `DELETE /api/requests` : vide l'historique, base et fichiers de corps compris.

### Flux d'événements

Un unique flux `Server-Sent Events` sur `GET /api/events`, diffusé à tous les clients connectés :

- `created` : nouvelle entrée, charge utile identique à une ligne de liste.
- `updated` : changement d'état ou de métrique, charge utile `{id, status, status_code, ttfb_ms, stream_ms, total_ms}`.
- `delta` : fragment reçu sur une réponse streamée, charge utile `{id, chunk}`. Émis uniquement pour les formats `ndjson` et `sse`.
- `deleted` : entrée purgée par la rétention, charge utile `{id}`.

Les corps complets ne transitent jamais par ce flux : la vue détaillée les récupère par les routes REST.

## Contraintes d'implémentation

- `httputil.ReverseProxy` tamponne la réponse par défaut, ce qui casse le streaming vu du client. Le champ `FlushInterval` doit valoir `-1` pour que les fragments soient transmis dès leur réception. La détection automatique de `ReverseProxy` ne couvre que `text/event-stream` et laisserait passer le `ndjson` d'Ollama en mode tamponné.
- Lire `Request.Body` dans le `Director` le consomme. Le corps doit être relu intégralement puis réinjecté via `io.NopCloser(bytes.NewReader(body))` avant le relais.
- La capture du corps de réponse passe par un `io.ReadCloser` enveloppant celui d'origine, qui recopie les octets vers le `store` au fil de la lecture par le client. Cette enveloppe ne doit jamais retarder ni altérer le flux transmis.
- Le diffuseur d'événements écrit vers plusieurs clients : un client lent ne doit pas bloquer la capture. Chaque abonné dispose d'un canal tamponné, et un canal saturé provoque la déconnexion de l'abonné plutôt que le blocage de l'émetteur.

## Outillage projet

### Configuration

Paramétrage par variables d'environnement :

- `TARGET_URL` : URL cible du proxy. Seule variable obligatoire.
- `LISTEN_ADDR` : adresse d'écoute du proxy.
- `WEB_ADDR` : adresse d'écoute de l'interface web.
- `PUBLIC_URL` : base d'URL utilisée pour construire l'en-tête `X-Debug-Url`.
- `DATA_DIR` : répertoire de la base et des fichiers de corps.
- `MAX_INLINE_BODY_SIZE` : seuil de bascule d'un corps vers le disque.
- `MAX_ENTRIES` : plafond de rétention de l'historique.
- `PENDING_TIMEOUT` : délai d'inactivité au terme duquel une entrée `pending` est clôturée en erreur.
- `SET_DEBUG_COOKIE` : pose également l'URL de détail en cookie.

### Image Docker

Build multi-étapes : compilation du binaire avec `CGO_ENABLED=0`, image finale sur une base statique minimale contenant le binaire et les fichiers du front, embarqués par `go:embed`.

La base doit fournir les certificats racine, faute de quoi une cible en HTTPS échoue à la vérification. Une base `distroless` statique convient ; `scratch` impose de copier les certificats manuellement.

### Docker Compose

Un fichier `docker-compose.yml` d'exemple à la racine, montrant l'usage type : le service `debugproxy` avec `TARGET_URL` pointant vers un autre service du même réseau, un volume pour `DATA_DIR`, et le port de l'interface exposé.

### Intégration continue

Un workflow GitHub Actions, plusieurs jobs :

- **Lint** : `go vet` et `golangci-lint`.
- **Test** : `go test ./... -race -cover`, sur chaque push et chaque pull request.
- **Build** : compilation pour `linux/amd64` et `linux/arm64`.
- **Publish** : construction et publication de l'image multi-architecture sur GitHub Container Registry, déclenchées par les tags de version et par les commits sur `main` pour le tag `latest`.

### Tests

- Tests unitaires sur `store` : cycle de vie complet d'une entrée, bascule mémoire vers base, application de la rétention, suppression des fichiers associés.
- Tests unitaires sur `detectFormat` : `Content-Type` absent, corps vide, JSON tronqué, NDJSON dont la première ligne seule est valide.
- Tests unitaires sur le décodeur Ollama, pour les deux formes de fragment.
- Test d'intégration sur `proxy` : backend factice, requête relayée, comparaison de l'entrée capturée avec le trafic réel. Un cas dédié vérifie qu'une réponse streamée arrive au client sans tampon et fragment par fragment.
- Pas de test de bout en bout sur l'interface : la complexité ne se justifie pas à cette échelle.

### Documentation

- `README.md` : présentation en une phrase, exemple `docker-compose.yml` à copier-coller, tableau des variables d'environnement, capture d'écran de l'interface.
- `CONTRIBUTING.md` : lancement des tests en local, convention de commit.
- `LICENSE` à la racine.
- Cette spécification, conservée dans le dépôt.

### Versionnement

Versionnement sémantique sur les tags Git, qui déclenchent la publication de l'image :

- Majeur : rupture du contrat d'API ou des variables d'environnement attendues.
- Mineur : fonctionnalité rétrocompatible, comme un format détecté ou une route supplémentaire.
- Correctif : correction sans changement de comportement observable.

Le fichier `CHANGELOG.md` est généré depuis les messages de commit si la convention `Conventional Commits` est adoptée, sinon tenu à la main à chaque tag.

## Points hors périmètre

- Pas d'authentification ni de contrôle d'accès : l'outil suppose un usage local exclusivement, et expose en clair les en-têtes d'authentification du trafic capturé.
- Pas d'interception TLS : le proxy termine en HTTP et relaie vers une cible qui peut être en HTTPS, mais ne déchiffre pas de trafic déjà chiffré entre client et cible.
- Pas de gestion d'UDP ni de TCP brut : le périmètre est HTTP.
- Pas de haute disponibilité ni de réplication : un processus, un fichier de base.
