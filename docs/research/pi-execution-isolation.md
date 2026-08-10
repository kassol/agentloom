# Pi whole-process execution isolation

Status: research for [GitHub issue #30](https://github.com/kassol/agentloom/issues/30)
Date: 2026-08-10
Scope: currently supported macOS and Linux hosts; no production implementation

## Decision

Do **not** advertise a Pi Sandbox capability in production yet.

The smallest design worth prototyping is to run the **entire** `pi --mode rpc`
process inside an OpenShell sandbox, using a Loom-owned, version-pinned Pi image
and OpenShell's local gateway. This is the only investigated option that puts
Pi itself, built-in tools, extensions, arbitrary extension code, shell children,
filesystem access, network egress, and credential delivery under one documented
policy boundary on both macOS and Linux. Pi's own documentation recommends this
whole-process OpenShell pattern for filesystem, process, network, credential,
and inference controls. [Pi containerization](https://github.com/earendil-works/pi-mono/blob/main/packages/coding-agent/docs/containerization.md)
[OpenShell architecture](https://docs.nvidia.com/openshell/about/how-it-works)

This is a **prototype recommendation, not a production recommendation**.
OpenShell's official overview currently labels the software alpha and says not
to use it in production. Until the prototype passes the gates below and that
upstream status changes, Pi isolation should remain explicitly unsupported
rather than silently falling back to host execution. [OpenShell overview](https://docs.nvidia.com/openshell/about/overview)

Do not build a bespoke macOS/Linux sandbox layer in Loom. That would duplicate
container/VM lifecycle, egress policy, credential brokering, process controls,
and recovery while creating a new security-critical subsystem.

## Boundary and threat statement

CodexLoom is local-first and trusts the Owner and other same-UID host processes.
The proposed boundary is therefore **not** intended to defend against a
malicious Owner or an attacker who already controls the Owner account.

It protects the Owner's host from behavior originating inside an isolated Pi
Agent: model mistakes, prompt injection from repository content, untrusted build
scripts, unexpected extension behavior, and descendant processes. The protected
assets are:

- host files outside the declared Agent workspace;
- host credentials and provider secrets;
- host processes and local services not explicitly exposed to the sandbox;
- network destinations not required by the task; and
- host availability against unbounded child-process creation.

The declared workspace is intentionally available to the Agent according to the
chosen workspace mode. A read-write host mount is therefore authority to modify
that host path, not protection from modifications within it. Pi says the same:
bind-mounted workspaces remain directly mutable, and stronger protection
requires read-only mounts or copying data into and out of the sandbox.
[Pi security](https://github.com/earendil-works/pi-mono/blob/main/packages/coding-agent/docs/security.md)
[Docker bind mounts](https://docs.docker.com/engine/storage/bind-mounts/)

Approval and isolation remain separate concepts. Approval can stop a recognized
tool call before execution, but extensions are arbitrary TypeScript running with
Pi's process permissions and can create their own I/O paths. Only an OS/container/
VM boundary can cover all code and descendants. [Pi extension security](https://github.com/earendil-works/pi-mono/blob/main/packages/coding-agent/docs/extensions.md#extension-security)

## Facts about Pi and the current Loom integration

Pi has no built-in sandbox. Its built-in tools and extensions run with the Pi
process's permissions, and Pi explicitly says real isolation must come from an
OS, container, or virtualization boundary. [Pi security](https://github.com/earendil-works/pi-mono/blob/main/packages/coding-agent/docs/security.md#no-built-in-sandbox)

Pi's default model-facing tools are `read`, `write`, `edit`, and `bash`; it can
also enable `grep`, `find`, `ls`, and extension-provided tools. Tool allowlists
reduce functionality but are not an isolation boundary because extension code
itself still executes. [Pi README](https://github.com/earendil-works/pi-mono/blob/main/packages/coding-agent/README.md)

The current Loom integration:

- launches host `pi --mode rpc` with a per-Agent host session directory and a
  Loom extension ([Pi Runtime start](../../internal/hub/pi_agent_runtime.go#L111-L146));
- inherits the Hub's complete environment before adding Loom variables
  ([RPC environment](../../internal/pi/rpc.go#L69-L91)); and
- lets the extension call the host Loom HTTP API using `CODEX_LOOM_API_URL` and
  `CODEX_LOOM_AGENT_ID` ([Loom extension transport](../../internal/pi/loom_extension.ts#L40-L59)).

Consequently, merely wrapping `bash` does not isolate the Pi process, extensions,
other tools, inherited credentials, or extension-originated network calls.

## Options considered

| Option | Coverage | macOS + Linux | Result |
| --- | --- | --- | --- |
| Pi example sandbox extension (`@anthropic-ai/sandbox-runtime`) | Wraps `bash` and user shell commands; Pi and other tools/extensions remain on host | Example supports both | Reject as whole-process isolation. Useful only if named honestly as sandboxed shell execution. [Example source](https://github.com/earendil-works/pi-mono/blob/main/packages/coding-agent/examples/extensions/sandbox/index.ts) |
| Gondolin tool-routing extension | Routes built-in tools and `!` commands into a micro-VM; Pi and other extension code remain on host; workspace writes pass through | QEMU-based local path | Reject as whole-process isolation. Pi explicitly documents this limitation. [Pi containerization](https://github.com/earendil-works/pi-mono/blob/main/packages/coding-agent/docs/containerization.md#gondolin) |
| macOS App Sandbox / `sandbox-exec` | Native macOS app-entitlement boundary | No shared Linux path | Reject. `sandbox-exec` is deprecated, and App Sandbox is an entitlement/bundling model rather than a portable wrapper for the current Node CLI. [Apple App Sandbox](https://developer.apple.com/documentation/security/app-sandbox) [macOS `sandbox-exec(1)`](https://keith.github.io/xcode-man-pages/sandbox-exec.1.html) |
| Linux namespaces/bubblewrap directly | Can constrain filesystem/process/network on Linux | Linux only | Reject as the product abstraction. A second macOS implementation would diverge, and direct assembly would make Loom own security-sensitive policy composition. |
| Plain Docker/Podman whole-process container | Covers Pi, extensions, tools, and descendants; macOS runs Linux containers in a VM | Yes | Viable only for a reduced profile. Bind mounts are writable by default, raw provider credentials enter the container, and portable deny-by-default destination policy plus host control-plane access requires more infrastructure. [Pi Docker guidance](https://github.com/earendil-works/pi-mono/blob/main/packages/coding-agent/docs/containerization.md#plain-docker) [Docker security](https://docs.docker.com/engine/security/) [Podman machine](https://docs.podman.io/en/stable/markdown/podman-machine-start.1.html) |
| OpenShell whole-process sandbox | Pi, extensions, tools, children, filesystem, process, egress, and credential/inference controls | Official Docker, Podman, and MicroVM drivers cover current macOS/Linux | Best prototype candidate, but production no-go while upstream remains alpha. [Installation/support](https://docs.nvidia.com/openshell/about/installation) [Security controls](https://docs.nvidia.com/openshell/security/best-practices) |

Plain containers are still useful evidence for the process boundary. Docker uses
namespaces and cgroups, supports a read-only root filesystem, PID limits, and
capability reduction; rootless mode also runs the daemon and containers in a
user namespace. On macOS, Docker Desktop runs Linux containers in a Linux VM.
These controls do not by themselves solve destination-aware egress or keep model
credentials out of the Agent process. [Docker Engine security](https://docs.docker.com/engine/security/)
[Docker run options](https://docs.docker.com/reference/cli/docker/container/run)
[Docker rootless mode](https://docs.docker.com/engine/security/rootless/)
[Docker Desktop VM](https://docs.docker.com/desktop/features/vmm/)

## Recommended prototype contract

Prototype one opt-in execution profile, tentatively `isolated`, with no automatic
downgrade:

1. **Whole process.** The OpenShell supervisor launches `pi --mode rpc`; the
   Loom extension and every child execute inside the same sandbox.
2. **Pinned image.** Build a small Loom-owned image containing the exact supported
   Pi version and Loom extension dependencies. Do not depend on a floating
   community image: OpenShell's current supported-agent list does not list Pi,
   even though Pi's own guide shows an OpenShell example.
   [OpenShell supported agents](https://docs.nvidia.com/openshell/about/supported-agents)
3. **No host home mount.** Never mount `~/.pi`, `~/.ssh`, cloud config, Docker/
   Podman sockets, or the Loom data root. Pi also warns that mounting its host
   Agent directory exposes host auth and sessions.
4. **Copy workspace for the first prototype.** Upload into the sandbox workspace
   and download explicit results. OpenShell transfer commands restrict downloads
   to the writable workspace and preserve sandbox lifetime independently of the
   initial command. Host bind mounts are disabled by default because they can
   negate workspace isolation and filesystem policy.
   [Sandbox transfer/lifecycle](https://docs.nvidia.com/openshell/sandboxes/manage-sandboxes)
   [Compute-driver mounts](https://docs.nvidia.com/openshell/reference/sandbox-compute-drivers)
5. **Deny-by-default egress.** Permit only brokered inference and the narrow Loom
   control-plane channel. OpenShell applies network policy at its gateway proxy
   and denies unmatched destinations.
6. **Broker credentials.** Use OpenShell provider/inference routing so the Pi
   process receives placeholders rather than raw provider keys. Do not copy the
   Hub environment into the sandbox.
   [OpenShell provider injection](https://docs.nvidia.com/openshell/sandboxes/manage-providers)
   [Inference routing](https://docs.nvidia.com/openshell/sandboxes/inference-routing)
7. **Scoped Loom channel.** Do not expose the existing unrestricted local HTTP
   API to the sandbox. The sandbox is a newly reachable untrusted boundary, so
   use the smallest effective mitigation: a per-Agent capability token and a
   narrow relay that accepts only that Agent's Message/Topic/Needs You operations.
   This is scoped transport authorization, not a multi-user auth framework.
8. **Explicit state.** Report `isolated`, `starting`, `running`, `degraded`, or
   `unavailable` with the cause and active policy revision. Never report Sandbox
   from approval policy or from the mere presence of a container engine.

OpenShell documents four enforcement layers relevant to this contract: a
deny-by-default network proxy, Landlock filesystem policy, seccomp plus privilege
drop for processes, and gateway-routed inference that keeps raw keys outside the
sandbox. On macOS these Linux enforcement mechanisms run inside the selected
Docker Desktop VM or OpenShell MicroVM rather than the Darwin host kernel.
[OpenShell controls](https://docs.nvidia.com/openshell/security/best-practices)
[OpenShell support matrix](https://docs.nvidia.com/openshell/reference/support-matrix.html)

## RPC, lifecycle, and recovery gates

OpenShell can execute a command in a persistent sandbox, accept stdin, stream
output, propagate the remote exit code, and leave the sandbox running after the
initial command exits. Deleting a sandbox stops processes and purges injected
credentials. [Sandbox execution and lifecycle](https://docs.nvidia.com/openshell/sandboxes/manage-sandboxes)

That documentation is not enough to assume compatibility with Pi RPC's long-lived,
full-duplex LF-delimited JSON stream. The prototype must prove all of these before
any production ticket:

- sustained bidirectional `pi --mode rpc` traffic through the chosen OpenShell
  command path without a TTY or framing changes;
- event ordering and stderr separation identical to host Pi;
- interrupt and close terminate the complete Pi process tree, with no remaining
  tool process;
- Hub restart reconnects to the stable sandbox identity and resumes the same Pi
  session without duplicating the logical Agent or Turn;
- Pi process loss leaves the sandbox available for evidence inspection and safe
  resume;
- gateway/driver loss produces explicit `unavailable`, never host fallback;
- sandbox deletion is an explicit destructive Owner action after workspace/result
  export; and
- provider/model switching, images, approvals, Message/Topic/Needs You, and
  history pass the same runtime conformance stories as unrestricted Pi.

Measure cold creation, warm resume, filesystem throughput, and workspace transfer
on one supported macOS and one supported Linux host. No performance threshold can
be selected from source documentation alone.

## Prototype acceptance matrix

| Dimension | Required evidence |
| --- | --- |
| Built-in and extension tools | A custom extension and every Pi built-in demonstrate that all effects occur inside the sandbox. |
| Filesystem | Can modify the sandbox workspace; cannot read/write host home, Loom data, or sibling repositories. |
| Processes and children | PID/fork limits apply; interrupt, crash, and delete remove the full descendant tree. |
| Network | Unlisted destinations and local host services fail; approved Loom relay and inference succeed and are logged. |
| Credentials | Raw provider, GitHub, SSH, cloud, and Loom authority are absent from env/files/process arguments; placeholders cannot be reused for another destination. |
| Workspace | Copy-in/out preserves required Git content and makes the Owner review/apply boundary explicit. |
| Recovery | Hub, Pi, gateway, and host restart scenarios preserve or explicitly fail the same Agent/session binding. |
| UX | The Inspector explains active boundary, workspace mode, network policy, credential mode, and why isolation is unavailable. |

## No-go conditions

Keep Pi Sandbox bounded unsupported if any of the following remains true:

- OpenShell is still officially not recommended for production;
- Pi RPC cannot be supervised as a stable full-duplex child through the sandbox;
- Loom must expose its unrestricted local API or raw Owner credentials to the
  sandbox;
- recovery can silently create a new Pi session or lose the long-lived Agent's
  history; or
- macOS and Linux require product-visible semantics that cannot pass the same
  conformance stories.

## Follow-up tickets justified by this research

1. **Prototype Pi RPC inside OpenShell on macOS and Linux.** Build a pinned image
   and execute only the lifecycle/RPC/recovery acceptance gates above.
2. **Specify the isolated Runtime capability contract.** Replace the single
   Boolean's implied promise with an outcome that reports boundary, workspace,
   network, credential, and recovery support plus an unavailable reason.
3. **Prototype a scoped Loom relay for isolated Runtimes.** Limit it to the
   existing Agent's Loom control-plane operations; do not create general remote
   API authentication.
4. **Reassess production adoption when OpenShell leaves alpha.** Review current
   platform support, policy semantics, upgrade/recovery behavior, and upstream Pi
   image support at that time.

Do not open a production implementation ticket until the first prototype passes.
