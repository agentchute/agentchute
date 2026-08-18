#!/bin/sh
# sshd-test.sh — the real-sshd hub integration suite (PLAN WI-6.1/6.2).
# Strips EVERY AGENTCHUTE_* var (the same idiom as tools/test.sh:7) and sets
# only AGENTCHUTE_SSHD_TEST=1, so a run from inside a serve session can never
# reach the live pool.
set -u
strip_env=$(env | awk -F= '/^AGENTCHUTE_/{print "-u " $1}')
# -race, deliberately. This milestone exists because in-process coverage hid
# defects the real transport exposed; the harness itself runs sshd, a serve
# lane, mux masters and the client concurrently, which is precisely the shape
# the detector is for. Measured cost is a few seconds on a ~100s suite.
exec env $strip_env AGENTCHUTE_SSHD_TEST=1 go test -race -tags sshd_integration ./integration/sshd/...
