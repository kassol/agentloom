# Runtime Contract v2

CodexLoom executes every Agent through the versioned Runtime Contract in
`internal/runtimecontract`. The Hub owns Loom identity, Turn state, approvals,
public history, and canonical events. An adapter owns its native protocol and
must not expose native maps, session paths, credentials, or protocol IDs across
that boundary.

## Ownership and lifecycle

A registered `RuntimeHostDriver` owns Runtime process or client supervision:

- `Preflight` proves the configured Runtime can be used without changing Agent
  state.
- `Acquire` returns one `AgentHost` for one Agent. The Hub performs this call
  without holding its global mutex, then revalidates the Agent binding before
  installing the result.
- `Shutdown` stops resources owned by the Driver and is idempotent.

An `AgentHost` owns one Agent handle. `Close` is idempotent and releases that
handle; for a shared Codex host it must not stop handles belonging to other
Agents. Its Contract must report the current `runtimecontract.Version` before
the Hub may use or persist a result.

The mandatory Contract is deliberately small and Loom-neutral. Create and
Start/Continue succeed only with `accepted`; Resume and Close only with
`completed`; Interrupt only with `interrupted`. Create/Resume bindings must
have schema v2, the selected Driver kind, and a non-empty native reference.
Start must provide a native Turn reference. A wrong success state, malformed
binding, invalid content/event/history, or malformed typed failure fails before
Loom state changes.

`Failure` always has a known phase, non-empty code, and safe public message.
Only a rejected `binding_resume` with `native_binding_not_found` permits the Hub
to create a replacement binding. Indeterminate results and message-text
sentinels are never retried.

## Events, correlation, and history

Each started Loom Turn has one stable Loom Turn ID. Native Turn references stay
inside the adapter/binding registry. A Contract event stream is ordered:

1. one `turn_started`;
2. zero or more typed `content` and `usage` events;
3. exactly one `terminal`, with no later event for that Turn.

History returns typed content and usage for the same correlation. An adopted
native Turn may initially have only a private Runtime reference; the public
projector assigns a stable Loom ID before it reaches HTTP, SSE, CLI, or Web.
Recovery markers settle unfinished tools as interrupted and never synthesize a
successful tool result.

Ordinary HTTP and SSE use canonical `loom/runtime-event` data and managed
artifact references only. Native evidence is available solely through the
explicit, redacted Runtime diagnostic endpoints. Historical native-shaped rows
in post-fork Stores are tombstoned during canonical replay; they are not a
compatibility stream.

## Optional capabilities

Runtime controls do not expand the mandatory request types. Sandbox, Provider,
Model, effort, Runtime-native resource inventory/policy, approval policy, input validation, Goal, Usage,
model catalog, compaction, rename/archive, and interruption inspection use
narrow typed optional hooks. A `CapabilitySnapshot` may say `available` only
when the corresponding hook is present and passes its capability-specific
exercise. Unknown or malformed descriptors fail closed.

`ContextMaintenanceCapability` is the `manual_compaction` hook. Its passive
`InspectContextMaintenance` reports an opaque revision, while `MaintainContext`
returns a typed terminal `Outcome`. Loom persists the Owner operation before
calling the Runtime and persists its terminal result before emitting the single
canonical `loom/context-maintenance` event. Restart recovery never repeats the
mutation: changed passive evidence proves completion and ambiguous evidence is
recorded as `indeterminate`. Runtime-native IDs, paths, and compaction summaries
remain adapter-private.

`ModelControlCapability` is the single per-Agent model surface. Its typed
state carries the available catalog, active model, per-model thinking choices,
and image-input truth; `SelectModel` applies one provider/model/thinking
selection. The Hub validates the pending selection against a fresh state,
revalidates the binding and configuration before persistence, and restores the
previous Runtime selection if persistence loses the race or fails.
`InputCapability` independently checks the committed active model again at
Turn start. Inspector previews may therefore show a pending model while the
composer continues to follow the active Capability Snapshot until Save
succeeds.

`UsageInspectionCapability` is passive: it reads one binding without acquiring
or starting its Host. Every scalar in the report carries availability and a
source. Aggregates may sum only observed values and must retain partial metric
coverage; unavailable Provider, cost, cache, reasoning, or call counts are not
zero and must not be inferred. Native usage may count consumed work outside the
visible branch, while `ReadHistory` keeps its own active-history semantics.
Pi preserves its persisted per-event Provider and native `totalTokens`; older
records without that total fall back to input + output + cache read + cache
write. Every user entry starts Activity, and only a terminal native stop reason
ends it, so tool-use and interrupted append tails remain open across restart.

`ContextEvidenceCapability` is the single passive context-inspection surface.
The Hub captures one Agent binding and its Loom-to-Runtime Turn correlation,
performs adapter I/O without its global mutex, then revalidates both before
publishing evidence. Codex keeps epoch, coverage, replay, and resend evidence.
Pi reads the exact historical user entry by its private Runtime Turn reference,
including entries outside the active branch, and reports only the exact Loom
blocks and source revisions it can prove. Missing evidence is `unknown`; an
unreadable or malformed native record is `unavailable`. Native references and
paths never cross the capability result.

Codex Provider definitions, credentials, verification, and shared Host restart
remain a Codex Host integration. They are not part of
`ModelControlCapability`, and Pi does not implement that administration
surface.

Approval proposals cross the Contract as a Loom-neutral tool/action summary
with a typed decision. Native RPC IDs, raw request JSON, client handles, and
wire response vocabulary remain in the adapter's private pending-response
table. Context replay evidence likewise uses a Runtime-neutral query/result;
only the Codex adapter translates it to rollout records. Provider-history
sanitization and restore are Codex Host maintenance capabilities rather than
shared Hub calls into rollout storage.

Codex Remote remains a shared Codex Host integration, not an Agent Runtime
capability. An Agent descriptor that marks Remote unavailable must not cause
the Web or Hub to route through a hidden per-Agent fallback.

## Certification

The shared conformance runner executes the same causal story for Codex, Pi,
and a minimal Driver/Host/Contract fixture: create, resume, configuration hook
effects, start, ordered typed stream, history, continue on the same binding and
Turn, interrupt, typed failure phases, close ownership, Driver shutdown, Store
reopen, identity-preserving acquire, and idempotent close/shutdown. Every
advertised capability is exercised; an unrecognized available capability is a
test failure.

Deterministic fake protocol processes cover ordering, steering, interruption,
failure, and recovery in CI. The opt-in `CODEX_REAL_BIN` and `PI_REAL_BIN`
smokes prove that the supported official binaries can traverse the same
Driver/Contract boundary and preserve binding and canonical history across a
real process restart. They complement rather than replace the shared suite.
