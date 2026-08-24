# Working notes for Claude Code

## What this is

A local HTTP debug proxy in Go: it relays traffic to `TARGET_URL`, captures every
exchange, and serves a live web interface. Shipped as a small Docker image.

**The design reference is [`docs/SPECIFICATION.md`](docs/SPECIFICATION.md).** Read
it before changing architecture, the storage schema, or the API contract, and keep
it in sync when a change makes it inaccurate.

## Layout

```
cmd/debugproxy/main.go   configuration from the environment, starts both servers
internal/proxy/          relay, capture, format detection, replay
internal/store/          single API over memory (pending) and SQLite (finished)
internal/web/            REST API, SSE stream, embedded static UI
```

## Working here

- Go 1.25+. `go test ./... -race -cover` before calling anything done.
- `gofmt -l .` must print nothing; `go vet ./...` must be clean.
- The UI is embedded with `go:embed`; restart the process after editing
  `internal/web/static/`.
- Commit messages follow Conventional Commits (see `CONTRIBUTING.md`).

## Things that break if you are not careful

- `ReverseProxy.FlushInterval` must stay `-1`. Its automatic detection only
  covers `text/event-stream`, so Ollama's `ndjson` would silently go back to
  being buffered. `TestStreamedResponseReachesClientUnbuffered` guards this.
- The SQLite driver must stay pure Go (`modernc.org/sqlite`). A cgo driver breaks
  `CGO_ENABLED=0` and the multi-architecture image build.
- Capture must never delay or alter the relayed stream, and a slow SSE subscriber
  must never block the capture path — a saturated subscriber channel disconnects
  the subscriber instead.
- The capture strategy follows the detected response format, never the request
  path. `is_ollama` is a display flag only.

## Deliberately out of scope

No authentication, no TLS interception, no raw TCP or UDP, no high availability.
No front-end framework, no ORM. Keep dependencies minimal.
