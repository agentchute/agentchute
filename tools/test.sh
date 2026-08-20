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

# -race, because CI and the release job both run it and this ritual is what lanes
# are told to run before pushing. Without it the local gate is strictly weaker
# than the one judging the push, and weaker in exactly the class that is
# invisible locally and intermittent on a loaded runner. It has already cost a
# red that way (#148), and release.yaml runs -race too — so the same race could
# fail the release job AFTER the tag was pushed.
#
# It roughly doubles wall-clock. That is the price of the local gate matching the
# real one; anyone who wants a fast path should add an explicit skip flag rather
# than let the default quietly differ from CI again.
say "go test -race ./..."
# shellcheck disable=SC2086
env $strip_env go test -race ./... || { say "FAIL: go test -race"; exit 1; }

say "cd conformance && go test ./..."
# shellcheck disable=SC2086
(cd conformance && env $strip_env go test ./...) || { say "FAIL: conformance go test"; exit 1; }

say "go build ./..."
# shellcheck disable=SC2086
env $strip_env go build ./... || { say "FAIL: go build"; exit 1; }

say "test.sh: PASS"
