#!/bin/sh
# Rehearse the hook-refresh-reliability fixes (2026-08-11 investigation) across
# a REAL old-release-to-current binary swap: a genuinely stale hook file and a
# pre-marker AGENTCHUTE.md (built from the real v1.0.0 tag) must both refresh
# to the current binary's canonical templates after `agentchute update`, and
# doctor must report clean afterward. Sibling to upgrade_smoke.sh (which
# proves the registration/lease wire-break separately, with wrappers=none —
# it never installs a hook file, so it cannot exercise this at all). This is
# the CI-level proof that closes the gap: prior to this file, no CI job ever
# exercised hook-file refresh across a real version boundary — every check
# was either a Go unit test against synthetic fixtures, or manual rehearsal
# (see docs/decisions/ if that changes).

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
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT HUP INT TERM

fail() {
	printf 'FAIL  %s\n' "$*" >&2
	exit 1
}

old_source="$tmp/old-source"
new_build="$tmp/new-build"
fixture="$tmp/fixture"
home="$tmp/home"
shim_dir="$tmp/shims"
old_bin="$tmp/bin/old-agentchute"
new_bin="$tmp/bin/new-agentchute"
mkdir -p "$old_source" "$new_build" "$fixture" "$home" "$shim_dir" "$(dirname "$old_bin")"

# The real v1.0.0 tag: predates the enrollment/spec version marker and ships
# `poller ensure` in its hook templates, a subcommand later removed — the
# exact pairing sonnet and codex each independently used by hand to confirm
# the root cause during the investigation this rehearsal now automates.
git -C "$repo" archive v1.0.0 | tar -x -C "$old_source"
(cd "$old_source" && go build -ldflags '-X main.version=1.0.0' -o "$old_bin" .)

(cd "$repo" && go build -o "$new_bin" .)
new_version=$("$new_bin" --version)

env HOME="$home" XDG_CONFIG_HOME="$home/.config" SHELL=/bin/sh PATH="$shim_dir:/usr/bin:/bin" \
	"$old_bin" setup \
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
	"$new_bin" boot --as claude-code --vendor anthropic --control-repo "$fixture" >"$tmp/boot.log" 2>&1 ||
	fail "boot did not succeed:
$(cat "$tmp/boot.log")"

env HOME="$home" XDG_CONFIG_HOME="$home/.config" SHELL=/bin/sh PATH="$shim_dir:/usr/bin:/bin" AGENTCHUTE_BIN="$new_bin" \
	"$new_bin" doctor --as claude-code --control-repo "$fixture" >"$tmp/doctor-before.log" 2>&1 && {
	fail "doctor unexpectedly passed against the stale v1.0.0 hook/spec BEFORE any update ran"
}
grep -qi 'poller' "$tmp/doctor-before.log" ||
	fail "doctor's pre-update blocker did not name the removed poller subcommand:
$(cat "$tmp/doctor-before.log")"

# The update path itself: point AGENTCHUTE_BIN at the new binary directly
# (this rehearsal is proving the setup/hooks refresh logic, not the
# GitHub-download step install.sh and upgrade_smoke.sh already cover) and run
# the new binary's own `setup` over the fixture, exactly what `update`'s
# resync does after swapping the binary.
env HOME="$home" XDG_CONFIG_HOME="$home/.config" SHELL=/bin/sh PATH="$shim_dir:/usr/bin:/bin" \
	"$new_bin" setup \
	--control-repo "$fixture" \
	--wake runner \
	--wrappers claude-code \
	--shim-dir "$shim_dir" \
	--no-profile \
	--yes >"$tmp/setup-new.log" 2>&1 ||
	fail "new binary's resync setup did not succeed:
$(cat "$tmp/setup-new.log")"

new_hook=$(cat "$hook_path")
[ "$new_hook" != "$old_hook" ] || fail "hook file is byte-identical to the stale v1.0.0 template after resync"
grep -q 'poller' "$hook_path" &&
	fail "refreshed hook still references the removed poller subcommand"

grep -q 'agentchute-spec' "$spec_path" ||
	fail "AGENTCHUTE.md was not stamped with the spec marker after resync"

env HOME="$home" XDG_CONFIG_HOME="$home/.config" SHELL=/bin/sh PATH="$shim_dir:/usr/bin:/bin" AGENTCHUTE_BIN="$new_bin" \
	"$new_bin" doctor --as claude-code --control-repo "$fixture" >"$tmp/doctor-after.log" 2>&1 ||
	fail "doctor still blocks after resync:
$(cat "$tmp/doctor-after.log")"

printf 'PASS  v1.0.0 hook (referencing removed poller subcommand) was stale before any update\n'
printf 'PASS  doctor correctly blocked on the stale hook before resync\n'
printf 'PASS  resync refreshed the hook to the current (%s) canonical template\n' "$new_version"
printf 'PASS  resync stamped AGENTCHUTE.md with the current spec marker\n'
printf 'PASS  doctor is clean after resync\n'
