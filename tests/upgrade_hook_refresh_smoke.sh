#!/bin/sh
# Rehearse the hook-refresh-reliability fixes (2026-08-11 investigation) across
# a REAL `agentchute update` run: a genuinely stale hook file and a pre-marker
# AGENTCHUTE.md (built from the real v1.0.0 tag) must both refresh to the
# current binary's canonical templates when the OLD installed binary downloads
# and swaps to the current one and re-execs its own setup resync — not merely
# when the new binary's `setup` is invoked directly (codex review on this
# file's first revision [P1]: a direct-setup rehearsal cannot catch a
# regression in `update`'s own resync invocation/composition, e.g. the PR #130
# lease-ordering fix). Sibling to upgrade_smoke.sh (which proves the
# registration/lease wire-break separately, with --wrappers none — it never
# installs a hook file, so it cannot exercise this at all); this script
# reuses that file's local-release-server pattern for the same reason.

set -eu

# Same two-guard safety rationale as upgrade_smoke.sh: `agentchute update`
# invalidates serve leases and rewrites hook/enrollment files in whatever
# control repo it resolves. Strip AGENTCHUTE_* and refuse a live-looking pool
# before doing anything else.
for _var in $(env | sed -n 's/^\(AGENTCHUTE_[A-Za-z0-9_]*\)=.*/\1/p'); do
	unset "$_var" || true
done

pool_has_registration() {
	loop=$1
	for f in "$loop"/agents/*.md; do
		[ -e "$f" ] || continue
		base=${f##*/}
		case "$base" in
		README.md | *.example.md) continue ;;
		esac
		return 0
	done
	return 1
}

refuse_live_pool() {
	candidate=$1
	label=$2
	[ -n "$candidate" ] || return 0
	loop=$candidate
	[ -d "$loop/agents" ] || loop="$candidate/.agentchute/loop"
	[ -d "$loop/agents" ] || return 0
	if pool_has_registration "$loop" || ls "$loop"/state/*/serve.claim >/dev/null 2>&1; then
		printf 'REFUSING TO RUN: %s points at a real agentchute pool (%s).\n' "$label" "$loop" >&2
		exit 2
	fi
}

refuse_live_pool "$(pwd)" "the current working directory"
repo=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
refuse_live_pool "$repo" "the repo under test"

tmp=$(mktemp -d)
server_pid=
cleanup() {
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

old_source="$tmp/old-source"
new_build="$tmp/new-build"
http_root="$tmp/http"
fixture="$tmp/fixture"
home="$tmp/home"
shim_dir="$tmp/shims"
installed="$tmp/bin/agentchute"
mkdir -p "$old_source" "$new_build" "$http_root" "$fixture" "$home" "$shim_dir" "$(dirname "$installed")"

# Build the CURRENT worktree and publish it as a fake v9.9.9 release on a
# local-only HTTP server. The version number is arbitrary and deliberately
# unreal (a real repo release is never actually "v9.9.9") — what matters is
# that it is newer than 1.0.0, so `update`'s version comparison proceeds.
new_version_tag=v9.9.9
new_bin="$new_build/agentchute"
(cd "$repo" && go build -ldflags "-X main.version=${new_version_tag#v}" -o "$new_bin" .)

goos=$(go env GOOS)
goarch=$(go env GOARCH)
asset="agentchute_${new_version_tag#v}_${goos}_${goarch}.tar.gz"
release_dir="$http_root/releases/download/$new_version_tag"
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

# The real v1.0.0 tag: predates the enrollment/spec version marker and ships
# `poller ensure` in its hook templates, a subcommand later removed — the
# exact pairing sonnet and codex each independently used by hand to confirm
# the root cause during the investigation this rehearsal now automates. Built
# with the download base pointed at the local server above, exactly like
# upgrade_smoke.sh, so `update` never touches the real network.
git -C "$repo" archive v1.0.0 | tar -x -C "$old_source"
(
	cd "$old_source"
	go build \
		-ldflags "-X main.version=1.0.0 -X github.com/agentchute/agentchute/internal/cli.updateGitHubBase=http://127.0.0.1:$port" \
		-o "$installed" .
)

env HOME="$home" XDG_CONFIG_HOME="$home/.config" SHELL=/bin/sh PATH="$shim_dir:/usr/bin:/bin" \
	"$installed" setup \
	--control-repo "$fixture" \
	--init \
	--wake runner \
	--wrappers claude-code \
	--shim-dir "$shim_dir" \
	--no-profile \
	--yes >"$tmp/setup-old.log" 2>&1 ||
	fail "old (v1.0.0) setup did not succeed:
$(cat "$tmp/setup-old.log")"

hook_path="$fixture/.claude/settings.json"
spec_path="$fixture/AGENTCHUTE.md"
[ -f "$hook_path" ] || fail "old setup did not install a claude-code hook file"
grep -q 'poller' "$hook_path" ||
	fail "sanity check failed: v1.0.0 hook does not reference poller (fixture assumption is wrong)"
grep -q 'agentchute-spec' "$spec_path" &&
	fail "sanity check failed: v1.0.0 AGENTCHUTE.md already carries a spec marker (fixture assumption is wrong)"
old_hook=$(cat "$hook_path")

# A real agent would have registered before doctor ever runs; boot it now so
# the ONLY thing left for doctor to catch is the stale hook/spec, not an
# unrelated missing-registration blocker.
env HOME="$home" XDG_CONFIG_HOME="$home/.config" SHELL=/bin/sh PATH="$shim_dir:/usr/bin:/bin" \
	"$installed" boot --as claude-code --vendor anthropic --control-repo "$fixture" >"$tmp/boot.log" 2>&1 ||
	fail "boot did not succeed:
$(cat "$tmp/boot.log")"

# Deliberately the NEW binary's doctor, not $installed (which is still the
# OLD v1.0.0 binary at this point — its own doctor doesn't know `poller` was
# later removed, so it would pass cleanly and prove nothing). This models
# "swap the binary, then run its doctor before resyncing" — precisely the
# gap window PR #127/#131 close.
env HOME="$home" XDG_CONFIG_HOME="$home/.config" SHELL=/bin/sh PATH="$shim_dir:/usr/bin:/bin" AGENTCHUTE_BIN="$new_bin" \
	"$new_bin" doctor --as claude-code --control-repo "$fixture" >"$tmp/doctor-before.log" 2>&1 && {
	fail "doctor unexpectedly passed against the stale v1.0.0 hook/spec BEFORE any update ran"
}
grep -qi 'poller' "$tmp/doctor-before.log" ||
	fail "doctor's pre-update blocker did not name the removed poller subcommand:
$(cat "$tmp/doctor-before.log")"

# THE ACTUAL UPDATE PATH: the old installed binary downloads the new one from
# the local server, swaps itself at $installed, and re-execs setup to resync
# — the exact composition PR #130 changed the ordering of (hook-compatibility
# repair now runs before lease invalidation). Nothing here calls `setup`
# directly.
env HOME="$home" XDG_CONFIG_HOME="$home/.config" SHELL=/bin/sh PATH="$shim_dir:/usr/bin:/bin" \
	"$installed" update \
	--control-repo "$fixture" \
	--version "$new_version_tag" >"$tmp/update.log" 2>&1 ||
	fail "update did not succeed:
$(cat "$tmp/update.log")"

swapped_version=$("$installed" --version)
[ "$swapped_version" = "agentchute ${new_version_tag#v}" ] ||
	fail "update did not actually swap the installed binary: reports $swapped_version, want agentchute ${new_version_tag#v}"

new_hook=$(cat "$hook_path")
[ "$new_hook" != "$old_hook" ] || fail "hook file is byte-identical to the stale v1.0.0 template after update"
grep -q 'poller' "$hook_path" &&
	fail "refreshed hook still references the removed poller subcommand"

grep -q 'agentchute-spec' "$spec_path" ||
	fail "AGENTCHUTE.md was not stamped with the spec marker after update"

env HOME="$home" XDG_CONFIG_HOME="$home/.config" SHELL=/bin/sh PATH="$shim_dir:/usr/bin:/bin" AGENTCHUTE_BIN="$installed" \
	"$installed" doctor --as claude-code --control-repo "$fixture" >"$tmp/doctor-after.log" 2>&1 ||
	fail "doctor still blocks after update:
$(cat "$tmp/doctor-after.log")"

printf 'PASS  v1.0.0 hook (referencing removed poller subcommand) was stale before any update\n'
printf 'PASS  doctor correctly blocked on the stale hook before update\n'
printf 'PASS  agentchute update actually swapped the installed binary (%s)\n' "$swapped_version"
printf 'PASS  agentchute update refreshed the hook to the current canonical template\n'
printf 'PASS  agentchute update stamped AGENTCHUTE.md with the current spec marker\n'
printf 'PASS  doctor is clean after update\n'
