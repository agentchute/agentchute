#!/bin/sh
# Unit tests for install.sh helpers. Sources install.sh with the library
# guard so main() doesn't run; exercises pure-logic functions directly.

set -eu

cd "$(dirname "$0")/.."

# shellcheck disable=SC1091  # source is checked separately
AGENTCHUTE_INSTALL_LIB=1 . ./install.sh

PASS=0
FAIL=0

assert_eq() {
	# $1=label, $2=expected, $3=actual
	if [ "$2" = "$3" ]; then
		PASS=$((PASS + 1))
		printf 'PASS  %s\n' "$1"
	else
		FAIL=$((FAIL + 1))
		printf 'FAIL  %s\n  expected: %s\n  actual:   %s\n' "$1" "$2" "$3"
	fi
}

assert_true() {
	# $1=label, $2=command (eval'd)
	if eval "$2"; then
		PASS=$((PASS + 1))
		printf 'PASS  %s\n' "$1"
	else
		FAIL=$((FAIL + 1))
		printf 'FAIL  %s  (command failed: %s)\n' "$1" "$2"
	fi
}

assert_false() {
	# $1=label, $2=command (eval'd)
	if eval "$2" 2>/dev/null; then
		FAIL=$((FAIL + 1))
		printf 'FAIL  %s  (command unexpectedly succeeded: %s)\n' "$1" "$2"
	else
		PASS=$((PASS + 1))
		printf 'PASS  %s\n' "$1"
	fi
}

# -----------------------------------------------------------------------
# is_valid_version
# -----------------------------------------------------------------------

assert_true  "valid: v0.1.0"        'is_valid_version v0.1.0'
assert_true  "valid: v1.2.3-rc.1"   'is_valid_version v1.2.3-rc.1'
assert_true  "valid: v0.1.0_alpha"  'is_valid_version v0.1.0_alpha'
assert_false "invalid: missing v"   'is_valid_version 0.1.0'
assert_false "invalid: empty"       'is_valid_version ""'
assert_false "invalid: slash"       'is_valid_version v0/1/0'
assert_false "invalid: space"       "is_valid_version 'v 0.1.0'"
assert_false "invalid: shell meta"  "is_valid_version 'v0.1.0\$x'"

# -----------------------------------------------------------------------
# release base URL
# -----------------------------------------------------------------------

assert_eq "release base: default GitHub" \
	"https://github.com/agentchute/agentchute" "$GITHUB"
override_base=$(AGENTCHUTE_BASE_URL="file:///tmp/agentchute-release" AGENTCHUTE_INSTALL_LIB=1 sh -c '. ./install.sh; printf "%s" "$GITHUB"')
assert_eq "release base: test override" \
	"file:///tmp/agentchute-release" "$override_base"

# -----------------------------------------------------------------------
# is_valid_install_dir
# -----------------------------------------------------------------------

assert_true  "valid dir: absolute"      'is_valid_install_dir /usr/local/bin'
assert_true  "valid dir: home expanded" 'is_valid_install_dir /Users/alice/.local/bin'
assert_false "invalid dir: empty"       'is_valid_install_dir ""'
assert_false "invalid dir: colon"       "is_valid_install_dir '/tmp:bad'"
# Newline in path → rejected. Built with printf for portability.
# shellcheck disable=SC2034  # nl_dir is used via eval expansion
nl_dir=$(printf '/tmp/\nbad')
assert_false "invalid dir: newline"     "is_valid_install_dir \"\$nl_dir\""

# -----------------------------------------------------------------------
# PATH helpers
# -----------------------------------------------------------------------

old_path=$PATH
PATH="/usr/bin:/tmp/agentchute-bin"
assert_true  "path contains exact dir"     'path_contains_dir /tmp/agentchute-bin'
assert_false "path rejects substring dir"  'path_contains_dir /tmp/agentchute'
PATH=$old_path

old_home=${HOME:-}
old_shell=${SHELL:-}
old_profile=${AGENTCHUTE_PROFILE:-}
old_no_path=${AGENTCHUTE_NO_PATH_UPDATE:-}
profile_tmp=$(mktemp -d)
HOME="$profile_tmp/home"
SHELL="/bin/zsh"
AGENTCHUTE_PROFILE="$profile_tmp/profile"
AGENTCHUTE_NO_PATH_UPDATE=0
PATH="/bin:/usr/bin"
mkdir -p "$HOME/.agentchute/bin"
ensure_path_available "$HOME/.agentchute/bin" "launcher shims" >/dev/null 2>&1
assert_true "profile path block written" "grep -F '# agentchute PATH entry for launcher shims' \"\$AGENTCHUTE_PROFILE\" >/dev/null"
assert_true "profile path uses HOME" "grep -F 'export PATH=\"\$HOME/.agentchute/bin:\$PATH\"' \"\$AGENTCHUTE_PROFILE\" >/dev/null"
before_lines=$(wc -l <"$AGENTCHUTE_PROFILE" | tr -d ' ')
ensure_path_available "$HOME/.agentchute/bin" "launcher shims" >/dev/null 2>&1
after_lines=$(wc -l <"$AGENTCHUTE_PROFILE" | tr -d ' ')
assert_eq "profile path block not duplicated" "$before_lines" "$after_lines"
rm -rf "$profile_tmp"
HOME=$old_home
SHELL=$old_shell
PATH=$old_path
if [ -n "$old_profile" ]; then AGENTCHUTE_PROFILE=$old_profile; else unset AGENTCHUTE_PROFILE; fi
if [ -n "$old_no_path" ]; then AGENTCHUTE_NO_PATH_UPDATE=$old_no_path; else unset AGENTCHUTE_NO_PATH_UPDATE; fi

# -----------------------------------------------------------------------
# extract_tag_from_url
# -----------------------------------------------------------------------

# Happy path: GitHub /releases/latest redirects to /releases/tag/<tag>.
tag=$(extract_tag_from_url "https://github.com/agentchute/agentchute/releases/tag/v0.1.0")
assert_eq "extract tag v0.1.0" "v0.1.0" "$tag"

tag=$(extract_tag_from_url "https://github.com/agentchute/agentchute/releases/tag/v0.2.0-rc.1")
assert_eq "extract tag with rc" "v0.2.0-rc.1" "$tag"

# Unexpected URL shape → empty.
tag=$(extract_tag_from_url "https://github.com/agentchute/agentchute/releases")
assert_eq "extract empty on bad URL" "" "$tag"

# -----------------------------------------------------------------------
# checksum_line_for — exact-filename match, not substring
# -----------------------------------------------------------------------

tmp=$(mktemp)
cat >"$tmp" <<'EOF'
abc123  agentchute_0.1.0_darwin_amd64.tar.gz
def456  agentchute_0.1.0_darwin_arm64.tar.gz
fedcba  agentchute_0.1.0_linux_amd64.tar.gz
EOF

line=$(checksum_line_for "$tmp" "agentchute_0.1.0_darwin_arm64.tar.gz")
assert_eq "checksum line: exact match" "def456  agentchute_0.1.0_darwin_arm64.tar.gz" "$line"

# Some tools emit `<hash> *<filename>` (binary mode marker). Defensive `*` strip
# in checksum_line_for should handle that too.
tmp_star=$(mktemp)
cat >"$tmp_star" <<'EOF'
deadbeef *agentchute_0.1.0_linux_arm64.tar.gz
EOF
line=$(checksum_line_for "$tmp_star" "agentchute_0.1.0_linux_arm64.tar.gz")
assert_eq "checksum line: star marker stripped" "deadbeef *agentchute_0.1.0_linux_arm64.tar.gz" "$line"
rm -f "$tmp_star"

# Substring match should NOT succeed: looking for "arm64.tar.gz" alone returns empty.
if checksum_line_for "$tmp" "arm64.tar.gz" >/dev/null 2>&1; then
	FAIL=$((FAIL + 1))
	printf 'FAIL  checksum line: substring match rejected\n'
else
	PASS=$((PASS + 1))
	printf 'PASS  checksum line: substring match rejected\n'
fi

# Missing file: returns nonzero, empty stdout.
out=$(checksum_line_for "$tmp" "agentchute_0.1.0_windows_amd64.tar.gz" 2>/dev/null || true)
assert_eq "checksum line: missing → empty" "" "$out"

rm -f "$tmp"

# -----------------------------------------------------------------------
# true main-flow: real install.sh, real network stack against a local
# release fixture (no actual network egress). Covers the upgrade-in-place
# setup-disposition gate and the PATH-shadow warning (2026-08-11 hook-
# refresh-reliability investigation, findings 1 and 6).
# -----------------------------------------------------------------------

# These scenarios depend on the ambient controlling-terminal state: CI (and
# any sandboxed agent shell) has none, matching the real-world bug — any
# non-interactive automation running install.sh over an existing install. A
# developer running this file from an interactive terminal has one, so the
# no-tty-specific assertions below are skipped rather than false-failed.
if ( : </dev/tty ) 2>/dev/null; then
	mainflow_have_tty=1
else
	mainflow_have_tty=0
fi

mainflow_root=$(mktemp -d)
mainflow_http="$mainflow_root/http"
mainflow_ver="v9.9.9"
mainflow_bare_ver="9.9.9"
mainflow_goos=$(detect_os)
mainflow_goarch=$(detect_arch)
mainflow_asset="agentchute_${mainflow_bare_ver}_${mainflow_goos}_${mainflow_goarch}.tar.gz"
mainflow_release_dir="$mainflow_http/releases/download/$mainflow_ver"
mkdir -p "$mainflow_release_dir"

mainflow_stage="$mainflow_root/stage"
mkdir -p "$mainflow_stage"
cat >"$mainflow_stage/agentchute" <<'FAKEBIN'
#!/bin/sh
echo "fake-agentchute-test-fixture"
FAKEBIN
chmod +x "$mainflow_stage/agentchute"
tar -C "$mainflow_stage" -czf "$mainflow_release_dir/$mainflow_asset" agentchute

if command -v sha256sum >/dev/null 2>&1; then
	mainflow_sum=$(sha256sum "$mainflow_release_dir/$mainflow_asset" | awk '{print $1}')
else
	mainflow_sum=$(shasum -a 256 "$mainflow_release_dir/$mainflow_asset" | awk '{print $1}')
fi
printf '%s  %s\n' "$mainflow_sum" "$mainflow_asset" >"$mainflow_release_dir/checksums.txt"

mainflow_port_file="$mainflow_root/http.port"
python3 - "$mainflow_http" "$mainflow_port_file" <<'PY' >"$mainflow_root/http.log" 2>&1 &
import http.server
import os
import sys

os.chdir(sys.argv[1])
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), http.server.SimpleHTTPRequestHandler)
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    handle.write(str(server.server_port))
server.serve_forever()
PY
mainflow_server_pid=$!

mainflow_wait_i=0
while [ ! -s "$mainflow_port_file" ]; do
	mainflow_wait_i=$((mainflow_wait_i + 1))
	if [ "$mainflow_wait_i" -ge 50 ]; then
		FAIL=$((FAIL + 1))
		printf 'FAIL  main-flow: local release server did not start\n'
		break
	fi
	sleep 0.1
done
mainflow_port=$(cat "$mainflow_port_file" 2>/dev/null || true)

if [ -n "$mainflow_port" ]; then
	mainflow_base="http://127.0.0.1:$mainflow_port"

	# Every real sh install.sh invocation below MUST override
	# AGENTCHUTE_INSTALL_LIB=0: the AGENTCHUTE_INSTALL_LIB=1 . ./install.sh
	# line at the top of this file sourced install.sh via the shell's `.`
	# special builtin, whose variable-assignment prefix persists (and
	# exports) in the CURRENT shell rather than scoping to the sourced file
	# — a POSIX special-builtin quirk (`VAR=val cmd` scopes normally;
	# `VAR=val .` does not). Left unset, every main-flow sh install.sh below
	# would silently inherit AGENTCHUTE_INSTALL_LIB=1 and skip main()
	# entirely, exiting 0 having done nothing.

	# Scenario A: fresh install (no existing binary at target), explicit
	# --no-setup so the scenario doesn't depend on ambient tty state. Must
	# succeed and actually write the target binary.
	mainflow_fresh_to="$mainflow_root/fresh-to"
	mkdir -p "$mainflow_fresh_to"
	mainflow_rc=0
	AGENTCHUTE_INSTALL_LIB=0 AGENTCHUTE_BASE_URL="$mainflow_base" HOME="$mainflow_root/fresh-home" AGENTCHUTE_NO_PATH_UPDATE=1 \
		sh install.sh --version "$mainflow_ver" --to "$mainflow_fresh_to" --no-setup \
		>"$mainflow_root/fresh.out" 2>"$mainflow_root/fresh.err" || mainflow_rc=$?
	assert_eq "main-flow: fresh install exits 0" "0" "$mainflow_rc"
	assert_true "main-flow: fresh install wrote the target binary" "[ -x \"$mainflow_fresh_to/agentchute\" ]"

	if [ "$mainflow_have_tty" = "0" ]; then
		# Scenario B: upgrade-in-place, do_setup left at its default (auto),
		# no controlling terminal. Must refuse BEFORE swapping the binary.
		mainflow_upgrade_to="$mainflow_root/upgrade-to"
		mkdir -p "$mainflow_upgrade_to"
		printf '#!/bin/sh\necho old-agentchute\n' >"$mainflow_upgrade_to/agentchute"
		chmod +x "$mainflow_upgrade_to/agentchute"
		mainflow_old_marker=$(cat "$mainflow_upgrade_to/agentchute")

		mainflow_rc=0
		AGENTCHUTE_INSTALL_LIB=0 AGENTCHUTE_BASE_URL="$mainflow_base" HOME="$mainflow_root/upgrade-home" AGENTCHUTE_NO_PATH_UPDATE=1 \
			sh install.sh --version "$mainflow_ver" --to "$mainflow_upgrade_to" \
			>"$mainflow_root/upgrade.out" 2>"$mainflow_root/upgrade.err" || mainflow_rc=$?

		assert_true "main-flow: no-tty upgrade-in-place refuses (nonzero exit)" "[ $mainflow_rc -ne 0 ]"
		assert_true "main-flow: refusal names the cause" \
			"grep -qi 'no controlling terminal' \"$mainflow_root/upgrade.err\""
		mainflow_after_marker=$(cat "$mainflow_upgrade_to/agentchute")
		assert_eq "main-flow: refused upgrade never swapped the binary" "$mainflow_old_marker" "$mainflow_after_marker"

		# Scenario C: same upgrade-in-place, but explicit --no-setup. Must
		# proceed (deliberate operator intent, not an environmental accident).
		mainflow_explicit_to="$mainflow_root/explicit-to"
		mkdir -p "$mainflow_explicit_to"
		printf '#!/bin/sh\necho old-agentchute\n' >"$mainflow_explicit_to/agentchute"
		chmod +x "$mainflow_explicit_to/agentchute"

		mainflow_rc=0
		AGENTCHUTE_INSTALL_LIB=0 AGENTCHUTE_BASE_URL="$mainflow_base" HOME="$mainflow_root/explicit-home" AGENTCHUTE_NO_PATH_UPDATE=1 \
			sh install.sh --version "$mainflow_ver" --to "$mainflow_explicit_to" --no-setup \
			>"$mainflow_root/explicit.out" 2>"$mainflow_root/explicit.err" || mainflow_rc=$?

		assert_eq "main-flow: explicit --no-setup upgrade proceeds" "0" "$mainflow_rc"
		mainflow_swapped_output=$("$mainflow_explicit_to/agentchute")
		assert_eq "main-flow: explicit --no-setup upgrade swapped the binary" \
			"fake-agentchute-test-fixture" "$mainflow_swapped_output"
	else
		printf 'SKIP  main-flow: no-tty upgrade scenarios (this shell has a controlling terminal)\n'
	fi

	# Scenario D: a stale agentchute earlier on PATH than the install
	# target — must warn (not fail) that PATH resolves elsewhere.
	mainflow_shadow_stale="$mainflow_root/shadow-stale-path"
	mainflow_shadow_to="$mainflow_root/shadow-to"
	mkdir -p "$mainflow_shadow_stale" "$mainflow_shadow_to"
	printf '#!/bin/sh\necho stale\n' >"$mainflow_shadow_stale/agentchute"
	chmod +x "$mainflow_shadow_stale/agentchute"

	mainflow_rc=0
	AGENTCHUTE_INSTALL_LIB=0 AGENTCHUTE_BASE_URL="$mainflow_base" HOME="$mainflow_root/shadow-home" AGENTCHUTE_NO_PATH_UPDATE=1 \
		PATH="$mainflow_shadow_stale:$PATH" \
		sh install.sh --version "$mainflow_ver" --to "$mainflow_shadow_to" --no-setup \
		>"$mainflow_root/shadow.out" 2>"$mainflow_root/shadow.err" || mainflow_rc=$?

	assert_eq "main-flow: PATH-shadow scenario still exits 0 (warn, not fail)" "0" "$mainflow_rc"
	assert_true "main-flow: warns when PATH resolves a different agentchute" \
		"grep -qi 'not the just-installed' \"$mainflow_root/shadow.err\""
fi

kill "$mainflow_server_pid" 2>/dev/null || true
wait "$mainflow_server_pid" 2>/dev/null || true
rm -rf "$mainflow_root"

# -----------------------------------------------------------------------
# summary
# -----------------------------------------------------------------------

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" = "0" ]
