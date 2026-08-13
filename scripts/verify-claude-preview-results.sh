#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <results-directory>" >&2
  exit 2
fi

directory=$1
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
generation=claude-runtime-v9-node24.19.0-sdk0.3.228
expected_commit=$(git -C "$root" rev-parse HEAD)
missing=0
verified_commit=
for row in darwin-arm64 darwin-x64 linux-arm64 linux-x64; do
  result="$directory/$row.json"
  expected_os=${row%-*}
  expected_arch=${row#*-}
  if [ ! -f "$result" ]; then
    echo "$row: missing result" >&2
    missing=1
    continue
  fi
  if ! rg -q '"schemaVersion":1' "$result" ||
     ! rg -q "\"row\":\"$row\"" "$result" ||
     ! rg -q "\"os\":\"$expected_os\"" "$result" ||
     ! rg -q "\"arch\":\"$expected_arch\"" "$result" ||
     ! rg -q "\"generation\":\"$generation\"" "$result" ||
     ! rg -q '"commit":"[0-9a-f]{40}"' "$result" ||
     ! rg -q '"status":"passed"' "$result" ||
     ! rg -q '"reason":"verified"' "$result" ||
     ! rg -q '"productionReady":false' "$result"; then
    echo "$row: result is missing, failed, or malformed: $(tr -d '\n' <"$result")" >&2
    missing=1
  else
    row_commit=$(rg -o '"commit":"[0-9a-f]{40}"' "$result" | cut -d '"' -f4)
    if [ -z "$verified_commit" ]; then
      verified_commit=$row_commit
    elif [ "$row_commit" != "$verified_commit" ]; then
      echo "$row: commit $row_commit does not match $verified_commit" >&2
      missing=1
      continue
    fi
    if [ "$row_commit" != "$expected_commit" ]; then
      echo "$row: commit $row_commit does not match checked-out release $expected_commit" >&2
      missing=1
      continue
    fi
    echo "$row: passed at $row_commit"
  fi
done

[ "$missing" -eq 0 ]
