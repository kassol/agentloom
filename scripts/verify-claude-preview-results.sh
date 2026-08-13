#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <results-directory>" >&2
  exit 2
fi

directory=$1
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
generation=claude-runtime-v16-node24.19.0-sdk0.3.228
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
  row_commit=$(sed -n 's/.*"commit":"\([0-9a-f][0-9a-f]*\)".*/\1/p' "$result")
  commit_valid=1
  case "$row_commit" in
    *[!0-9a-f]*|"") commit_valid=0 ;;
  esac
  if ! grep -Fq '"schemaVersion":1' "$result" ||
     ! grep -Fq "\"row\":\"$row\"" "$result" ||
     ! grep -Fq "\"os\":\"$expected_os\"" "$result" ||
     ! grep -Fq "\"arch\":\"$expected_arch\"" "$result" ||
     ! grep -Fq "\"generation\":\"$generation\"" "$result" ||
     [ "$commit_valid" -ne 1 ] ||
     [ "${#row_commit}" -ne 40 ] ||
     ! grep -Fq '"status":"passed"' "$result" ||
     ! grep -Fq '"reason":"verified"' "$result" ||
     ! grep -Fq '"productionReady":false' "$result"; then
    echo "$row: result is missing, failed, or malformed: $(tr -d '\n' <"$result")" >&2
    missing=1
  else
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
