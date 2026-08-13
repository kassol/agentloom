[CmdletBinding()]
param(
    [string]$Version = "0.1.0-dev",
    [switch]$SkipWeb
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$originalLocation = Get-Location

function Invoke-Checked {
    param(
        [Parameter(Mandatory = $true)][string]$Command,
        [Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments
    )
    & $Command @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$Command exited with code $LASTEXITCODE"
    }
}

try {
    Set-Location $repoRoot

    if (-not $SkipWeb) {
        Invoke-Checked pnpm --dir web install --frozen-lockfile
        Invoke-Checked pnpm --dir web run build
    }

    $commit = (& git rev-parse --short=12 HEAD).Trim()
    if ($LASTEXITCODE -ne 0) {
        $commit = "unknown"
    }
    if ((& git status --porcelain).Count -gt 0) {
        $commit += "-dirty"
    }
    $builtAt = [DateTime]::UtcNow.ToString("yyyy-MM-ddTHH:mm:ssZ")
    $module = "github.com/yan5xu/codex-loom/internal/buildinfo"
    $ldflags = "-X $module.Version=$Version -X $module.Commit=$commit -X $module.BuiltAt=$builtAt"

    New-Item -ItemType Directory -Force "bin" | Out-Null
    $binaries = @(
        [pscustomobject]@{ Name = "codex-loom.exe"; Package = "./cmd/codex-loom" }
        [pscustomobject]@{ Name = "codex-loom-reloader.exe"; Package = "./cmd/codex-loom-reloader" }
        [pscustomobject]@{ Name = "loom.exe"; Package = "./cmd/loom" }
        [pscustomobject]@{ Name = "loom-gateway.exe"; Package = "./cmd/loom-gateway" }
        [pscustomobject]@{ Name = "loom-feishu-gateway.exe"; Package = "./cmd/loom-feishu-gateway" }
        [pscustomobject]@{ Name = "loom-slack-gateway.exe"; Package = "./cmd/loom-slack-gateway" }
        [pscustomobject]@{ Name = "loom-parall-gateway.exe"; Package = "./cmd/loom-parall-gateway" }
    )
    foreach ($binary in $binaries) {
        Invoke-Checked go build -ldflags $ldflags -o (Join-Path "bin" $binary.Name) $binary.Package
    }

    Copy-Item -Force "bin/codex-loom.exe" "bin/codex-hub.exe"
    Copy-Item -Force "bin/codex-loom-reloader.exe" "bin/codex-hub-reloader.exe"
    Copy-Item -Force "bin/loom.exe" "bin/chub.exe"
    Copy-Item -Force "bin/loom-gateway.exe" "bin/chub-gateway.exe"

    $index = Get-Content -Raw "internal/webui/dist/index.html"
    $match = [regex]::Match($index, 'src="/([^"?]+\.js)')
    if (-not $match.Success) {
        throw "cannot identify WebUI entrypoint"
    }
    $asset = $match.Groups[1].Value
    $binaryText = [Text.Encoding]::UTF8.GetString([IO.File]::ReadAllBytes((Resolve-Path "bin/codex-loom.exe")))
    if (-not $binaryText.Contains($asset)) {
        throw "bin/codex-loom.exe does not embed $asset"
    }
    Write-Host "verified embedded WebUI: $asset"
}
finally {
    Set-Location $originalLocation
}
