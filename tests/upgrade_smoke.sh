#!/bin/sh
# Rehearse the old-to-v2.5 wire break: an old updater swaps in the current
# binary, the new setup invalidates the old supervisor's lease, and an explicit
# relaunch resumes heartbeats under the new binary.

set -eu

# ---------------------------------------------------------------------------
# SAFETY: this rehearsal invalidates EVERY serve lease in whatever pool it
# resolves, which fences every supervisor in that pool. That is the point of
# the test — and it is catastrophic if the pool is a real one.
#
# The isolation below (temp HOME, temp shim dir, temp installed binary, temp
# --control-repo fixture, local download server) is NOT sufficient on its own:
# `ac serve` pins AGENTCHUTE_CONTROL_REPO / AGENTCHUTE_LOOP_DIR into every
# child's environment, and the binary resolves the pool from those env vars.
# An agent running this script under serve therefore aims the rehearsal at the
# LIVE bus despite every flag pointing at the fixture. That is exactly what
# happened on 2026-07-31: the whole fleet was fenced mid-slice, including the
# agent running the test.
#
# Two guards, both mandatory:
#   1. strip every AGENTCHUTE_* var from this script's own environment, so
#      nothing inherited can redirect the binary;
#   2. refuse to run if a live-looking pool is reachable from the environment
#      we were handed, so a stripped-but-still-dangerous context (e.g. a cwd
#      inside a control repo) cannot slip through.
# ---------------------------------------------------------------------------

live_control_repo="${AGENTCHUTE_CONTROL_REPO:-}"
live_loop_dir="${AGENTCHUTE_LOOP_DIR:-}"

for _var in $(env | sed -n 's/^\(AGENTCHUTE_[A-Za-z0-9_]*\)=.*/\1/p'); do
	unset "$_var" || true
done

refuse_live_pool() {
	candidate=$1
	label=$2
	[ -n "$candidate" ] || return 0
	loop=$candidate
	[ -d "$loop/agents" ] || loop="$candidate/.agentchute/loop"
	[ -d "$loop/agents" ] || return 0
	if ls "$loop"/agents/*.md >/dev/null 2>&1 || ls "$loop"/state/*/serve.claim >/dev/null 2>&1; then
		printf 'REFUSING TO RUN: %s points at a real agentchute pool (%s).\n' "$label" "$loop" >&2
		printf 'This rehearsal invalidates every serve lease in the pool it resolves and would fence that fleet.\n' >&2
		printf 'Run it from a shell with no AGENTCHUTE_* env and no live pool in scope (CI, or a scratch dir).\n' >&2
		exit 2
	fi
}

refuse_live_pool "$live_control_repo" "AGENTCHUTE_CONTROL_REPO"
refuse_live_pool "$live_loop_dir" "AGENTCHUTE_LOOP_DIR"
refuse_live_pool "$(pwd)" "the current working directory"

repo=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
refuse_live_pool "$repo" "the repo under test"
tmp=$(mktemp -d)
old_pid=
new_pid=
server_pid=

cleanup() {
	if [ -n "$old_pid" ]; then
		kill -CONT "$old_pid" 2>/dev/null || true
		kill "$old_pid" 2>/dev/null || true
		wait "$old_pid" 2>/dev/null || true
	fi
	if [ -n "$new_pid" ]; then
		kill "$new_pid" 2>/dev/null || true
		wait "$new_pid" 2>/dev/null || true
	fi
	if [ -n "$server_pid" ]; then
		kill "$server_pid" 2>/dev/null || true
		wait "$server_pid" 2>/dev/null || true
	fi
	rm -rf "$tmp"
}
trap cleanup EXIT HUP INT TERM

fail() {
	printf 'FAIL  %s\n' "$*" >&2
	exit 1
}

wait_for_file() {
	path=$1
	label=$2
	i=0
	while [ "$i" -lt 100 ]; do
		[ -f "$path" ] && return 0
		i=$((i + 1))
		sleep 0.1
	done
	fail "timed out waiting for $label: $path"
}

wait_for_exit() {
	pid=$1
	label=$2
	i=0
	while [ "$i" -lt 100 ]; do
		if ! kill -0 "$pid" 2>/dev/null; then
			wait "$pid" 2>/dev/null || true
			return 0
		fi
		i=$((i + 1))
		sleep 0.1
	done
	fail "timed out waiting for $label pid=$pid to exit"
}

last_seen() {
	python3 - "$1" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    print(json.load(handle)["last_seen"])
PY
}

wait_for_heartbeat_change() {
	path=$1
	before=$2
	label=$3
	i=0
	while [ "$i" -lt 50 ]; do
		after=$(last_seen "$path")
		[ "$after" != "$before" ] && return 0
		i=$((i + 1))
		sleep 0.2
	done
	fail "$label heartbeat did not advance from $before"
}

old_source="$tmp/old-source"
new_build="$tmp/new-build"
http_root="$tmp/http"
fixture="$tmp/fixture"
home="$tmp/home"
shim_dir="$tmp/shims"
installed="$tmp/bin/agentchute"
mkdir -p "$old_source" "$new_build" "$http_root" "$fixture" "$home" "$shim_dir" "$(dirname "$installed")"

base=$(git -C "$repo" merge-base HEAD origin/main)
git -C "$repo" archive "$base" | tar -x -C "$old_source"

(
	cd "$repo"
	go build -ldflags '-X main.version=2.5.0' -o "$new_build/agentchute" .
)

goos=$(go env GOOS)
goarch=$(go env GOARCH)
asset="agentchute_2.5.0_${goos}_${goarch}.tar.gz"
release_dir="$http_root/releases/download/v2.5.0"
mkdir -p "$release_dir"
tar -C "$new_build" -czf "$release_dir/$asset" agentchute
if command -v sha256sum >/dev/null 2>&1; then
	checksum=$(sha256sum "$release_dir/$asset" | awk '{print $1}')
else
	checksum=$(shasum -a 256 "$release_dir/$asset" | awk '{print $1}')
fi
printf '%s  %s\n' "$checksum" "$asset" >"$release_dir/checksums.txt"

port_file="$tmp/http.port"
python3 - "$http_root" "$port_file" <<'PY' &
import http.server
import os
import sys

os.chdir(sys.argv[1])
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), http.server.SimpleHTTPRequestHandler)
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    handle.write(str(server.server_port))
server.serve_forever()
PY
server_pid=$!
wait_for_file "$port_file" "local release server port"
port=$(cat "$port_file")

(
	cd "$old_source"
	go build \
		-ldflags "-X main.version=1.0.0 -X github.com/agentchute/agentchute/internal/cli.updateGitHubBase=http://127.0.0.1:$port" \
		-o "$installed" .
)

env \
	HOME="$home" \
	XDG_CONFIG_HOME="$home/.config" \
	SHELL=/bin/sh \
	PATH="$shim_dir:/usr/bin:/bin" \
	"$installed" setup \
		--control-repo "$fixture" \
		--init \
		--wake runner \
		--wrappers none \
		--shim-dir "$shim_dir" \
		--no-profile \
		--yes >"$tmp/setup-old.log" 2>&1

env \
	HOME="$home" \
	XDG_CONFIG_HOME="$home/.config" \
	SHELL=/bin/sh \
	PATH="$shim_dir:/usr/bin:/bin" \
	"$installed" serve \
		--as upgrade-smoke \
		--vendor openai \
		--control-repo "$fixture" \
		--interval 1 \
		-- /bin/sh -c 'trap "exit 0" TERM INT; while :; do sleep 1; done' \
		>"$tmp/serve-old.log" 2>&1 &
old_pid=$!

loop_dir="$fixture/.agentchute/loop"
claim="$loop_dir/state/upgrade-smoke/serve.claim"
runner_state="$loop_dir/state/upgrade-smoke/runner.json"
registration="$loop_dir/agents/upgrade-smoke.md"
live="$loop_dir/live/upgrade-smoke.live"
wait_for_file "$claim" "old serve claim"
wait_for_file "$runner_state" "old runner state"
wait_for_file "$registration" "old registration"
wait_for_file "$live" "old live heartbeat"
old_seen=$(last_seen "$live")
wait_for_heartbeat_change "$live" "$old_seen" "old supervisor"

# Freeze the old supervisor and mark its diagnostic state foreign-host so the
# new setup cannot politely stop it. Lease invalidation must be what fences it.
kill -STOP "$old_pid"
python3 - "$runner_state" <<'PY'
import json
import os
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as handle:
    state = json.load(handle)
state["host"] = "upgrade-smoke-foreign-host"
tmp = path + ".upgrade-smoke"
with open(tmp, "w", encoding="utf-8") as handle:
    json.dump(state, handle)
    handle.write("\n")
os.replace(tmp, path)
PY

env \
	HOME="$home" \
	XDG_CONFIG_HOME="$home/.config" \
	SHELL=/bin/sh \
	PATH="$shim_dir:/usr/bin:/bin" \
	"$installed" update \
		--control-repo "$fixture" \
		--version v2.5.0 >"$tmp/update.log" 2>&1

[ ! -e "$claim" ] || fail "new setup left the old serve claim in place"
[ -f "$registration" ] || fail "new setup deleted the registration row"

kill -CONT "$old_pid"
wait_for_exit "$old_pid" "old fenced supervisor"
old_pid=

stopped_one=$(last_seen "$live")
sleep 2
stopped_two=$(last_seen "$live")
[ "$stopped_one" = "$stopped_two" ] || fail "old last_seen continued after supervisor exit: $stopped_one -> $stopped_two"

env \
	HOME="$home" \
	XDG_CONFIG_HOME="$home/.config" \
	SHELL=/bin/sh \
	PATH="$shim_dir:/usr/bin:/bin" \
	"$installed" serve \
		--as upgrade-smoke \
		--vendor openai \
		--control-repo "$fixture" \
		--interval 1 \
		-- /bin/sh -c 'trap "exit 0" TERM INT; while :; do sleep 1; done' \
		>"$tmp/serve-new.log" 2>&1 &
new_pid=$!

wait_for_file "$claim" "new serve claim"
new_seen=$(last_seen "$live")
wait_for_heartbeat_change "$live" "$new_seen" "new supervisor"
expected_protocol=$(awk '/CurrentProtocolVersion =/ {print $3; exit}' "$repo/internal/loop/registration.go")
grep -Eq "^v: ${expected_protocol}$" "$registration" ||
	fail "new registration does not report protocol $expected_protocol"

printf 'PASS  old updater swapped in v2.5 and new setup invalidated the old serve lease\n'
printf 'PASS  old supervisor exited and last_seen stopped across two samples\n'
printf 'PASS  explicit new-binary relaunch published protocol %s and resumed heartbeats\n' "$expected_protocol"
