# Claude Runtime generation

Claude Runtime support is a developer preview. This release supports exactly one
compatibility row: bridge protocol/build, Node 24.19.0, Claude Agent SDK 0.3.228,
bundled Claude Code 2.1.228, and the required bridge capabilities. It does not
search `PATH`, reuse a global Node installation, or silently install/update.

The Owner installs from Settings > System or with:

```sh
loom runtime claude status
loom runtime claude install --accept-terms
loom runtime claude verify --target staged
loom runtime claude activate
loom runtime claude rollback
```

Install downloads the pinned Node archive from nodejs.org, verifies its SHA-256,
runs `npm ci --ignore-scripts` against the embedded exact lock, checks installed
package versions, and runs the bridge's zero-model self-test. It does not read a
Claude credential or send a model request. A successful install is only staged;
activation is a separate explicit operation. Activation retains one compatible
previous generation for explicit rollback and never rolls back automatically.
The acknowledgement is tied to Anthropic's Commercial Terms effective
June 17, 2025; a future manifest revision requires a new acknowledgement.
This is an install-time acknowledgement only. Written legal acceptance for
production distribution/support is a separate release record and cannot be
inferred from installation state.

Generations live outside Agent workspaces and Loom backups in the platform data
directory (`~/Library/Application Support/CodexLoom/claude-runtime` on macOS;
`$XDG_DATA_HOME/codex-loom/claude-runtime` or
`~/.local/share/codex-loom/claude-runtime` on Linux). Public API/CLI/Web responses
contain the compatibility row and safe state only, never executable paths,
credentials, npm helper state, or account data.

Supported preview hosts are macOS 14+ and Ubuntu 22.04+ with glibc, on arm64 or
x64. Windows, musl Linux, and other platforms report `unsupported` with an
alternative and perform no download. Production readiness remains false until
the release's legal acceptance and four-platform real smoke gates are complete.
The exact result rows and missing-result behavior are defined in
[claude-runtime-certification.md](claude-runtime-certification.md).
