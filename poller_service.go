package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/agentchute/agentchute/internal/loop"
)

// This file holds the poller's internal support helpers: the wrapper-invocation
// builder and the read-only inbox scan that decides whether a `poller run` tick
// should wake the wrapper. These helpers were previously shared with the
// standalone poll/scheduler command surfaces that v0.9.0 removed; only the
// poller consumes them now.

// serviceKindScript is the only launch kind the poller emits: an inline-sh
// wrapper invocation run per tick.
const serviceKindScript = "script"

// serviceParams describes a single poller-managed wrapper launch.
type serviceParams struct {
	Kind     string
	AgentID  string
	Vendor   string
	Wrapper  string // command-line tool name (claude/codex/gemini)
	Command  string // operator override for the full wrapper invocation
	Interval int
	Repo     string
	Out      string
	Launch   bool

	ControlRepo string
	LoopDir     string
}

// vendorPresets maps an agent ID to its default vendor + wrapper CLI.
// Operators can override via --vendor / --command. These match the
// established v0.1 enrollment conventions.
var vendorPresets = map[string]struct {
	Vendor     string
	Wrapper    string
	Candidates []string
}{
	"claude-code": {"anthropic", "claude", []string{"claude", "claude-code"}},
	"codex":       {"openai", "codex", []string{"codex"}},
	"gemini-cli":  {"google", "gemini", []string{"gemini", "gemini-cli"}},
	"grok":        {"xai", "grok", []string{"grok"}},
}

func wrapperForVendor(vendor string) string {
	switch strings.TrimSpace(vendor) {
	case "anthropic":
		return "claude"
	case "openai":
		return "codex"
	case "google":
		return "gemini"
	default:
		return ""
	}
}

// wrapperInvocation renders the non-interactive command the poller runs to wake
// a wrapper when work is pending.
//
// The prompt is consumed by shells before reaching the model (poller outer-sh →
// wrapper -p), so every shell-special character (` $ \ " ') must be absent from
// the prompt body or one of the layers will interpret it. Plain text only.
func wrapperInvocation(p serviceParams) string {
	if p.Command != "" {
		return p.Command
	}
	vendor := p.Vendor
	if vendor == "" {
		vendor = "<vendor>" // unreachable: poller refuses launch for an unknown-agent unless --command set
	}
	prompt := fmt.Sprintf(
		"Process agentchute mail. Start with: agentchute boot --as %s --vendor %s (idempotent). Reply to messages using send --reply-to. Do not declare done until your inbox is empty.",
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

// selfPollResult is the poller's per-tick wake decision, returned by
// computeSelfPollResult. ShouldWake is the only field the poller acts on; the
// counts are retained so the decision stays auditable.
type selfPollResult struct {
	Agent          string
	ShouldWake     bool
	NeedsBoot      bool
	UnreadCount    int
	MalformedCount int
}

// computeSelfPollResult is the read-only inbox/ledger scan that decides whether
// a poller tick should wake the wrapper. It is strictly side-effect-free.
func computeSelfPollResult(cfg *loop.Config, agentID string) (selfPollResult, error) {
	// Detect missing self registration / inbox dir BEFORE the listing read. A
	// first-run poller (before the agent has ever booted) needs a wakeable
	// signal — surface needs_boot so the tick drives the wrapper through boot
	// rather than failing and leaving the poller idle forever.
	needsBoot := false
	regPath := cfg.AgentRegistrationPath(agentID)
	if _, statErr := os.Stat(regPath); statErr != nil && os.IsNotExist(statErr) {
		needsBoot = true
	}
	inboxDir := cfg.AgentInboxDir(agentID)
	if _, statErr := os.Stat(inboxDir); statErr != nil && os.IsNotExist(statErr) {
		needsBoot = true
	}

	// Same read paths as pending. ErrInboxMissing (registration exists but the
	// inbox dir doesn't — partial state) is folded into needs_boot so the tick
	// can drive the wrapper through the boot path that recreates the inbox.
	var msgs []loop.Message
	var skipped []string
	if !needsBoot {
		var listErr error
		msgs, skipped, listErr = loop.ListInboxMessagesWithSkipped(inboxDir)
		if listErr != nil {
			if errors.Is(listErr, loop.ErrInboxMissing) {
				needsBoot = true
				msgs, skipped = nil, nil
			} else {
				return selfPollResult{}, fmt.Errorf("list inbox: %w", listErr)
			}
		}
	}
	// Reply obligations are asker-owned only (v0.9.0): the poller must NOT wake
	// merely because this agent is OWED replies (a non-blocking dead-recipient
	// signal, not deliverable inbound work). Wake is driven purely by needs-boot,
	// unread mail, and malformed inbox files.
	return selfPollResult{
		Agent:          agentID,
		ShouldWake:     needsBoot || len(msgs) > 0 || len(skipped) > 0,
		NeedsBoot:      needsBoot,
		UnreadCount:    len(msgs),
		MalformedCount: len(skipped),
	}, nil
}
