#!/bin/sh
# test.sh — the AGENTS.md §4 verification ritual, env-stripped (AGENTS.md E10).
# Strips leaked AGENTCHUTE_* vars (false "serve lease fenced" failures when run from a
# lane under the runner) before gofmt/vet/test/build. Run from the repo root.
set -u

strip_env=$(env | awk -F= '/^AGENTCHUTE_/{print "-u " $1}')

say() { printf '%s\n' "$*"; }

say "gofmt -w ."
gofmt -w . || { say "FAIL: gofmt"; exit 1; }

say "go vet ./..."
# shellcheck disable=SC2086  # $strip_env is a list of -u NAME flags, word-split by design
env $strip_env go vet ./... || { say "FAIL: go vet"; exit 1; }

say "go test ./..."
# shellcheck disable=SC2086
env $strip_env go test ./... || { say "FAIL: go test"; exit 1; }

say "go build ./..."
# shellcheck disable=SC2086
env $strip_env go build ./... || { say "FAIL: go build"; exit 1; }

say "test.sh: PASS"
