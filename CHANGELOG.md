# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Entries are derived from [Conventional Commits](https://www.conventionalcommits.org/)
messages; see [`CONTRIBUTING.md`](CONTRIBUTING.md).

## [Unreleased]

### Added

- HTTP proxy relaying to a configurable target and capturing every exchange, with
  streamed responses forwarded fragment by fragment rather than buffered.
- `X-Debug-Url` response header linking each relayed response to its detail page,
  and an optional cookie carrying the same URL (`SET_DEBUG_COOKIE`).
- Payload format detection (`json`, `xml`, `ndjson`, `sse`, `raw`) from the
  `Content-Type` header, falling back to sniffing the body.
- Ollama awareness: `/api/generate`, `/api/chat` and `/api/embeddings` are flagged
  and get a dedicated tab holding the assistant message reconstructed from the
  streamed fragments, the conversation sent in the request, the declared tools with
  their JSON Schema, and the MCP servers behind them.
- Storage arbitrating between in-memory pending entries and a SQLite history,
  with bodies over `MAX_INLINE_BODY_SIZE` written to disk, retention capped at
  `MAX_ENTRIES`, and orphan body files swept at startup.
- Web interface with no framework: live request list, split detail view,
  expandable JSON and XML bodies, per-value copy, curl export, and replay by
  editing an existing request. A pending request shows how long it has been
  waiting, in the list and in the detail view. The divider between the list and
  the detail pane is draggable, keyboard-operable and remembered across reloads,
  and the detail header and tab bar stay pinned while the tab content scrolls.
- REST API and a Server-Sent Events stream (`created`, `updated`, `delta`, `deleted`).
- Periodic sweep closing in error any pending entry idle for longer than
  `PENDING_TIMEOUT`.
- Multi-architecture Docker image on a distroless static base, and a
  `docker-compose.yml` example.

[Unreleased]: https://github.com/DrSmithFr/http-debug/commits/main
