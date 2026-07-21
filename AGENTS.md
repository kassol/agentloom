# Repository Instructions

## Production build and restart

CodexLoom embeds `internal/webui/dist` into the Go server binary at compile
time. A process restart does not reread files from that directory.

- Use `make build` (or the compatibility alias `make release`) before a
  production restart. The target always builds the WebUI first, compiles the
  binaries second, and verifies that `bin/codex-loom` contains the current Vite
  entrypoint.
- Do not publish or restart a production binary created with a bare
  `go build ./cmd/codex-loom`; it may embed stale frontend assets.
- After restart, verify `GET /api/version`: `build.webAsset` must match the
  module entrypoint in `internal/webui/dist/index.html`.
- When a frontend feature adds an API, verify that API returns JSON after the
  restart. An HTML response from an `/api/...` URL means the running binary
  does not contain that route and the SPA fallback handled the request.
