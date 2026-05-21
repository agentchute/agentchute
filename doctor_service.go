package main

import (
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/agentchute/agentchute/internal/loop"
)

// repoCharRE is the conservative whitelist for --repo. Go's %q wraps
// the path in double quotes, but `$(...)` and backticks expand inside
// double quotes in shell — so without an input-charset gate, an
// operator (or worse, a passed-through CI variable) could inject shell
// at scheduler-tick time. We accept the safe subset of POSIX filename
// characters: letters, digits, `/`, `_`, `.`, `-`, space. Operators
// with paths containing `$` / backtick / `;` / `"` / `'` etc. can use
// --command to bypass our rendering entirely.
var repoCharRE = regexp.MustCompile(`^[A-Za-z0-9_/.\- ]+$`)

// --generate-service emits unit/script files for the preflighted-scheduler
// pattern (round-3 synthesis tier 2): every N seconds, run
// `agentchute self-poll --as <id> --heartbeat`; on rc=2 launch the wrapper.
//
// The generated artifacts are deliberately self-contained inline-sh so the
// operator gets ONE file per init kind. Doctor never installs/loads/starts
// anything itself — that remains an explicit operator action.

const (
	serviceKindLaunchd        = "launchd"
	serviceKindSystemdService = "systemd-service"
	serviceKindSystemdTimer   = "systemd-timer"
	serviceKindScript         = "script"
)

type serviceParams struct {
	Kind     string
	AgentID  string
	Vendor   string
	Wrapper  string // command-line tool name (claude/codex/gemini)
	Command  string // operator override for the full wrapper invocation
	Interval int
	Repo     string
	Out      string
}

// vendorPresets maps an agent ID to its default vendor + wrapper CLI.
// Operators can override via --vendor / --command. These match the
// established v0.1 enrollment conventions.
var vendorPresets = map[string]struct{ Vendor, Wrapper string }{
	"claude-code": {"anthropic", "claude"},
	"codex":       {"openai", "codex"},
	"gemini-cli":  {"google", "gemini"},
}

func handleGenerateService(args []string) error {
	fs := flag.NewFlagSet("doctor --generate-service", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var kind, agentID, vendor, command, repo, out string
	var interval int
	fs.StringVar(&kind, "generate-service", "", "kind: launchd | systemd-service | systemd-timer | script")
	fs.StringVar(&agentID, "as", "", "agent id (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "wrapper vendor (optional; inferred from --as for known agents)")
	fs.StringVar(&command, "command", "", "override the full wrapper invocation (advanced)")
	fs.IntVar(&interval, "interval", 30, "poll interval in seconds")
	fs.StringVar(&repo, "repo", "", "working directory for the service (default: cwd)")
	fs.StringVar(&out, "out", "", "write to file (default: stdout)")
	// Re-declare doctor's other flags so they don't error here. They are
	// ignored in --generate-service mode.
	var ignoreControlRepo, ignoreLoopDir string
	var ignoreJSON bool
	fs.StringVar(&ignoreControlRepo, "control-repo", "", "")
	fs.StringVar(&ignoreLoopDir, "loop-dir", "", "")
	fs.BoolVar(&ignoreJSON, "json", false, "")

	if err := fs.Parse(args); err != nil {
		return doctorUsage(err)
	}
	if fs.NArg() != 0 {
		return doctorUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	params := serviceParams{
		Kind:     kind,
		AgentID:  strings.TrimSpace(firstNonEmpty(agentID, os.Getenv("AGENTCHUTE_AGENT_ID"))),
		Vendor:   strings.TrimSpace(vendor),
		Command:  strings.TrimSpace(command),
		Interval: interval,
		Repo:     strings.TrimSpace(repo),
		Out:      strings.TrimSpace(out),
	}
	return generateService(params)
}

func generateService(p serviceParams) error {
	if p.AgentID == "" {
		return fmt.Errorf("--as is required for --generate-service")
	}
	// Codex review #3 (2026-05-20): validate the agent id BEFORE rendering.
	// The id is interpolated into shell strings and unit-file labels; an
	// unvalidated `bad;id` would emit injection-shaped output.
	if err := loop.ValidateAgentID(p.AgentID); err != nil {
		return err
	}
	switch p.Kind {
	case serviceKindLaunchd, serviceKindSystemdService, serviceKindSystemdTimer, serviceKindScript:
	default:
		return fmt.Errorf("--generate-service: unknown kind %q (want launchd | systemd-service | systemd-timer | script)", p.Kind)
	}
	if p.Interval < 5 {
		return fmt.Errorf("--interval must be >= 5 seconds")
	}
	if p.Repo == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		p.Repo = cwd
	}
	abs, err := filepath.Abs(p.Repo)
	if err != nil {
		return err
	}
	p.Repo = abs
	// Codex re-review #4 (2026-05-20): --repo flows into `cd %q` in the
	// shell tick, and `$(...)` inside Go-`%q` double quotes still
	// command-substitutes. Validate against a strict whitelist.
	if !repoCharRE.MatchString(p.Repo) {
		return fmt.Errorf("--repo %q contains characters not in [A-Za-z0-9_/.- ]; move the repo or use --command", p.Repo)
	}

	if preset, ok := vendorPresets[p.AgentID]; ok {
		if p.Vendor == "" {
			p.Vendor = preset.Vendor
		}
		if p.Wrapper == "" {
			p.Wrapper = preset.Wrapper
		}
	}
	// Codex re-review #3 (2026-05-20): --vendor lands inside the
	// generated prompt; an unvalidated `bad$(rm -rf /)` evaluates in the
	// scheduler shell before the wrapper ever sees the prompt. Validate
	// with the same slug rule as agent_id (lowercase ASCII + dash).
	if p.Vendor != "" {
		if err := loop.ValidateAgentID(p.Vendor); err != nil {
			return fmt.Errorf("--vendor %q must be a shell-safe slug (same rule as agent_id): %w", p.Vendor, err)
		}
	}
	if p.Command == "" {
		if p.Wrapper == "" {
			return fmt.Errorf("cannot infer wrapper command for agent %q; pass --vendor and a known wrapper, or use --command", p.AgentID)
		}
	}

	var content string
	switch p.Kind {
	case serviceKindLaunchd:
		content = renderLaunchdPlist(p)
	case serviceKindSystemdService:
		content = renderSystemdService(p)
	case serviceKindSystemdTimer:
		content = renderSystemdTimer(p)
	case serviceKindScript:
		content = renderScript(p)
	}

	if p.Out == "" {
		_, err := io.WriteString(os.Stdout, content)
		return err
	}
	// Operator picks the path; we don't second-guess. Owner-only by default.
	if err := os.WriteFile(p.Out, []byte(content), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", p.Out)
	return nil
}

// wrapperInvocation returns the inline `<cmd> -p "<prompt>"` (or operator
// override). The prompt is concrete — no placeholders — per codex's
// signoff caveat that generated artifacts must supply concrete vendor /
// agent identity, not template tokens for the model to interpret. The
// prompt leads with `agentchute boot` so the first-run needs_boot path
// from self-poll also lands cleanly (boot is idempotent for existing
// registrations and bootstraps fresh ones).
//
// Codex re-review #2 (2026-05-20): the prompt is consumed by THREE
// shells before reaching the model (scheduler outer-sh → preflight
// inline-sh → wrapper -p). Every shell-special character (` $ \ " ')
// must therefore be absent from the prompt body or one of the layers
// will interpret it. Plain text only.
func wrapperInvocation(p serviceParams) string {
	if p.Command != "" {
		return p.Command
	}
	vendor := p.Vendor
	if vendor == "" {
		vendor = "<vendor>" // unreachable: generateService refuses unknown-agent unless --command set
	}
	prompt := fmt.Sprintf(
		"Process agentchute mail. Start with: agentchute boot --as %s --vendor %s (idempotent). Reply to obligations using send --reply-to or release them using defer --message. Do not declare done until your inbox is empty.",
		p.AgentID, vendor,
	)
	// Each wrapper has its own flag for non-interactive prompt input.
	switch p.Wrapper {
	case "claude":
		return fmt.Sprintf(`claude -p "%s"`, prompt)
	case "codex":
		return fmt.Sprintf(`codex exec "%s"`, prompt)
	case "gemini":
		return fmt.Sprintf(`gemini -p "%s"`, prompt)
	default:
		return fmt.Sprintf(`%s "%s"`, p.Wrapper, prompt)
	}
}

// preflightTick is the shared inline-sh used by every kind.
//
//   - POSIX `mkdir` is the lock (atomic on POSIX). `flock` is not on
//     macOS by default. The trap rmdir releases the lock when the
//     subshell exits.
//   - The whole tick runs in a `(...)` subshell so `exit 0` on the idle
//     path only exits the subshell — `while`-loop script kind survives.
//   - Preflight is `agentchute self-poll --as <id> --heartbeat`; it scans the
//     inbox, writes the poller heartbeat, and exits 2 on needs_boot too, so
//     first-run wake fires the wrapper through to boot. The heartbeat is what
//     lets doctor/gate prove non-tmux recipient polling is alive.
//   - No inner single quotes anywhere in the body. The systemd ExecStart
//     wrapper uses outer single quotes — a single-quoted `trap '...'`
//     would terminate the ExecStart string at the inner quote. So the
//     trap action is double-quoted, and the lock path is a validated
//     ASCII string (agent_id is gated through loop.ValidateAgentID) so
//     it's safe to inline unquoted.
func preflightTick(p serviceParams) string {
	lockDir := fmt.Sprintf("/tmp/agentchute-%s.lock", p.AgentID)
	return fmt.Sprintf(
		`( cd %q || exit 0; agentchute self-poll --as %s --heartbeat --heartbeat-method scheduler --heartbeat-interval %d >/dev/null 2>&1; rc=$?; [ "$rc" -ne 2 ] && exit 0; mkdir %s 2>/dev/null || exit 0; trap "rmdir %s" EXIT; sh -c %q )`,
		p.Repo, p.AgentID, p.Interval, lockDir, lockDir, wrapperInvocation(p),
	)
}

func renderLaunchdPlist(p serviceParams) string {
	label := fmt.Sprintf("com.agentchute.preflight.%s", p.AgentID)
	logPath := fmt.Sprintf("/tmp/agentchute-%s.log", p.AgentID)
	// XML-escape every shell-shaped value that lands inside a plist
	// <string>. The preflight line in particular contains `2>&1` and
	// `>/dev/null`; the `&` is a hard XML error if left raw.
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + html.EscapeString(label) + `</string>
    <key>WorkingDirectory</key>
    <string>` + html.EscapeString(p.Repo) + `</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    </dict>
    <key>ProgramArguments</key>
    <array>
        <string>/bin/sh</string>
        <string>-c</string>
        <string>` + html.EscapeString(preflightTick(p)) + `</string>
    </array>
    <key>StartInterval</key>
    <integer>` + fmt.Sprint(p.Interval) + `</integer>
    <key>StandardOutPath</key>
    <string>` + html.EscapeString(logPath) + `</string>
    <key>StandardErrorPath</key>
    <string>` + html.EscapeString(logPath) + `</string>
</dict>
</plist>
`
}

func renderSystemdService(p serviceParams) string {
	return `[Unit]
Description=agentchute preflighted scheduler for ` + p.AgentID + `
After=network.target

[Service]
Type=oneshot
WorkingDirectory=` + p.Repo + `
Environment=PATH=/usr/local/bin:/usr/bin:/bin
ExecStart=/bin/sh -c '` + preflightTick(p) + `'
`
}

func renderSystemdTimer(p serviceParams) string {
	return `[Unit]
Description=agentchute preflighted scheduler timer for ` + p.AgentID + `

[Timer]
OnBootSec=` + fmt.Sprint(p.Interval) + `s
OnUnitActiveSec=` + fmt.Sprint(p.Interval) + `s
Unit=agentchute-` + p.AgentID + `.service

[Install]
WantedBy=timers.target
`
}

func renderScript(p serviceParams) string {
	return `#!/bin/sh
# agentchute preflighted scheduler for ` + p.AgentID + `.
# Run in a long-lived process (cron @reboot, tmux pane, manual sh) — the
# script loops itself. Preflight via self-poll writes a poller heartbeat;
# wrapper only launches when work exists. Single-flight via POSIX mkdir lock.
#
# No 'set -e': agentchute self-poll intentionally exits 2 when work
# exists, and the script needs to keep looping past every tick
# regardless of how any subcommand exits.
INTERVAL=` + fmt.Sprint(p.Interval) + `
while true; do
    ` + preflightTick(p) + `
    sleep "$INTERVAL"
done
`
}
