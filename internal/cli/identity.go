package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/agentchute/agentchute/internal/loop"
)

const missingAgentIdentityHint = "missing agent identity: pass --as/--from or set AGENTCHUTE_AGENT_ID (e.g. export AGENTCHUTE_AGENT_ID=claude-code). See AGENTS.md enrollment."

func resolveAgentID(flagID string) (string, error) {
	id, err := resolveAgentIDRaw(flagID)
	if err != nil {
		return "", err
	}
	// Structural traversal-safety: every path that produces an agent id flows
	// through this single validation, so a hostile --as / AGENTCHUTE_AGENT_ID
	// (e.g. "../../etc/x") can never escape to filesystem resolution.
	if err := loop.ValidateAgentID(id); err != nil {
		return "", err
	}
	return id, nil
}

func resolveAgentIDRaw(flagID string) (string, error) {
	// 1. Explicit --as flag wins.
	if strings.TrimSpace(flagID) != "" {
		return strings.TrimSpace(flagID), nil
	}

	// 2. AGENTCHUTE_AGENT_ID env var.
	if envID := strings.TrimSpace(os.Getenv("AGENTCHUTE_AGENT_ID")); envID != "" {
		return envID, nil
	}

	return "", fmt.Errorf("%s", missingAgentIdentityHint)
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
	return vendorForAgentID(agentID)
}

func registrationMatchesCanonical(agentID, canon string) bool {
	agentID = strings.TrimSpace(agentID)
	canon = strings.TrimSpace(canon)
	return agentID == canon || strings.HasPrefix(agentID, canon+"-")
}
