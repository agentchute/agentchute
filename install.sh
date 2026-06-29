#!/bin/sh
# agentchute install — fetches the latest (or pinned) release binary from
# GitHub releases, verifies its SHA256, and installs it. Optionally runs
# `agentchute setup` against the current directory when a terminal is
# available.
#
# Usage:
#   sh install.sh [--version VERSION] [--to DIR] [--setup|--no-setup] [--wake MODE] [--dry-run]
#   curl -fsSL https://raw.githubusercontent.com/agentchute/agentchute/main/install.sh | sh
#
# Equivalent env vars (flags win on conflict):
#   AGENTCHUTE_VERSION       pin a specific tag (default: latest release)
#   AGENTCHUTE_INSTALL_DIR   override install dir (default: ~/.local/bin)
#   AGENTCHUTE_SHIM_DIR      override launcher shim dir used by setup (default: ~/.agentchute/bin)
#   AGENTCHUTE_PROFILE       shell profile to update when PATH needs entries
#   AGENTCHUTE_NO_PATH_UPDATE=1  do not update shell profile; print hints only
#   AGENTCHUTE_SETUP=0       skip setup after install
#   AGENTCHUTE_WAKE          pass --wake to setup (runner[,tmux][,herdr] | all)
#   AGENTCHUTE_WRAPPERS      pass --wrappers to setup (default: all)
#   AGENTCHUTE_DRY_RUN=1     print the plan and exit; no mutation
#
# Security: this script verifies release checksums; piping the installer
# still trusts this GitHub repository. To inspect before running:
#   curl -fsSLO https://raw.githubusercontent.com/agentchute/agentchute/main/install.sh
#   less install.sh
#   sh install.sh

set -eu

REPO_OWNER="agentchute"
REPO_NAME="agentchute"
GITHUB="https://github.com/${REPO_OWNER}/${REPO_NAME}"

# Source guard: when an outer test harness sources this file, it sets
# AGENTCHUTE_INSTALL_LIB=1 to load helpers without running main.
AGENTCHUTE_INSTALL_LIB="${AGENTCHUTE_INSTALL_LIB:-0}"

# ---------- logging ----------

info() { printf '%s\n' "$*" >&2; }
warn() { printf 'warning: %s\n' "$*" >&2; }
err()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# ---------- validation helpers ----------

# is_valid_version returns 0 if the string looks like an agentchute release tag.
# Conservative pattern (per codex): leading `v`, then digits/letters/dots/hyphens/underscores.
# Rejects empty, slashes, whitespace, shell metacharacters.
is_valid_version() {
	case "$1" in
		v[0-9]*) ;;
		*) return 1 ;;
	esac
	# Reject anything outside the allowed character set.
	stripped=$(printf '%s' "$1" | tr -d 'A-Za-z0-9._-')
	[ -z "$stripped" ]
}

# is_valid_install_dir rejects empty, PATH-hostile, and shell-active values (colon, newline, quotes, dollar signs, backticks, backslashes).
is_valid_install_dir() {
	case "$1" in
		'') return 1 ;;
		*:*) return 1 ;;
		*'"'*) return 1 ;;
		*'$'*) return 1 ;;
		*'`'*) return 1 ;;
		*\\*) return 1 ;;
		*'
'*) return 1 ;;
	esac
	return 0
}

# ---------- platform detection ----------

# detect_os prints darwin or linux on success, fails otherwise. macOS arm64 +
# amd64, Linux amd64 + arm64. Windows refused with WSL hint.
detect_os() {
	uname_s=$(uname -s 2>/dev/null || echo unknown)
	case "$uname_s" in
		Darwin) printf 'darwin' ;;
		Linux)  printf 'linux' ;;
		MINGW*|MSYS*|CYGWIN*)
			err "Windows is not supported natively; use WSL (the Linux binary works there)."
			;;
		*) err "unsupported OS: $uname_s" ;;
	esac
}

# detect_arch prints amd64 or arm64 on success, fails otherwise.
detect_arch() {
	uname_m=$(uname -m 2>/dev/null || echo unknown)
	case "$uname_m" in
		x86_64|amd64)        printf 'amd64' ;;
		aarch64|arm64)       printf 'arm64' ;;
		*) err "unsupported arch: $uname_m" ;;
	esac
}

# ---------- dependency probe ----------

# require_cmd aborts if cmd is missing from PATH.
require_cmd() {
	command -v "$1" >/dev/null 2>&1 || err "missing required dependency: $1"
}

# pick_sha256 prints the path to a working SHA256 utility (sha256sum on Linux,
# shasum on macOS), or aborts if neither is present.
pick_sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		printf 'sha256sum'
	elif command -v shasum >/dev/null 2>&1; then
		printf 'shasum -a 256'
	else
		err "missing required dependency: sha256sum or shasum"
	fi
}

# probe_deps aborts unless every dependency the script will use is present.
probe_deps() {
	require_cmd curl
	require_cmd uname
	require_cmd tar
	require_cmd mktemp
	require_cmd mkdir
	require_cmd mv
	require_cmd chmod
	# sha256/shasum verified via pick_sha256 later.
}

# ---------- version resolution ----------

# extract_tag_from_url parses the final URL of /releases/latest after redirect.
# Format: https://github.com/<owner>/<repo>/releases/tag/<tag>. Returns the tag
# or empty if the URL shape is unexpected.
extract_tag_from_url() {
	url="$1"
	expected_prefix="${GITHUB}/releases/tag/"
	case "$url" in
		"${expected_prefix}"*)
			printf '%s' "${url#"${expected_prefix}"}"
			;;
		*) ;; # unexpected; caller checks for empty
	esac
}

# resolve_latest_version calls GitHub's /releases/latest redirect and extracts
# the tag from the final URL. No JSON, no jq.
resolve_latest_version() {
	url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "${GITHUB}/releases/latest")
	tag=$(extract_tag_from_url "$url")
	if [ -z "$tag" ]; then
		err "could not resolve latest release URL: $url"
	fi
	if ! is_valid_version "$tag"; then
		err "resolved release tag $tag does not match expected v… form"
	fi
	printf '%s' "$tag"
}

# ---------- checksum verify ----------

# checksum_line_for prints the canonical "hash  filename" line for the given
# filename from a checksums.txt file. Pure-sh `while read` exact-match (per
# codex: no grep substring).
checksum_line_for() {
	checksums_path="$1"
	target_name="$2"
	# `while read` doesn't see the last unterminated line on some shells; the
	# `|| [ -n "$line" ]` guard handles that edge case.
	while IFS= read -r line || [ -n "$line" ]; do
		# Each line in checksums.txt is "<hash><whitespace><filename>".
		# Strip leading hash and whitespace; compare what's left exactly to target.
		hash=${line%%[[:space:]]*}
		file=${line#"$hash"}
		# Trim leading whitespace and a single '*' (binary marker some tools emit).
		file=${file#"${file%%[![:space:]]*}"}
		file=${file#\*}
		if [ "$file" = "$target_name" ]; then
			printf '%s' "$line"
			return 0
		fi
	done <"$checksums_path"
	return 1
}

# verify_archive validates archive_path against the checksum recorded in
# checksums_path for archive_basename. Aborts on mismatch.
verify_archive() {
	archive_path="$1"
	archive_basename="$2"
	checksums_path="$3"
	sha_cmd="$4"

	expected_line=$(checksum_line_for "$checksums_path" "$archive_basename") || \
		err "no checksum entry for $archive_basename in checksums.txt"

	expected_hash=${expected_line%%[[:space:]]*}
	actual_line=$($sha_cmd "$archive_path")
	actual_hash=${actual_line%%[[:space:]]*}

	if [ "$expected_hash" != "$actual_hash" ]; then
		err "checksum mismatch for $archive_basename
  expected: $expected_hash
  actual:   $actual_hash"
	fi
}

# ---------- install dir ----------

# validate_install_dir runs non-mutating checks on the destination so that
# --dry-run catches obvious problems (missing parent on a custom path,
# unwritable existing dir). Mutation (mkdir) is left to ensure_install_dir.
validate_install_dir() {
	dir="$1"
	is_custom="$2"

	is_valid_install_dir "$dir" || err "invalid install dir: $dir"

	if [ "$is_custom" = "1" ]; then
		# Custom dir: parent MUST already exist; do not silently create deep trees.
		parent=$(dirname -- "$dir")
		[ -d "$parent" ] || err "parent of install dir does not exist: $parent"
	fi
	# If the dir already exists, require it to be writable now (dry-run can
	# catch perm issues before download). If it doesn't exist yet, the
	# parent's writability is what matters; we test that via the mkdir in
	# ensure_install_dir.
	if [ -e "$dir" ] && [ ! -w "$dir" ]; then
		err "cannot write to $dir; pass --to or fix permissions (sudo not supported)"
	fi
}

# ensure_install_dir creates the install dir if missing and verifies the
# final writability check. Called only on the apply path (not dry-run).
ensure_install_dir() {
	dir="$1"
	mkdir -p "$dir" || err "failed to create $dir"
	if ! [ -w "$dir" ]; then
		err "cannot write to $dir; pass --to or fix permissions (sudo not supported)"
	fi
	printf '%s' "$dir"
}

# path_contains_dir returns 0 when dir is already present in the current PATH.
path_contains_dir() {
	install_dir="$1"
	case ":$PATH:" in
		*":$install_dir:"*) return 0 ;;
	esac
	return 1
}

# default_shell_profile prints the shell profile this installer can update.
default_shell_profile() {
	[ -n "${HOME:-}" ] || return 1
	if [ -n "${AGENTCHUTE_PROFILE:-}" ]; then
		printf '%s' "$AGENTCHUTE_PROFILE"
		return 0
	fi
	case "${SHELL:-}" in
		*zsh)  printf '%s' "$HOME/.zshrc" ;;
		*bash)
			if [ "$(uname -s 2>/dev/null || echo unknown)" = "Darwin" ]; then
				printf '%s' "$HOME/.bash_profile"
			else
				printf '%s' "$HOME/.bashrc"
			fi
			;;
		*fish) printf '%s' "$HOME/.config/fish/config.fish" ;;
		*sh)   printf '%s' "$HOME/.profile" ;;
		*)     return 1 ;;
	esac
}

# path_expr_for_profile prints a shell-profile expression for dir. Prefer
# $HOME-relative entries so copied home directories remain portable.
path_expr_for_profile() {
	dir="$1"
	case "$dir" in
		"$HOME"/*) printf '%s/%s' "\$HOME" "${dir#"$HOME"/}" ;;
		*)         printf '%s' "$dir" ;;
	esac
}

# warn_path_missing prints a friendly add-to-PATH hint if install_dir isn't on
# $PATH. No mutation.
warn_path_missing() {
	install_dir="$1"
	path_contains_dir "$install_dir" && return 0
	warn "$install_dir is not on PATH"
	case "${SHELL:-}" in
		*zsh)  rcfile="\$HOME/.zshrc" ;;
		*bash) rcfile="\$HOME/.bashrc" ;;
		*fish) warn "  add: fish_add_path $install_dir"; return 0 ;;
		*)     rcfile="your shell profile" ;;
	esac
	warn "  add to $rcfile:  export PATH=\"$install_dir:\$PATH\""
}

# ensure_path_available makes a best-effort update to the user's next shell.
# The current process PATH cannot be changed for the parent shell, so the
# caller still prints a restart/new-shell instruction.
ensure_path_available() {
	dir="$1"
	label="$2"
	path_contains_dir "$dir" && return 0

	if [ "${AGENTCHUTE_NO_PATH_UPDATE:-0}" = "1" ]; then
		warn_path_missing "$dir"
		return 0
	fi
	case "$dir" in
		*[[:space:]]*)
			warn "$dir is not on PATH and contains whitespace; not editing shell profile automatically"
			warn "  add: export PATH=\"$dir:\$PATH\""
			return 0
			;;
	esac
	profile=$(default_shell_profile) || {
		warn_path_missing "$dir"
		return 0
	}
	profile_dir=$(dirname -- "$profile")
	mkdir -p "$profile_dir" || {
		warn "could not create $profile_dir; add $dir to PATH manually"
		return 0
	}
	expr=$(path_expr_for_profile "$dir")
	begin="# agentchute PATH entry for $label ($expr) begin"
	end="# agentchute PATH entry for $label ($expr) end"
	if [ -f "$profile" ] && grep -F "$begin" "$profile" >/dev/null 2>&1; then
		info "PATH profile entry for $label already present in $profile"
		return 0
	fi
	# Defer to a setup-managed PATH region if one already covers this profile:
	# `agentchute setup` owns a single managed block (markers below) and supersedes
	# install.sh's per-label regions, so don't stack a second block alongside it.
	if [ -f "$profile" ] && grep -F "# >>> agentchute setup PATH >>>" "$profile" >/dev/null 2>&1; then
		info "setup-managed PATH block already present in $profile; skipping install.sh entry for $label"
		return 0
	fi
	# Intentional: the printf formats keep $PATH/$HOME/$PATH[1] literal so they
	# expand in the user's shell at profile-source time, not here (SC2016). The
	# grep-then-append on $profile is sequential, not a concurrent pipeline
	# (SC2094).
	# shellcheck disable=SC2016,SC2094
	{
		printf '\n%s\n' "$begin"
		case "$profile" in
			*config.fish)
				# fish: PATH is a list; prepend only if not already first.
				printf 'if test "$PATH[1]" != %s\n' "$expr"
				printf '    set -gx PATH %s $PATH\n' "$expr"
				printf 'end\n'
				;;
			*)
				printf '%s\n' "case \"\$PATH\" in"
				printf '  "%s:"*) ;;\n' "$expr"
				printf '  *) export PATH="%s:$PATH" ;;\n' "$expr"
				printf 'esac\n'
				;;
		esac
		printf '%s\n' "$end"
	} >>"$profile" || {
		warn "could not update $profile; add $dir to PATH manually"
		return 0
	}
	info "added $label PATH entry to $profile"
}

print_setup_next_steps() {
	info ""
	info "next: run setup from the repo where your agents will coordinate:"
	info "  agentchute setup"
	info "then restart your agents; setup resets local agentchute runtime state"
	info "and clears stale registrations/Herdr names so wrappers re-enroll cleanly"
	info "optional check: agentchute doctor --as <agent-id>"
	info "upgrading from an older release? re-run setup in each control repo to"
	info "refresh ENROLLMENT blocks; setup also migrates a legacy .rehumanlabs/loop"
	info "to .agentchute/loop automatically in safe cases and refuses to auto-merge"
	info "if both hold live state."
}

# ---------- main flow ----------

main() {
	# Save the user's original cwd before any temp-dir work; setup runs here.
	orig_pwd=$(pwd)

	# Defaults pulled from env vars (flags override below).
	version="${AGENTCHUTE_VERSION:-}"
	install_dir="${AGENTCHUTE_INSTALL_DIR:-}"
	shim_dir="${AGENTCHUTE_SHIM_DIR:-}"
	do_setup="${AGENTCHUTE_SETUP:-auto}"
	setup_wake="${AGENTCHUTE_WAKE:-}"
	setup_wrappers="${AGENTCHUTE_WRAPPERS:-all}"
	dry_run=0
	[ "${AGENTCHUTE_DRY_RUN:-0}" = "1" ] && dry_run=1

	# Flag parsing.
	while [ $# -gt 0 ]; do
		case "$1" in
			--version)   shift; version="${1:-}"; [ -n "$version" ] || err "--version requires a value" ;;
			--version=*) version="${1#--version=}" ;;
			--to)        shift; install_dir="${1:-}"; [ -n "$install_dir" ] || err "--to requires a value" ;;
			--to=*)      install_dir="${1#--to=}" ;;
			--setup)     do_setup=1 ;;
			--no-setup)  do_setup=0 ;;
			--wake)      shift; setup_wake="${1:-}"; [ -n "$setup_wake" ] || err "--wake requires a value" ;;
			--wake=*)    setup_wake="${1#--wake=}" ;;
			--wrappers)  shift; setup_wrappers="${1:-}"; [ -n "$setup_wrappers" ] || err "--wrappers requires a value" ;;
			--wrappers=*) setup_wrappers="${1#--wrappers=}" ;;
			--dry-run)   dry_run=1 ;;
			-h|--help)
				cat <<EOF
agentchute install — fetches the latest release binary and installs it.

usage:
  sh install.sh [--version VERSION] [--to DIR] [--setup|--no-setup] [--wake MODE] [--wrappers SET] [--dry-run]

flags:
  --version   pin a specific tag (default: latest release)
  --to DIR    install dir (default: ~/.local/bin)
  --setup     run \`agentchute setup\` after install (default when a tty exists)
  --no-setup  skip setup and print the command to run later
  --wake MODE pass --wake to setup (runner[,tmux][,herdr] | all)
  --wrappers  pass --wrappers to setup (default: all)
  --dry-run   print the plan and exit; no mutation

env vars (flags override):
  AGENTCHUTE_VERSION, AGENTCHUTE_INSTALL_DIR, AGENTCHUTE_SHIM_DIR,
  AGENTCHUTE_PROFILE, AGENTCHUTE_NO_PATH_UPDATE=1,
  AGENTCHUTE_SETUP=0|1|auto, AGENTCHUTE_WAKE, AGENTCHUTE_WRAPPERS,
  AGENTCHUTE_DRY_RUN=1
EOF
				return 0
				;;
			*) err "unknown argument: $1" ;;
		esac
		shift
	done

	probe_deps
	sha_cmd=$(pick_sha256)

	# Resolve OS/arch.
	os=$(detect_os)
	arch=$(detect_arch)

	# Resolve version — flag/env override, else GitHub redirect.
	if [ -n "$version" ]; then
		is_valid_version "$version" || err "invalid version: $version (expected v… form)"
	else
		version=$(resolve_latest_version)
	fi

	# GoReleaser strips the leading `v` from filenames; preserve the tag for URLs.
	bare_version=${version#v}
	archive_basename="agentchute_${bare_version}_${os}_${arch}.tar.gz"
	archive_url="${GITHUB}/releases/download/${version}/${archive_basename}"
	checksums_url="${GITHUB}/releases/download/${version}/checksums.txt"

	# Resolve install dir.
	is_custom=1
	if [ -z "$install_dir" ]; then
		install_dir="${HOME:-}/.local/bin"
		is_custom=0
		[ -n "${HOME:-}" ] || err "HOME unset; pass --to explicitly"
	fi
	# Non-mutating validation runs before the dry-run exit so impossible
	# destinations are caught at plan time (missing parent on a custom path,
	# unwritable existing dir). Mutation (mkdir) happens later.
	validate_install_dir "$install_dir" "$is_custom"

	# Show the plan in a tab-aligned summary (gemini's "indifferent-but-informative" voice).
	cat <<EOF
agentchute install
  version:        $version
  os/arch:        $os/$arch
  download:       $archive_url
  install dir:    $install_dir
  shim dir:       ${shim_dir:-${HOME:-}/.agentchute/bin}
  setup:          $do_setup
  setup wake:     ${setup_wake:-prompt}
  setup wrappers: $setup_wrappers
EOF

	# --dry-run wins over setup (per codex). Still resolves version (network OK,
	# just no mutation).
	if [ "$dry_run" = "1" ]; then
		info ""
		info "(dry-run; no changes made)"
		return 0
	fi

	# Create install dir (mkdir is the mutating step kept out of dry-run).
	install_dir=$(ensure_install_dir "$install_dir")

	# Temp dir with trap cleanup.
	tmpdir=$(mktemp -d 2>/dev/null) || err "mktemp -d failed"
	# shellcheck disable=SC2064
	trap "rm -rf '$tmpdir'" EXIT INT TERM

	# Download archive + checksums.
	info ""
	info "fetching archive..."
	curl -fsSL -o "$tmpdir/$archive_basename" "$archive_url" \
		|| err "download failed: $archive_url"
	info "fetching checksums..."
	curl -fsSL -o "$tmpdir/checksums.txt" "$checksums_url" \
		|| err "download failed: $checksums_url"

	# Verify SHA256 BEFORE extracting (per codex).
	info "verifying SHA256..."
	verify_archive "$tmpdir/$archive_basename" "$archive_basename" "$tmpdir/checksums.txt" "$sha_cmd"
	info "  verified: SHA256 OK"

	# Extract.
	info "extracting..."
	tar -xzf "$tmpdir/$archive_basename" -C "$tmpdir" \
		|| err "extract failed"
	[ -x "$tmpdir/agentchute" ] || err "extracted archive missing executable 'agentchute'"

	# Atomic-ish install: temp file in install_dir (same filesystem), then mv.
	info "installing to $install_dir/agentchute..."
	temp_target="$install_dir/.agentchute.tmp.$$"
	mv "$tmpdir/agentchute" "$temp_target" || err "stage to $temp_target failed"
	chmod +x "$temp_target" || err "chmod failed"
	mv "$temp_target" "$install_dir/agentchute" || err "rename to $install_dir/agentchute failed"

	info ""
	info "installed agentchute $version to $install_dir/agentchute"
	info ""
	info "Security: this verified release checksums; piping the installer still trusts this GitHub repository."

	ensure_path_available "$install_dir" "binary"

	if [ -z "$shim_dir" ]; then
		[ -n "${HOME:-}" ] || err "HOME unset; set AGENTCHUTE_SHIM_DIR explicitly for launcher shims"
		shim_dir="${HOME}/.agentchute/bin"
	fi
	is_valid_install_dir "$shim_dir" || err "invalid shim dir: $shim_dir (must not contain quotes, dollar signs, backticks, or backslashes)"

	# tmux is one peer-wake adapter. Missing tmux only removes that adapter:
	# runner/herdr wake paths and polling-only setups remain valid.
	if ! command -v tmux >/dev/null 2>&1; then
		info ""
		info "note: tmux not found on PATH. The tmux wake adapter won't be"
		info "available, but runner/herdr wake paths and polling-only setups still"
		info "work. See https://agentchute.dev/ \"Running without tmux\" for patterns."
	fi

	if [ "$do_setup" = "auto" ]; then
		if ( : </dev/tty ) 2>/dev/null; then
			do_setup=1
		else
			do_setup=0
		fi
	fi

	if [ "$do_setup" = "1" ]; then
		set -- setup --shim-dir "$shim_dir" --wrappers "$setup_wrappers"
		if [ -n "$setup_wake" ]; then
			set -- "$@" --wake "$setup_wake" --yes
		fi
		if [ "${AGENTCHUTE_NO_PATH_UPDATE:-0}" = "1" ]; then
			set -- "$@" --no-profile
		fi
		if ! ( : </dev/tty ) 2>/dev/null && [ -z "$setup_wake" ]; then
			info ""
			warn "setup needs a tty when --wake is not provided; skipping setup"
			warn "  run \`agentchute setup\` from your control repo"
			return 2
		fi
		info ""
		info "running agentchute setup in $orig_pwd..."
		cd "$orig_pwd"
		"$install_dir/agentchute" "$@"
	else
		print_setup_next_steps
	fi
}

if [ "$AGENTCHUTE_INSTALL_LIB" != "1" ]; then
	main "$@"
fi
