# Contributing

Thanks for taking the time. This is a small, deliberately simple project — the
guidelines below exist to keep it that way.

## Getting set up

You need **Go 1.25 or later**. There is no build step for the interface: the
HTML, CSS and JavaScript under `internal/web/static/` are embedded into the
binary with `go:embed`.

```bash
git clone https://github.com/DrSmithFr/http-debug.git
cd http-debug
go build ./...
```

## Running it locally

```bash
TARGET_URL=http://localhost:11434 \
WEB_ADDR=127.0.0.1:8081 \
LISTEN_ADDR=127.0.0.1:8080 \
DATA_DIR=./data \
go run ./cmd/debugproxy
```

Send traffic to `http://localhost:8080` and open <http://localhost:8081>.
`./data` is git-ignored.

Working on the interface only? The files are embedded at build time, so restart
the process after editing them.

## Running the tests

```bash
go test ./... -race -cover
```

The suite covers the store lifecycle, format detection, the Ollama decoder, and
the proxy end to end against a fake backend — including a case asserting that a
streamed response reaches the client fragment by fragment rather than buffered.
There are no browser tests: at this scale the complexity is not worth it.

Before opening a pull request:

```bash
gofmt -l .          # must print nothing
go vet ./...
golangci-lint run   # optional locally, runs in CI
```

## Building the image

```bash
docker build -t http-debug:dev .
docker run --rm -p 8080:8080 -p 8081:8081 \
  -e TARGET_URL=http://host.docker.internal:11434 http-debug:dev
```

The binary is built with `CGO_ENABLED=0` and the final image is a distroless
static base. The SQLite driver must stay a pure Go implementation, otherwise
static compilation and the multi-architecture build both break.

## Commit convention

This project uses [Conventional Commits](https://www.conventionalcommits.org/),
which is what lets `CHANGELOG.md` be generated from history.

```
<type>(<optional scope>): <description>
```

Common types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `ci`.

```
feat(store): keep a preview of bodies written to disk
fix(proxy): flush ndjson fragments instead of buffering them
docs: document PENDING_TIMEOUT
```

A breaking change is marked with `!` after the type — `feat!: rename WEB_ADDR` —
and explained in a `BREAKING CHANGE:` footer.

## Versioning

Semantic versioning on Git tags, which trigger the image publication:

- **Major** — a break in the API contract or in the expected environment variables.
- **Minor** — a backward-compatible feature, such as a newly detected format or an extra route.
- **Patch** — a fix with no observable change in behaviour.

## Pull requests

- One concern per pull request; a small, reviewable diff beats a complete one.
- Add or update tests for behaviour you change.
- Update `README.md` when you touch a variable, a route or a default.
- Keep the design document [`docs/SPECIFICATION.md`](docs/SPECIFICATION.md) in
  sync when the change affects the architecture, the storage schema or the API
  contract.
- CI must be green: lint, tests with the race detector, and the multi-architecture build.

## Scope

The design principles are simplicity first, a small image, and a transparent
proxy. Proposals that add a front-end framework, an ORM, or a heavy dependency
are unlikely to be accepted. The
[out of scope](README.md#out-of-scope) section lists what this tool deliberately
does not do; if you need one of those, an issue discussing it first will save
you the implementation work.
