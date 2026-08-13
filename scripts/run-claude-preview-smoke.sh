#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <darwin|linux> <arm64|x64> <result.json>" >&2
  exit 2
fi

expected_os=$1
expected_arch=$2
result=$3
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
generation=claude-runtime-v14-node24.19.0-sdk0.3.228
case "$(uname -s)" in
  Darwin) actual_os=darwin ;;
  Linux) actual_os=linux ;;
  *) actual_os=unsupported ;;
esac
case "$(uname -m)" in
  arm64|aarch64) actual_arch=arm64 ;;
  x86_64|amd64) actual_arch=x64 ;;
  *) actual_arch=unsupported ;;
esac
commit=$(git -C "$root" rev-parse HEAD)
status=missing
reason=prerequisite_absent
log_file=${result%.json}.log

mkdir -p "$(dirname "$result")"
if [ "$actual_os" != "$expected_os" ] || [ "$actual_arch" != "$expected_arch" ]; then
  status=failed
  reason=runner_platform_mismatch
elif [ -z "${CLAUDE_PREVIEW_ANTHROPIC_API_KEY:-}" ]; then
  status=missing
  reason=product_safe_credential_absent
else
  set +e
  (
    cd "$root"
    CLAUDE_REAL_SMOKE=1 \
    CLAUDE_REAL_INSTALL="${CLAUDE_PREVIEW_INSTALL_EXACT_GENERATION:-0}" \
    CLAUDE_REAL_ACCEPT_INSTALL_TERMS="${CLAUDE_PREVIEW_ACCEPT_INSTALL_TERMS:-0}" \
    CLAUDE_REAL_API_KEY="$CLAUDE_PREVIEW_ANTHROPIC_API_KEY" \
      go test ./internal/hub -run '^TestClaudeRuntimeManagedGenerationRealSmoke$' -count=1 -v
  ) >"$log_file" 2>&1
  code=$?
  set -e
  if [ "$code" -eq 0 ] && grep -Fq -- '--- PASS: TestClaudeRuntimeManagedGenerationRealSmoke' "$log_file"; then
    status=passed
    reason=verified
  elif [ "$code" -eq 0 ] && grep -Fq -- '--- SKIP: TestClaudeRuntimeManagedGenerationRealSmoke' "$log_file"; then
    status=missing
    reason=smoke_skipped
  else
    status=failed
    reason=smoke_failed
  fi
fi

printf '{"schemaVersion":1,"row":"%s-%s","os":"%s","arch":"%s","generation":"%s","commit":"%s","status":"%s","reason":"%s","productionReady":false}\n' \
  "$expected_os" "$expected_arch" "$expected_os" "$expected_arch" "$generation" "$commit" "$status" "$reason" >"$result"

[ "$status" = passed ]
