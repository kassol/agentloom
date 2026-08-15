# Native Windows Support

CodexLoom supports local Hub, CLI, embedded WebUI, backup, and Runtime Host
interfaces on 64-bit Windows. This page owns the exact Windows build and
operations contract.

## Supported system and toolchain

- Windows 11 22H2 or newer on x86-64.
- PowerShell 7 or Windows PowerShell 5.1.
- Go 1.25, as pinned by `go.mod`.
- Node.js 22 and pnpm 11.21.0, pinned by `web/package.json`.
- Git for Windows.
- `codex` in `PATH`, plus Pi 0.84.1 or newer in `PATH` (or `PI_BIN`).

GitHub Actions uses a real `windows-latest` runner to build the WebUI and all
Windows binaries, execute Windows-native core tests, and compile every Go
package and test binary. Windows Server and ARM64 are not currently supported
production targets.

## Install and build

From a PowerShell prompt:

```powershell
git clone https://github.com/kassol/agentloom.git
Set-Location agentloom
.\scripts\build.ps1
```

The PowerShell script is the canonical Windows production build. It performs a
frozen pnpm install and the WebUI build first, builds the Hub, reloader, CLI,
and gateway binaries second, and verifies that `bin\codex-loom.exe` contains
the current Vite entrypoint. Do not publish a bare `go build` result because it
may embed a stale WebUI.

Stop a running Hub before replacing its executable. Windows locks a running
`.exe`, so an in-place build over the active binary can fail.

## Start, stop, and restart

Start in the foreground and open the registered browser:

```powershell
.\bin\codex-loom.exe -open
```

The WebUI is at <http://localhost:4870>. The default data directory is
`$env:LOCALAPPDATA\CodexLoom`. Set `CODEX_LOOM_DATA` or pass `-data` to use a
different local fixed-disk directory.

Press `Ctrl+C` in the Hub console to stop gracefully. CodexLoom checkpoints
active Runtime state, closes Runtime hosts, and releases the single-writer
Store lease before exiting.

Use the WebUI Restart action or:

```powershell
Invoke-RestMethod -Method Post -Uri http://localhost:4870/api/admin/restart
```

Restart first drains active work and creates a `pre-restart` backup. The
Windows reloader requests shutdown through a PID-scoped native Windows event,
waits for graceful exit, forcibly terminates only after the configured timeout,
and then starts the replacement process. It does not use Unix signals.

CodexLoom does not install a Windows Service. Foreground console operation is
the supported lifecycle for this release.

## Backup and restore

Create and inspect backups while the Hub is running:

```powershell
.\bin\loom.exe backup --reason before-upgrade
.\bin\loom.exe backups
```

Snapshots are under `$env:LOCALAPPDATA\CodexLoom\backups` unless the data
directory was overridden. They are portable `tar.gz` files.

There is no automated restore command. For a restore drill:

```powershell
$archive = Get-ChildItem "$env:LOCALAPPDATA\CodexLoom\backups\*.tar.gz" |
  Sort-Object LastWriteTime -Descending |
  Select-Object -First 1
$drill = Join-Path $env:TEMP "codexloom-restore-drill"
New-Item -ItemType Directory -Force $drill | Out-Null
tar -xzf $archive.FullName -C $drill
Get-Content (Join-Path $drill "manifest.json")
.\bin\loom.exe dev canary start --from (Join-Path $drill "codex-loom") --port auto
.\bin\loom.exe dev canary status
.\bin\loom.exe dev canary stop
```

For a production restore, stop the Hub, preserve the current data directory,
copy the extracted `codex-loom` contents into the configured data directory,
restore `codex-sessions` into `$HOME\.codex\sessions`, then start the Hub and
run `.\bin\loom.exe doctor`. Production restore remains an explicit Owner
operation because external delivery state cannot be safely replayed from an
older snapshot.

## Rollback

1. Create a backup and stop the Hub with `Ctrl+C`.
2. Preserve the current `bin` directory and data directory.
3. Restore the complete previous `bin` directory; Hub, CLI, reloader, and
   embedded WebUI must come from the same build.
4. If the release changed persisted state, restore the matching pre-release
   snapshot as described above.
5. Start `codex-loom.exe`, then run:

   ```powershell
   .\bin\loom.exe doctor
   .\bin\loom.exe version --running
   .\bin\loom.exe agent list
   ```

## Current limitations

- Managed Feishu/Lark, Slack, and Parall gateways are unsupported on Windows.
  Their install, retirement, and typed service-recovery paths return an explicit
  unsupported-platform error; CodexLoom does not pretend to register them with
  launchd or systemd. Gateway binaries compile, but Windows gateway operations
  are not an accepted product path.
- The managed Claude Runtime generation developer preview remains unsupported
  on Windows and reports that state through the Runtime status API/CLI.
- Windows Service installation, boot-time start, tray integration, and ARM64
  packages are not implemented.
- File permission bits do not describe Windows ACLs. Secret imports accept only
  bounded, regular, non-symlink files; prefer the normal Windows Credential
  Manager-backed integration flow.

## Troubleshooting

- If startup reports that the Store is already owned, stop the other Hub
  process and retry. Do not delete Store lock files while a Hub is running.
- Reloader diagnostics are in
  `$env:TEMP\codex-loom-reloader.log`; restarted Hub output is in
  `$env:TEMP\codex-loom.log`.
- If an `/api/...` URL returns HTML, rebuild with `.\scripts\build.ps1`; the
  running executable contains stale embedded WebUI assets.
- If browser opening fails, open <http://localhost:4870> manually.
- If `codex`, `pi`, `node`, or `npm` is not found, verify the command in the
  same PowerShell session and restart the shell after changing `PATH`.

## Acceptance evidence

CI proves Windows compilation, native event-based shutdown signaling, data
directory helpers, backup code, CLI/Hub/reloader builds, WebUI reproducibility,
and selected core tests on a Windows runner. It does not prove a human desktop
workflow. Release acceptance still requires one manual Windows 11 E2E covering
first start, browser launch, Agent creation and one Runtime turn, backup and
restore drill, graceful stop, WebUI restart, and rollback.
