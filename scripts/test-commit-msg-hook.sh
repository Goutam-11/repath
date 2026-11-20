#!/bin/sh
# Exercise .githooks/commit-msg in a throwaway repo. Runs inside `make check`.
# Each case sets the identity and the message, then asserts the hook's verdict.

set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
hook="$root/.githooks/commit-msg"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

[ -x "$hook" ] || { echo "hooks-check: $hook is not executable" >&2; exit 1; }

cd "$tmp"
git init -q .
msg=$tmp/msg

pass=0
fail=0

# run_case <name> <expect: 0|1> <author-email> <message>
run_case() {
	name=$1
	expect=$2
	email=$3
	printf '%s' "$4" > "$msg"
	got=0
	# `|| got=$?` keeps `set -e` from aborting the run on an expected rejection.
	GIT_AUTHOR_NAME="Test" GIT_AUTHOR_EMAIL="$email" \
	GIT_COMMITTER_NAME="Test" GIT_COMMITTER_EMAIL="$email" \
		sh "$hook" "$msg" >/dev/null 2>&1 || got=$?
	if [ "$got" -eq "$expect" ]; then
		pass=$((pass + 1))
	else
		echo "FAIL: $name (expected exit $expect, got $got)" >&2
		fail=$((fail + 1))
	fi
}

run_case "plain message, verified gmail" 0 "goutamkumar.sharmq@gmail.com" "feat: add a thing"
run_case "ID-prefixed noreply identity" 0 "159244984+Goutam-11@users.noreply.github.com" "feat: add a thing"
run_case "bare noreply identity" 1 "goutamsharma@users.noreply.github.com" "feat: add a thing"
run_case "bare noreply co-author trailer" 1 "goutamkumar.sharmq@gmail.com" \
	"feat: add a thing

Co-authored-by: Someone <goutamsharma@users.noreply.github.com>"
run_case "ID-prefixed co-author trailer" 0 "goutamkumar.sharmq@gmail.com" \
	"feat: add a thing

Co-authored-by: Someone <159244984+Goutam-11@users.noreply.github.com>"
run_case "bare noreply in body prose" 1 "goutamkumar.sharmq@gmail.com" \
	"docs: mention goutamsharma@users.noreply.github.com in a note"
run_case "bare noreply in identity and message" 1 "goutamsharma@users.noreply.github.com" \
	"fix: x

Co-authored-by: Someone <goutamsharma@users.noreply.github.com>"

echo "hooks-check: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
