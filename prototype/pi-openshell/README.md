# Pi whole-process OpenShell prototype

Status: blocked environment probe, not a production Runtime capability

Issue: [#32](https://github.com/kassol/agentloom/issues/32)

Date: 2026-08-10

This directory contains only fail-closed prototype artifacts. Nothing here is
wired into the Hub, advertised as Pi Sandbox, or allowed to fall back to the
host Pi process.

## Artifacts

- `Dockerfile` pins both the Linux base image digest and Pi `0.84.1`. It copies
  the real Loom extension plus a probe extension into the same image as Pi.
- `probe_extension.ts` leaves an in-container load marker and writes its
  diagnostic sentinel to stderr.
- `policy.yaml` grants writes only to `/sandbox` and `/tmp`, does not mount a
  host home or workspace, and has no network destinations. It intentionally
  cannot reach the current unrestricted Loom HTTP API.
- `cmd/prereq` checks the OpenShell executable and gateway and emits a JSON
  result. It reports the prototype fallback path as `not_implemented` and
  OpenShell isolation evidence as `unverified`; it does not claim to prove the
  behavior of a future Runtime launcher.
- Go tests exercise unavailable prerequisite reporting and an opt-in real-image
  Pi RPC baseline.

## Reproduce

The safe prerequisite check does not install or start OpenShell:

```sh
go run ./prototype/pi-openshell/cmd/prereq
```

The ordinary test suite does not build a container:

```sh
go test ./prototype/pi-openshell/... -count=1
```

The opt-in baseline requires a working Docker daemon. It builds a disposable
tag and removes that tag after the test:

```sh
CODEX_LOOM_RUN_OPEN_SHELL_IMAGE_PROBE=1 \
  go test ./prototype/pi-openshell \
  -run TestPinnedImageLoadsPiAndExtensionsWithOrderedRPC -count=1 -v
```

## Evidence from this macOS host

Host: macOS 26.6.1 arm64. Docker 29.4.0 is available through OrbStack and its
Linux arm64 daemon. OpenShell is not installed, no OpenShell gateway is
running, and Homebrew has no `openshell` formula on this host. No install was
attempted because that would mutate the host beyond this safe prototype.

The prerequisite command returned:

```json
{
  "state": "unavailable",
  "reason": "openshell executable was not found",
  "hostOS": "darwin",
  "hostArch": "arm64",
  "hostFallback": "not_implemented",
  "isolationEvidence": "unverified"
}
```

The real-image baseline passed in 152.83 seconds on a cold build. It proved:

- the pinned image runs Pi `0.84.1` as the non-root `sandbox` user;
- the real Loom extension and custom probe extension load in that Pi process;
- two LF-delimited `get_state` requests return valid JSON responses in order;
- the extension diagnostic remains on stderr, never contaminating RPC stdout;
  and
- the probe extension's file effect occurs at `/sandbox` inside the disposable
  container.

This is a plain-container packaging and RPC baseline, not OpenShell isolation
evidence.

## Acceptance matrix

| Gate | Result | Evidence or blocker |
| --- | --- | --- |
| Pinned whole-process image | Partial | Pi and both extensions run in one pinned, non-root container; built-in/custom descendant isolation requires OpenShell. |
| Full-duplex RPC lifecycle | Partial | Ordered LF-JSONL and stderr separation pass; sustained traffic, interrupt, close, and descendant termination are unmeasured. |
| Filesystem/process/network/credentials | Blocked | Deny-default policy is authored but cannot be applied or attacked without an OpenShell gateway. No host data or credentials were supplied to the baseline. |
| Scoped Loom relay | Blocked | The existing extension targets the unrestricted local API, which is intentionally unreachable. Building a per-Agent relay is required before allowing any Loom destination. |
| Restart and identity recovery | Blocked | No sandbox identity exists to restart or reconnect. |
| Pi conformance stories | Blocked | Model switching, images, Approval, collaboration, and history cannot be exercised through the missing sandbox transport. |
| macOS and Linux measurements | Blocked | Only a macOS packaging baseline exists; no OpenShell macOS measurement and no Linux host measurement exist. |
| No host fallback | Blocked | The prerequisite probe itself has no Runtime launch or fallback path. A real OpenShell launcher failure test is required to prove future Runtime behavior. |

## Recommendation

Issue #32 is a no-go in this environment: keep Pi isolation explicitly
unsupported. Do not expose the current Loom API, copy provider credentials, or
use the host Pi as a fallback. A later continuation needs a deliberately
provisioned OpenShell gateway on one supported macOS host and one supported
Linux host, followed by the lifecycle, hostile-boundary, scoped-relay,
recovery, and conformance probes above. OpenShell's upstream alpha status also
remains an independent production no-go condition.
