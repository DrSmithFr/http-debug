# HTTP Debug Proxy — Design Specification

This document is the design reference for the project. It describes what the
tool does and why it is built the way it is. `README.md` covers how to use it.

## Objective

Provide a local HTTP debug proxy, shipped as a small Docker image, which
captures every request relayed to a target URL and exposes it in a web
interface consultable in real time.

The tool targets debugging on a developer machine, has no authentication, and
is not designed to be exposed beyond the host machine.

## Design principles

- Simplicity above all: no front-end framework, no ORM, minimal Go dependencies.
- Small final image: static Go binary, minimal base.
- Proxy transparency: relayed traffic is never modified, apart from a correlation header added to the response.
- Separation between capture (raw bytes, content-agnostic) and interpretation (per-format decoders).
- Optional persistence: history survives a restart if a volume is mounted, but nothing requires one.

## Architecture

```
cmd/debugproxy/
    main.go            — reads the configuration, starts the proxy and the web server
internal/
    proxy/
        proxy.go       — httputil.ReverseProxy with Director and ModifyResponse
        capture.go     — request/response capture, format detection, stream accumulation
        replay.go      — sends a request composed in the interface
    store/
        store.go       — single API (Put, Update, Finalize, Get, List), arbitrates memory and database
        memory.go      — in-flight requests, map[ID]*Entry guarded by a mutex
        sqlite.go      — finished requests, one row per Entry
        blobs.go       — oversized bodies written to disk, referenced by path
        events.go      — notification channel feeding the event broadcaster
    web/
        server.go      — REST API, event stream, static file serving
        static/        — HTML/CSS/JS of the interface, no framework
```

The `store` package exposes a single API that looks in memory first, then in the
database if the entry is no longer there. The rest of the code does not know
where the data lives.

## Request lifecycle

1. The proxy intercepts the request, reads its body, then re-injects it into `Request.Body` so the relay remains possible.
2. The proxy creates an `Entry` with status `pending` and hands it to the `store`, which assigns it an identifier and publishes a `created` event.
3. The proxy relays the request and captures the response as it flows: line-by-line accumulation for streamed responses, read to completion for the others.
4. Each fragment received publishes a `delta` event; each state change publishes an `updated` event.
5. On closure the entry becomes `done` or `error`: bodies over the size threshold are written to disk, the row goes to the database, and the memory entry is dropped.

A request whose response never arrives stays `pending` indefinitely. A periodic
sweep closes in `error` any entry idle for longer than `PENDING_TIMEOUT`, to
avoid a silent accumulation in memory.

## `Entry` schema

Go type shared by the memory and the database backends:

- `id` — unique identifier, also used in the detail URL and in body file names.
- `method`, `url` — relayed method and URL.
- `status` — `pending`, `done`, or `error`.
- `status_code` — HTTP response code, once known.
- `error` — network error message if the backend never answered.
- `request_format`, `response_format` — `json`, `xml`, `ndjson`, `sse`, or `raw`.
- `is_ollama` — true if the path matches a known Ollama endpoint.
- `is_replay` — true if the request was issued from the interface rather than received from a client.
- `request_headers`, `response_headers` — serialized as JSON.
- `request_body`, `response_body` — inline content under the size threshold, otherwise `NULL`.
- `request_body_path`, `response_body_path` — file path if the body went to disk, otherwise `NULL`.
- `request_body_size`, `response_body_size` — full byte count, whether the body is inline or on disk.
- `started_at` — timestamp at which the proxy received the request.
- `finished_at` — closure timestamp.
- `ttfb_ms` — latency until the first byte received from the backend.
- `stream_ms` — duration between the first and the last fragment, `NULL` if the response is not streamed.
- `total_ms` — total duration, from receiving the request to closure.

## Storage

### Memory

Only `pending` requests: a `map[ID]*Entry` guarded by a `sync.RWMutex`.

A long streamed response accumulates its body in memory until closure. Past
`MAX_INLINE_BODY_SIZE`, accumulation switches to a file open for writing, and
only a leading preview stays in memory.

### SQLite database

History of finished requests.

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
    request_body_size   INTEGER NOT NULL DEFAULT 0,
    response_headers    TEXT,
    response_body       BLOB,
    response_body_path  TEXT,
    response_body_size  INTEGER NOT NULL DEFAULT 0,
    started_at          INTEGER NOT NULL,
    finished_at         INTEGER,
    ttfb_ms             INTEGER,
    stream_ms           INTEGER,
    total_ms            INTEGER
);

CREATE INDEX idx_requests_started_at ON requests (started_at DESC);
```

`PRAGMA journal_mode=WAL` avoids blocking between writes (closing an entry) and
reads (the interface listing entries).

The driver must be a pure Go implementation, without `cgo`, to preserve static
compilation and the multi-architecture build.

### Body files

Bodies over `MAX_INLINE_BODY_SIZE` are written to
`${DATA_DIR}/blobs/<id>-request` or `${DATA_DIR}/blobs/<id>-response`. No
subdirectory hierarchy: the number of files stays low for local debug use.

### Retention

The history is capped at `MAX_ENTRIES` requests. On each insertion, entries
older than the cap are deleted from the database and their body files removed in
the same operation.

An orphan file can survive an abrupt container stop. At startup, the `store`
deletes files under `blobs/` whose identifier appears in no row.

## Format detection

The function `detectFormat(contentType string, body []byte) Format` in
`capture.go`, with this priority:

1. The `Content-Type` header.
2. Failing that, inspection of the first non-blank bytes of the body.

Recognized formats: `json`, `xml`, `ndjson`, `sse`, `raw`.

`ndjson` is only claimed when every non-empty line parses as JSON on its own and
there is more than one such line — a pretty-printed JSON document also spans
several lines and must not be mistaken for a stream. A body whose first line
alone is valid therefore falls back to `json`, which is the honest description
of a truncated document.

The format determined on the response side drives the capture strategy:
`ndjson` and `sse` trigger fragment-by-fragment accumulation and the emission of
`delta` events; the others are read to the end of the response.

When a response carries no usable `Content-Type`, detection is deferred to the
first chunk received, before it is consumed, and the entry's format is updated.

## Ollama detection

The `is_ollama` flag is raised when the path matches a known endpoint:
`/api/generate`, `/api/chat`, `/api/embeddings`. It only serves display — a
badge in the list, a message preview tab in the detail view.

This flag does not drive capture. An Ollama call with `"stream": false` returns
a single JSON object, and the OpenAI-compatible endpoints
(`/v1/chat/completions`) return `sse` rather than `ndjson`. The capture strategy
therefore depends on the format detected on the response, never on the path.

For the preview, the Ollama decoder concatenates the `response` field
(`/api/generate` endpoint) or `message.content` (`/api/chat` endpoint) of each
fragment, reconstructing the message as the end user would have seen it.

The rest of the `Ollama` tab is derived in the browser from the captured request
body, which the detail route already carries; no extra route or server-side
decoding is involved. The conversation comes from `messages`, or from
`system`/`prompt` for `/api/generate`, which is presented in the same shape.

Tools are split between plain functions and MCP servers. MCP declarations reach a
model in three shapes depending on the client, and all of them are folded into a
single server list: a top-level `mcp_servers` object, a tool of `type: "mcp"`
carrying `server_label` and `allowed_tools`, and tools flattened into the `tools`
array under an `mcp__<server>__<tool>` name. A request body that spilled to disk
is not inlined in the detail payload, so the tab offers to load it on demand.

## Correlation with the detail page

The proxy adds an `X-Debug-Url` header to each relayed response, containing the
absolute URL of the detail page for the entry.

A header is preferred over a cookie: the clients concerned are mostly programs
— Open WebUI, `curl`, an SDK — which surface response headers in their logs but
ignore cookies. A header also stays visible in `curl -v` and in a browser's
network tab, with no side effect on the client's session.

Optional: when `SET_DEBUG_COOKIE` is enabled, the same URL is also set as a
cookie, for cases where the client is a browser and reading headers is
impractical.

## Web interface

### Home page

List of requests, updated automatically by the event stream. Each row shows the
method, the path, the status code, the total duration, and a badge for
`is_ollama` or `is_replay`. A `pending` entry shows a progress indicator and a
running count of the time waited so far, rather than a final duration. The same
elapsed reading appears among the metrics of the detail view while the response
is still in flight.

Clicking one opens a preview taking half the screen, in the spirit of a
messaging interface. The list stays visible and keeps updating. The divider
between the two panes is draggable — also operable from the keyboard, and reset
by a double-click — and the chosen width is kept in `localStorage`, clamped so
neither pane can be squeezed away.

Neither pane scrolls as a whole. In the detail pane the header and the tab bar
are pinned and only the tab content moves, so the URL, the metrics and the tab
bar stay reachable however long the payload is.

### Detail view

Request and response presented in two panes:

- Every value — header, query parameter, body field — is individually copyable.
- The body is consultable raw and as an expandable interactive view for the `json` and `xml` formats.
- The `ttfb_ms`, `stream_ms` and `total_ms` metrics are shown together, to distinguish backend latency from slow generation.
- For an `is_ollama` entry, a dedicated `Ollama` tab groups everything the exchange carries: the message reconstructed from the fragments and updated live during streaming, the conversation sent in the request, the tools declared to the model, and the MCP servers behind them.
- A button exports the request as a `curl` command.

A body that spilled to disk is not inlined in the detail payload; the view
reports its size and loads it on demand from the raw body route.

### Replay by editing

No separate replay action. From an existing entry, a pre-filled form (method,
URL, headers, body) opens in edit mode, and submitting it sends the request.

The request issued is captured like any other and appears in the list with
`is_replay` set to true. It is sent by the server, not the browser, which avoids
any cross-origin restriction and allows targeting an arbitrary URL rather than
only the configured target.

## API contract

### REST routes

- `GET /api/requests` — list of entries, metadata only, without bodies. Parameters `limit` and `before` (cursor on `started_at`).
- `GET /api/requests/{id}` — full entry. Bodies under the threshold are inline; past it, the response carries their size and the URL of the raw body.
- `GET /api/requests/{id}/body/{side}` — raw body, with `side` being `request` or `response`. Serves the inline content or the file, restoring the original `Content-Type`.
- `POST /api/requests` — sends a request composed in the interface. Body `{method, url, headers, body}`, response `{id}` of the entry created.
- `DELETE /api/requests` — clears the history, database rows and body files included.
- `GET /api/config` — effective target URL, so the interface can display what it sits in front of.

### Event stream

A single `Server-Sent Events` stream on `GET /api/events`, broadcast to every
connected client:

- `created` — new entry, payload identical to a list row.
- `updated` — state or metric change, payload `{id, status, status_code, ttfb_ms, stream_ms, total_ms}`.
- `delta` — fragment received on a streamed response, payload `{id, chunk}`. Emitted only for the `ndjson` and `sse` formats.
- `deleted` — entry purged by retention, payload `{id}`.

Full bodies never travel on this stream: the detail view fetches them over the
REST routes.

## Implementation constraints

- `httputil.ReverseProxy` buffers the response by default, which breaks streaming as seen by the client. The `FlushInterval` field must be `-1` so fragments are forwarded as they are received. `ReverseProxy`'s automatic detection only covers `text/event-stream` and would leave Ollama's `ndjson` buffered.
- Reading `Request.Body` in the `Director` consumes it. The body must be read in full and re-injected with `io.NopCloser(bytes.NewReader(body))` before the relay.
- Response body capture goes through an `io.ReadCloser` wrapping the original, copying bytes to the `store` as the client reads them. This wrapper must never delay nor alter the forwarded stream.
- The event broadcaster writes to several clients: a slow client must not block capture. Each subscriber gets a buffered channel, and a saturated channel disconnects the subscriber rather than blocking the emitter.

## Project tooling

### Configuration

Configured through environment variables. Defaults are listed in
[`README.md`](../README.md#configuration).

- `TARGET_URL` — target URL of the proxy. The only required variable.
- `LISTEN_ADDR` — listen address of the proxy.
- `WEB_ADDR` — listen address of the web interface.
- `PUBLIC_URL` — URL base used to build the `X-Debug-Url` header.
- `DATA_DIR` — directory for the database and the body files.
- `MAX_INLINE_BODY_SIZE` — threshold past which a body goes to disk.
- `MAX_ENTRIES` — retention cap on the history.
- `PENDING_TIMEOUT` — idle delay after which a `pending` entry is closed in error.
- `SET_DEBUG_COOKIE` — also sets the detail URL as a cookie.

### Docker image

Multi-stage build: the binary is compiled with `CGO_ENABLED=0`, and the final
image sits on a minimal static base containing the binary and the front-end
files, embedded with `go:embed`.

The base must provide root certificates, otherwise an HTTPS target fails
verification. A static `distroless` base fits; `scratch` would require copying
the certificates manually.

### Docker Compose

An example `docker-compose.yml` at the root shows the typical usage: the
`debugproxy` service with `TARGET_URL` pointing at another service on the same
network, a volume for `DATA_DIR`, and the interface port published.

### Continuous integration

A GitHub Actions workflow, several jobs:

- **Lint** — `go vet` and `golangci-lint`.
- **Test** — `go test ./... -race -cover`, on every push and every pull request.
- **Build** — compilation for `linux/amd64` and `linux/arm64`.
- **Publish** — build and publish the multi-architecture image to the GitHub Container Registry, triggered by version tags and by commits on `main` for the `latest` tag.

### Tests

- Unit tests on `store`: full entry lifecycle, memory-to-database transition, retention applied, associated files removed.
- Unit tests on `detectFormat`: absent `Content-Type`, empty body, truncated JSON, NDJSON whose first line alone is valid.
- Unit tests on the Ollama decoder, for both fragment shapes.
- Integration tests on `proxy`: fake backend, relayed request, comparison of the captured entry against the real traffic. A dedicated case verifies that a streamed response reaches the client unbuffered and fragment by fragment.
- No end-to-end test on the interface: the complexity is not justified at this scale.

### Documentation

- `README.md` — one-sentence presentation, copy-paste `docker-compose.yml` example, environment variable table, interface screenshot.
- `CONTRIBUTING.md` — running the tests locally, commit convention.
- `LICENSE` at the root.
- This specification, kept in the repository.

### Versioning

Semantic versioning on Git tags, which trigger the image publication:

- **Major** — a break in the API contract or in the expected environment variables.
- **Minor** — a backward-compatible feature, such as a newly detected format or an extra route.
- **Patch** — a fix with no observable change in behaviour.

`CHANGELOG.md` is generated from commit messages, the `Conventional Commits`
convention being adopted.

## Out of scope

- No authentication or access control: the tool assumes local use exclusively, and displays captured authentication headers in clear text.
- No TLS interception: the proxy terminates in HTTP and relays to a target that may be in HTTPS, but does not decrypt traffic already encrypted between a client and its target.
- No UDP or raw TCP handling: the scope is HTTP.
- No high availability or replication: one process, one database file.
