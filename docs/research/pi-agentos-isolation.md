# Pi whole-process isolation with agentOS

Status: research only; no production implementation

Date: 2026-08-12

Scope: whether [agentOS](https://agentos-sdk.dev/) can replace or unblock the
whole-process Pi isolation prototype in [GitHub issue #32](https://github.com/kassol/agentloom/issues/32)

## Decision

**Do not replace the current Pi launcher with agentOS or advertise Pi Sandbox.**
agentOS is a credible candidate for a separate, opt-in `pi-agentos` Runtime
prototype, but it is not a drop-in isolation wrapper around CodexLoom's current
`pi --mode rpc` Runtime and does not yet satisfy #32.

The attractive part is real: agentOS runs guest JavaScript in V8 isolates,
compiled tools in WebAssembly, and mediates guest files, processes, sockets,
environment access, and bindings through its own sidecar/kernel. Pi is available
as packaged guest software, AgentOS sessions are long-lived, and network access
is denied by default. This covers far more of the Pi execution surface than a
shell-only wrapper. [Architecture](https://agentos-sdk.dev/docs/architecture/)
[Security model](https://agentos-sdk.dev/docs/security-model/)

The blockers are equally material:

- agentOS explicitly says it is **beta and still undergoing security review**;
- its boundary is an in-process V8/WASM virtual kernel, not an OS, container, or
  microVM boundary, so the native sidecar/kernel and isolate implementation are
  the trusted computing base;
- its packaged Pi is currently `@earendil-works/pi-coding-agent` **0.80.6**, while
  CodexLoom has validated Pi **0.84.1**;
- its documented Pi path is AgentOS session/ACP, not Loom's existing raw Pi RPC
  lifecycle, event schema, extensions, and session files;
- arbitrary native binaries and toolchains such as Go, Rust, and C++ are not
  supported in the lightweight VM and require an external full sandbox; and
- the documented Pi setup injects provider keys into the session environment,
  while persisted session environment and credential values may be stored as
  trusted plaintext.

[AgentOS security warning](https://agentos-sdk.dev/docs/security-model/)
[Limitations](https://agentos-sdk.dev/docs/limitations/)
[Persistence](https://agentos-sdk.dev/docs/persistence/)
[Pinned Pi package source](https://github.com/rivet-dev/agentos/blob/44a6c8022f26ef0edd56e3d224f8296ee6387232/software/pi/package.json)

## What agentOS is and where it runs

agentOS is an Apache-2.0 TypeScript/Node library plus a native sidecar. The host
application is the trusted client; the sidecar owns a virtual Linux kernel,
filesystem, process and socket tables, policy, pipes, and PTYs; guest JavaScript
runs in V8 isolates and compiled guest commands run as WebAssembly. Many VMs may
share one sidecar process, but each actor has its own virtual state. It runs on
the same machine and inside the backend deployment rather than provisioning one
host OS VM per Agent. [README at evaluated commit](https://github.com/rivet-dev/agentos/blob/44a6c8022f26ef0edd56e3d224f8296ee6387232/README.md)
[Security model source](https://github.com/rivet-dev/agentos/blob/44a6c8022f26ef0edd56e3d224f8296ee6387232/docs/content/docs/security-model.mdx)

This matters for CodexLoom: the Go Hub cannot simply prefix its existing Pi
command. It would need to supervise a Node/AgentOS host, translate Loom's Runtime
Contract to AgentOS sessions/ACP, package the Loom extension into the guest, and
reconcile two durable control planes. That is a new Runtime adapter, not a small
launcher change.

Prebuilt sidecars exist for macOS x64/arm64 and glibc Linux x64/arm64. Windows
and Linux musl are not listed by the resolver. The guest is a virtual Linux
environment on both supported host families.
[Sidecar resolver](https://github.com/rivet-dev/agentos/blob/44a6c8022f26ef0edd56e3d224f8296ee6387232/packages/sidecar-binary/index.js)
[Runtime sidecar platforms](https://github.com/rivet-dev/agentos/blob/44a6c8022f26ef0edd56e3d224f8296ee6387232/packages/runtime-sidecar/README.md)

No Rivet Cloud account is inherently required: the library and platform are
open source and documented for self-hosting. Rivet Actors add durable state,
scheduling, and orchestration; Cloud is an optional managed deployment path.
[Official deployment overview](https://agentos-sdk.dev/)

## Can it host the complete Pi process?

### What is supported

agentOS has two relevant primitives:

1. guest processes can be spawned long-term, receive stdin, stream stdout and
   stderr, be signalled, inspected, stopped, killed, and waited on; and
2. `openSession` can keep a packaged coding agent alive across prompts and
   expose session events, prompting, cancellation, permission requests, and
   durable completed history.

[Process documentation](https://agentos-sdk.dev/docs/processes/)
[Session architecture](https://agentos-sdk.dev/docs/architecture/agent-sessions/)

The repository ships a real Pi package and an ACP entrypoint. The evaluated
package bundles Pi and `pi-acp`; its manifest launches `pi-acp`, which in turn
drives the packaged Pi command inside the guest VM.
[Pi manifest](https://github.com/rivet-dev/agentos/blob/44a6c8022f26ef0edd56e3d224f8296ee6387232/software/pi/agentos-package.json)
[Pi ACP build](https://github.com/rivet-dev/agentos/blob/44a6c8022f26ef0edd56e3d224f8296ee6387232/software/pi/scripts/build-pi-acp.mjs)

Therefore, **a complete packaged Pi process, its JavaScript extensions, its
virtual child processes, and supported tools can plausibly live behind one
AgentOS VM boundary**. This is strong enough to justify a prototype.

### What is not established

It does not establish that CodexLoom can place its existing arbitrary host Pi
0.84.1 RPC command behind that boundary unchanged:

- the current package pins Pi 0.80.6, not Loom's supported 0.84.1;
- the public integration is AgentOS sessions over ACP, while Loom consumes Pi's
  LF-JSONL RPC and native session evidence directly;
- only packaged V8/WASM/Pyodide-compatible software runs; arbitrary downloaded
  native executables and `apt`/`yum` are unavailable; and
- AgentOS sleep destroys the live adapter, processes, shells, subscriptions,
  and in-progress deltas, then restores or recreates the adapter later.

The implementation must not call this the existing Pi Runtime with isolation.
It changes the execution substrate and externally observable lifecycle.

## Isolation properties

| Property | AgentOS documented behavior | CodexLoom judgment |
| --- | --- | --- |
| Pi and extension code | Packaged Pi and extension JavaScript execute as guest code inside V8/WASM VM mediation. | Promising whole-guest boundary, subject to package/version parity and beta security review. |
| Files | Per-VM VFS does not expose host files by default. Explicit host mounts are root-confined and read-only unless made writable. | Can protect home, Loom data, and sibling repos. Real project maintenance needs a deliberately writable workspace mount or copy/apply workflow. |
| Network | Guest sockets and DNS traverse the virtual socket table; egress is denied by default and can be allowed per host. | Good policy shape. Must attack-test DNS, redirects, IP literals, loopback, local services, and every Pi/extension network path. |
| Environment and credentials | Host environment is not inherited. Explicit session `env` is guest-visible; the Pi example passes provider API keys this way. | Better than inheriting the Hub environment, but fails #32's raw-credential absence requirement as documented. |
| Child processes | Guest children are virtual processes and can run only mounted V8/WASM/Python commands; they cannot spawn host processes. | Strong on paper for supported commands. It deliberately cannot run arbitrary native project toolchains. |
| Host functions | Named bindings cross to trusted host code and return only JSON results. | Suitable mechanism for a scoped Loom relay, but every binding expands authority and must be Agent-scoped. |
| Resource control | VM policies and limits cover CPU, memory, payload, timing, process and filesystem operations. | Must be measured under hostile fork/memory/output cases; docs are not acceptance evidence. |
| Blast radius | Each actor has isolated VM state, while multiple actors can share the sidecar. | Guest-to-guest isolation is claimed, but a sidecar defect can affect the host process; beta review is a production no-go. |

[Filesystem](https://agentos-sdk.dev/docs/filesystem/)
[Networking](https://agentos-sdk.dev/docs/networking/)
[Permissions](https://agentos-sdk.dev/docs/permissions/)
[Models and credentials](https://agentos-sdk.dev/docs/models-and-credentials/)

### Credential caveat

The marketing claim that bindings can keep secrets on the host is valid for
host functions, but the official Pi guide currently tells callers to pass
`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and similar keys in session `env`.
AgentOS also warns that session environment values, MCP credentials, prompts,
messages, and tool/permission payloads may be persisted as plaintext. A
production candidate needs a Pi-compatible inference gateway or host binding
that never puts reusable provider secrets into guest environment, files,
arguments, or persistent SQLite.
[Pi credentials](https://agentos-sdk.dev/docs/agents/pi/)
[Persistence warning](https://agentos-sdk.dev/docs/persistence/)

## Persistence, recovery, and fail-closed behavior

AgentOS persists `/home/agentos`, session metadata, and completed ACP history in
actor SQLite. It does **not** preserve running processes, live adapter state,
shells, subscriptions, in-progress message deltas, or in-memory mounts. On wake,
it prefers native ACP session restore when supported; otherwise it starts a new
adapter session with bounded AgentOS history as context.
[Persistence](https://agentos-sdk.dev/docs/persistence/)

That is useful durability but weaker than #32's required proof that the same
Loom Agent, Loom Thread, Pi Session, and history survive Hub, Pi, gateway,
sandbox, and host restarts. Recreating an adapter with summarized/bounded
history is not the same native Pi session. Loom would need an explicit mapping,
persisted private references, turn fences, duplicate-prevention tests, and an
`unavailable` outcome when exact restoration is impossible.

Within the VM, missing commands and denied syscalls fail rather than falling
through to host processes. However, no AgentOS feature can prove that a future
Loom launcher will never fall back to unrestricted host Pi. The adapter itself
must omit that fallback and test it with a host sentinel, as required by #32.

## #32 acceptance assessment

| #32 acceptance criterion | Current assessment | Reason |
| --- | --- | --- |
| Version-pinned complete Pi, Loom extension, tools, custom code, descendants in one sandbox | **Partial** | AgentOS packages a complete Pi guest, but at 0.80.6; Loom's 0.84.1 extension and exact built-in/custom paths have not run there. |
| Sustained full-duplex LF-JSONL, ordering, stderr, interrupt, close, process-tree termination | **Blocked** | AgentOS demonstrates process/session streaming through ACP, not Loom's exact long-lived Pi RPC contract and failure matrix. |
| No host home/Loom/sibling/local-service/raw-credential/unlisted-egress access | **Partial / credential no-go** | VFS and deny-default network are promising. Official Pi setup places raw provider keys in guest session env and may persist them plaintext. |
| Minimal scoped Loom relay for Message, Topic, Needs You | **Blocked** | Bindings are a suitable primitive, but no Go↔Node Agent-scoped relay or Loom extension integration exists. |
| Hub/Pi/sidecar/VM/host restart preserves identity/history or fails explicitly | **Blocked** | Completed history persists, but live processes and in-flight deltas do not; adapter recreation may use bounded history instead of the same native Pi session. |
| Model switching, images, Approval, collaboration, history conformance | **Blocked** | AgentOS exposes ACP sessions and approvals, but none of Loom's Runtime Contract stories have been run against it. |
| macOS and Linux measurements and no-go record | **Partial** | Published sidecars cover both OS families on x64/arm64, but no Loom prototype or hostile-boundary measurements exist. |
| Never host fallback; remain prototype-only | **Unproven** | VM syscalls do not fall through, but Loom launcher failure behavior does not exist yet. Beta status independently prevents a production claim. |

Result: AgentOS does **not** close or unblock #32 today.

## Fit for real CodexLoom project maintenance

The lightweight AgentOS VM is not a general replacement for the host Pi used by
CodexLoom developers. Its registry includes Git and common POSIX tools, but its
official limitations require an external full sandbox for arbitrary native
binaries, Go/Rust/C++ toolchains, Docker, heavy compilation, file watching, and
native-extension npm packages. CodexLoom itself is a Go + Web project, so a Pi
Agent tasked with building and maintaining Loom would cross this boundary
immediately. [Limitations](https://agentos-sdk.dev/docs/limitations/)
[AgentOS versus full sandbox](https://agentos-sdk.dev/docs/versus-sandbox/)

AgentOS can mount an external sandbox and expose its commands through bindings,
but then the complete execution boundary is the external provider (Docker,
E2B, Daytona, Vercel, Cloudflare, and others), not AgentOS alone. That path adds
another lifecycle, filesystem, credential, and recovery layer and does not
simplify #32. [External sandboxes](https://agentos-sdk.dev/docs/sandboxes/)

## Smallest worthwhile prototype

Prototype a **new `pi-agentos` Runtime adapter**, not a flag on the current Pi
Runtime. Do not add dependencies to the production Hub until this standalone
probe passes.

1. Pin AgentOS, the sidecar binary, Pi 0.84.1 source/package, pi-acp, and the
   exact Loom extension. Reject any version drift before launch.
2. Run one AgentOS VM per Loom Agent with no host home or Loom data mount,
   deny-default network, explicit CPU/memory/process/output limits, and a copied
   test workspace. Do not begin with a writable host repository.
3. Drive Pi through the AgentOS session API and separately probe raw packaged
   `pi --mode rpc`; record which route can preserve Loom's event, interrupt,
   usage, context evidence, compaction, and session semantics without invention.
4. Replace provider-key session env with a non-reusable, destination-scoped
   inference path. Stop the prototype if raw reusable credentials remain visible
   to Pi or persisted state.
5. Expose only Message, Topic, and Needs You through three Agent-scoped host
   bindings. Do not expose Loom's unrestricted local API.
6. Attack filesystem escapes, network redirects/DNS/local services, environment
   reads, extension-originated I/O, process exhaustion, cancellation, sidecar
   crash, Hub restart, host restart, and same-session recovery on macOS arm64 and
   glibc Linux.
7. Prove fail-closed behavior with a sentinel host Pi. Any AgentOS startup,
   restore, package, or policy failure must return structured `unavailable` and
   leave the sentinel untouched.

The prototype is a go only if all #32 gates pass and AgentOS's upstream security
status no longer makes production use a no-go. Performance claims and ordinary
happy-path Pi prompts are not isolation evidence.

## Risks and no-go conditions

Keep production Pi Sandbox unsupported while any of these remains true:

- AgentOS remains beta or its security review is incomplete;
- the exact supported Pi and Loom extension cannot be pinned and run;
- provider or Loom credentials are reusable from guest env/files/arguments or
  persisted plaintext;
- real project work requires an external sandbox whose policy and recovery are
  not included in the same acceptance boundary;
- AgentOS adapter recreation cannot preserve or explicitly reject Loom's native
  session identity and in-flight state;
- the Node sidecar becomes a second durable authority for facts Loom already
  owns, or failures can commit contradictory state; or
- macOS and Linux do not pass the same hostile-boundary and conformance suite.

## Recommendation

- **Existing Pi Runtime:** do not wrap or replace it with AgentOS now.
- **Separate `pi-agentos` Runtime:** recommend a small, standalone prototype when
  there is appetite to maintain an ACP/Node adapter and accept a restricted
  virtual toolchain.
- **#32 production isolation:** remain open and unsupported. AgentOS is stronger
  than `just-bash`, but it neither satisfies the current acceptance matrix nor
  removes the need for a full external sandbox for general native project work.

This assessment installed no dependency, changed no Runtime code, and did not
alter the OpenShell prototype or issue state.
