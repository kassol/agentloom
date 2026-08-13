#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$root"

for script in \
  scripts/run-claude-preview-smoke.sh \
  scripts/verify-claude-preview-results.sh \
  scripts/verify-release-gates.sh
do
  sh -n "$script"
done

go_files=$(git ls-files --cached --others --exclude-standard -- '*.go')
unformatted=""
if [ -n "$go_files" ]; then
  unformatted=$(printf '%s\n' "$go_files" | xargs gofmt -l)
fi
if [ -n "$unformatted" ]; then
  printf 'gofmt required:\n%s\n' "$unformatted" >&2
  exit 1
fi

git diff --check -- . ':(exclude)internal/webui/dist'
git diff --cached --check -- . ':(exclude)internal/webui/dist'
if git rev-parse --verify 'HEAD^' >/dev/null 2>&1; then
  git diff --check HEAD^ HEAD -- . ':(exclude)internal/webui/dist'
fi

if git grep -nE 'SessionStore|getSessionMessages|listSessions|transcript|\.claude/projects' -- \
  'internal/hub/*.go' 'internal/store/*.go' ':(exclude)**/*_test.go'; then
  echo "Claude canonical History must depend only on the Loom Store ledger" >&2
  exit 1
fi

if grep -nE 'from ["'\'']@anthropic-ai/claude-agent-sdk/|ClaudeAgentSDK\._|ClaudeAgentSDK\[['\''"]|SessionStore|\.claude/projects' \
  internal/claudegen/assets/bridge.mjs; then
  echo "Claude bridge must use only the public Agent SDK package surface" >&2
  exit 1
fi
