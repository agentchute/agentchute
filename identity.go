package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func resolveAgentID(flagID, vendor string, cfg *loop.Config) (string, error) {
	// 1. Explicit --as flag wins.
	if strings.TrimSpace(flagID) != "" {
		return strings.TrimSpace(flagID), nil
	}

	// 2. AGENTCHUTE_AGENT_ID env var.
	if envID := strings.TrimSpace(os.Getenv("AGENTCHUTE_AGENT_ID")); envID != "" {
		return envID, nil
	}

	// 3. A tmux wake runs inside the target pane. If that pane already has
	// one live registration in this pool, reuse it so bracketed wake prompts
	// can simply say "check inbox" without an exported identity variable.
	if cfg != nil {
		// A herdr wake injects only "check inbox" (no identity env), so a
		// re-launched / woken pane must map back to ITS registration by
		// resolving the registered herdr name to the current HERDR_PANE_ID.
		// Without this, a second same-wrapper pane (codex-agentchute-2) would
		// fall through to the contextual default and split its inbox.
		if id, ok := agentIDForCurrentHerdrPane(cfg, vendor); ok {
			return id, nil
		}
		if id, ok := agentIDForCurrentTmuxPane(cfg, vendor); ok {
			return id, nil
		}
	}

	// 4. Contextual default: <canonical-wrapper-id>-<folder-slug>.
	canon := canonicalAgentIDForVendor(vendor)
	if canon == "" {
		return "", fmt.Errorf("missing agent identity; pass --as, set AGENTCHUTE_AGENT_ID, run from a registered tmux/herdr pane, or provide a recognized --vendor/--wrapper for a contextual default")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	baseID := canon + "-" + getFolderSlug(cwd)

	// If we don't have a config yet (e.g. discovery failed elsewhere),
	// we can't do conflict detection. Just return the base.
	if cfg == nil {
		return baseID, nil
	}
	return availableContextualAgentID(cfg, baseID, time.Now().UTC()), nil
}

func resolveRegisteredAgentID(flagID string, cfg *loop.Config) (string, error) {
	id, err := resolveAgentID(flagID, "", cfg)
	if err != nil {
		return "", fmt.Errorf("missing agent identity; pass --as, set AGENTCHUTE_AGENT_ID, or run from a registered tmux/herdr pane")
	}
	return id, nil
}

func contextualIdentityBase(flagID, vendor string) (string, bool, error) {
	if strings.TrimSpace(flagID) != "" || strings.TrimSpace(os.Getenv("AGENTCHUTE_AGENT_ID")) != "" {
		return "", false, nil
	}
	canon := canonicalAgentIDForVendor(vendor)
	if canon == "" {
		return "", false, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", false, err
	}
	return canon + "-" + getFolderSlug(cwd), true, nil
}

func agentIDForCurrentTmuxPane(cfg *loop.Config, vendor string) (string, bool) {
	currentPane := strings.TrimSpace(os.Getenv("TMUX_PANE"))
	if currentPane == "" {
		return "", false
	}
	localHost, _ := os.Hostname()
	localHost = strings.TrimSpace(localHost)
	canon := canonicalAgentIDForVendor(vendor)
	regs, _ := loop.ReadRegistrationsLenient(cfg.AgentsDir())
	for _, reg := range regs {
		if strings.TrimSpace(reg.WakeMethod) != "tmux" || strings.TrimSpace(reg.WakeTarget) != currentPane {
			continue
		}
		if localHost != "" && strings.TrimSpace(reg.Host) != "" && reg.Host != localHost {
			continue
		}
		if reg.Status == loop.StatusOffline || reg.Status == loop.StatusExhausted {
			continue
		}
		if canon != "" && !registrationMatchesCanonical(reg.AgentID, canon) {
			continue
		}
		return reg.AgentID, true
	}
	return "", false
}

func agentIDForCurrentHerdrPane(cfg *loop.Config, vendor string) (string, bool) {
	if !herdrEnvActive() || !herdrAvailable() {
		return "", false
	}
	pane := currentHerdrPane()
	localHost, _ := os.Hostname()
	localHost = strings.TrimSpace(localHost)
	canon := canonicalAgentIDForVendor(vendor)
	regs, _ := loop.ReadRegistrationsLenient(cfg.AgentsDir())
	for _, reg := range regs {
		if strings.TrimSpace(reg.WakeMethod) != "herdr" {
			continue
		}
		if localHost != "" && strings.TrimSpace(reg.Host) != "" && reg.Host != localHost {
			continue
		}
		if reg.Status == loop.StatusOffline || reg.Status == loop.StatusExhausted {
			continue
		}
		if canon != "" && !registrationMatchesCanonical(reg.AgentID, canon) {
			continue
		}
		// The wake_target is the stable herdr name; adopt this registration
		// only if that name currently resolves to OUR pane.
		if herdrAgentLookup(strings.TrimSpace(reg.WakeTarget)).PaneID == pane {
			return reg.AgentID, true
		}
	}
	return "", false
}

func availableContextualAgentID(cfg *loop.Config, baseID string, now time.Time) string {
	regs, _ := loop.ReadRegistrationsLenient(cfg.AgentsDir())
	reserved := make(map[string]bool)
	for _, reg := range regs {
		if registrationReservesIdentity(cfg, reg, now) {
			reserved[reg.AgentID] = true
		}
	}

	candidate := baseID
	for i := 2; ; i++ {
		if !reserved[candidate] {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", baseID, i)
		if i > 100 {
			return candidate
		}
	}
}

func canonicalAgentIDForVendor(vendor string) string {
	v := strings.ToLower(strings.TrimSpace(vendor))
	switch v {
	case "anthropic", "claude", "claude-code":
		return "claude-code"
	case "openai", "codex":
		return "codex"
	case "google", "gemini", "gemini-cli":
		return "gemini-cli"
	case "xai", "grok":
		return "grok"
	default:
		return ""
	}
}

func vendorForAgentID(agentID string) string {
	switch {
	case registrationMatchesCanonical(agentID, "claude-code"):
		return "anthropic"
	case registrationMatchesCanonical(agentID, "codex"):
		return "openai"
	case registrationMatchesCanonical(agentID, "gemini-cli"):
		return "google"
	case registrationMatchesCanonical(agentID, "grok"):
		return "xai"
	default:
		return ""
	}
}

func resolveAgentVendor(vendor, agentID string, cfg *loop.Config) string {
	if strings.TrimSpace(vendor) != "" {
		return strings.TrimSpace(vendor)
	}
	if cfg != nil {
		reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath(agentID))
		if err == nil && strings.TrimSpace(reg.Vendor) != "" {
			return strings.TrimSpace(reg.Vendor)
		}
	}
	if preset, ok := vendorPresets[agentID]; ok {
		return preset.Vendor
	}
	return vendorForAgentID(agentID)
}

func getFolderSlug(cwd string) string {
	root, err := gitRootForCwd(cwd)
	if err != nil {
		root = cwd
	}
	slug := slugify(filepath.Base(root))
	if slug == "" {
		return "repo"
	}
	return slug
}

func gitRootForCwd(cwd string) (string, error) {
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("git root empty")
	}
	return root, nil
}

var slugifyRE = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = slugifyRE.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

func registrationMatchesCanonical(agentID, canon string) bool {
	agentID = strings.TrimSpace(agentID)
	canon = strings.TrimSpace(canon)
	return agentID == canon || strings.HasPrefix(agentID, canon+"-")
}

func registrationReservesIdentity(cfg *loop.Config, reg *loop.Registration, now time.Time) bool {
	if reg == nil || strings.TrimSpace(reg.AgentID) == "" {
		return false
	}
	if reg.RestartAt != nil && reg.RestartAt.After(now) {
		return true
	}
	if reg.Status == loop.StatusOffline || reg.Status == loop.StatusExhausted {
		return false
	}
	if !reg.IsPokable() {
		// A poller heartbeat proves recipient liveness, but it does not identify a
		// distinct interactive lane. Reserving poller-only registrations makes
		// hook-managed no-tmux sessions suffix on every lifecycle command.
		return false
	}
	if reg.LastSeen.IsZero() {
		return true
	}
	age := now.Sub(reg.LastSeen.UTC())
	if age < 0 {
		age = 0
	}
	return age < StaleRegThreshold
}
