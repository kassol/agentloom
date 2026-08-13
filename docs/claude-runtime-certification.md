# Claude Runtime developer-preview certification

Claude Runtime remains a developer preview. `productionReady` is deliberately
`false`; deterministic certification and a successful local install do not
establish production support.

## Deterministic certification

The mandatory Runtime Contract runner executes Codex, Pi, Claude, and the
minimal fixture through the same Driver/Host/Contract causal story. Claude uses
the real Store-backed Canonical Turn Ledger and a supervised bridge process,
including Driver and Store reopen. Available capability descriptors are
exercised through their typed hooks; unknown available descriptors fail.
Goal is the one intentional ownership distinction: it is Loom-owned product
state and therefore does not require a Claude-native Goal hook.

The deterministic manifest in
`internal/hub/claude_certification_manifest_test.go` makes the required
cross-package evidence visible. It includes the mixed Codex + Pi + Claude
HTTP/SSE/Store-reopen story and focused adoption, Approval, Needs You, model,
resource, context, usage, Goal, archive, race, redaction, no-replay, and
process-tree tests.

Run the focused gates with:

```sh
go test ./internal/hub -run 'TestRuntimeContractConformanceCodexPiClaudeAndMinimalFake|TestClaudeDeterministicCertificationManifest|TestClaudeRuntimeManagedGenerationRealSmoke'
go test ./internal/httpapi -run TestCanonicalMixedRuntimeHTTPAndSSEExecuteCodexPiClaudeAcrossRestart
go test -race ./internal/hub ./internal/claudebridge
sh scripts/verify-release-gates.sh
```

The real smoke skips unless explicitly enabled. It uses the exact active
managed generation and only reads `CLAUDE_REAL_API_KEY` after
`CLAUDE_REAL_SMOKE=1`. Its single isolated Turn disables tools, permits no
writes, uses one model turn with a USD 0.05 ceiling, and verifies typed content,
usage, interrupt receipt and terminal, Canonical Ledger History after fresh
bridge and Store reopen, and process-group cleanup.

## Four-row preview result contract

The manually dispatched `Claude preview smoke` workflow defines these required
result rows:

| Result row | Minimum host | Workflow runner |
|---|---|---|
| `darwin-arm64` | macOS 14 arm64 | `macos-14-xlarge` |
| `darwin-x64` | macOS 14 x64 | `macos-14` |
| `linux-arm64` | Ubuntu 22.04+ glibc arm64 | `ubuntu-24.04-arm` |
| `linux-x64` | Ubuntu 22.04+ glibc x64 | `ubuntu-22.04` |

Each row uploads one JSON result for the exact generation and commit. Missing
credentials, an unavailable runner, a skipped smoke, a platform mismatch, or a
failed smoke is `missing`/`failed`, never a pass. The final result-contract job
requires four `passed` artifacts and also requires
`"productionReady":false`. If GitHub-hosted capacity cannot execute a row, an
operator must run the same script on a matching external host and retain its
typed result; absence of that result keeps production support gated.

For an external row:

```sh
export CLAUDE_PREVIEW_ANTHROPIC_API_KEY='<product-safe credential>'
export XDG_DATA_HOME='<isolated preview data directory>'
export CLAUDE_PREVIEW_INSTALL_EXACT_GENERATION=1
export CLAUDE_PREVIEW_ACCEPT_INSTALL_TERMS=1
sh scripts/run-claude-preview-smoke.sh linux arm64 results/linux-arm64.json
sh scripts/verify-claude-preview-results.sh results
```

Do not put the credential in arguments, artifacts, logs, or repository files.
Setting `CLAUDE_PREVIEW_ACCEPT_INSTALL_TERMS=1` is the explicit install-time
acknowledgement for the exact manifest; the script never enables it implicitly.
The row artifact contains only platform, generation, commit, status, and safe
reason fields.

## Separate legal gate

`loom runtime claude install --accept-terms` records the Owner's install-time
acknowledgement of the manifest's Anthropic terms revision. It is not written
legal acceptance for production distribution or support. Production readiness
requires a separately retained written legal acceptance record and four real,
passing platform results. Neither gate is set or claimed by this developer
preview certification.
