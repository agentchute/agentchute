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
# summary
# -----------------------------------------------------------------------

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" = "0" ]
