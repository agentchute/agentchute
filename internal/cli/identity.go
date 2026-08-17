package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
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

// resolveAgentVendor keeps its shipped signature and behavior: an explicit
// vendor wins, otherwise fall back to the agent's existing registration row and
// then to the canonical-id table. Those two fallbacks are now the seam's
// (op.ResolveVendor), which is the same resolution the hub performs for a nil
// RegisterReq.Vendor — one canonical-id table, not two that can drift.
func resolveAgentVendor(vendor, agentID string, cfg *loop.Config) string {
	if strings.TrimSpace(vendor) != "" {
		return strings.TrimSpace(vendor)
	}
	return op.ResolveVendor(cfg, agentID)
}

// registrationMatchesCanonical keeps its shipped name and signature for its
// wrapper/hook-identity callers (doctor.go, setup_reset.go) and delegates to the
// seam, so the canonical-id rule has ONE definition rather than a verbatim twin
// per package.
func registrationMatchesCanonical(agentID, canon string) bool {
	return op.MatchesCanonicalID(agentID, canon)
}
