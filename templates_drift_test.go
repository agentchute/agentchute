package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestTemplatesMatchRepoWrappers fails if the rendered ENROLLMENT block from
// templates/enrollment/{wrapper,agents}.md no longer matches the marked block
// in the dev repo's auto-discovered wrapper files. The two must stay in sync:
// the templates are what `agentchute init` writes into a USER's project, and
// the dev repo's wrappers are what brand-new agents read in THIS repo. Drift
// means agents in the two contexts see different protocol instructions.
func TestTemplatesMatchRepoWrappers(t *testing.T) {
	cases := []struct {
		wrapperFile string
		render      func() string
	}{
		{
			wrapperFile: "CLAUDE.md",
			render:      func() string { return renderWrapperBlock("claude-code", "anthropic") },
		},
		{
			wrapperFile: "CODEX.md",
			render:      func() string { return renderWrapperBlock("codex", "openai") },
		},
		{
			wrapperFile: "GEMINI.md",
			render:      func() string { return renderWrapperBlock("gemini-cli", "google") },
		},
		// GROK.md intentionally diverges from the standard
		// wrapper.md template in v0.2.1: `agentchute hooks install`
		// does not yet ship a grok template, so the GROK.md
		// enrollment block points at manual boot instead of
		// promising automation that doesn't exist. Add a grok
		// template + restore this entry when the grok wrapper
		// ships.
		// {wrapperFile: "GROK.md", render: ...},
		{
			wrapperFile: "AGENTS.md",
			render:      func() string { return enrollmentAgentsTemplate },
		},
	}

	markerRE := regexp.MustCompile(`(?s)<!-- agentchute-enrollment v\d+ begin -->.*?<!-- agentchute-enrollment v\d+ end -->`)

	for _, c := range cases {
		raw, err := os.ReadFile(c.wrapperFile)
		if err != nil {
			t.Errorf("read %s: %v", c.wrapperFile, err)
			continue
		}
		extracted := markerRE.FindString(string(raw))
		if extracted == "" {
			t.Errorf("%s: no agentchute-enrollment block found", c.wrapperFile)
			continue
		}
		rendered := strings.TrimRight(c.render(), "\n")
		extracted = strings.TrimRight(extracted, "\n")
		if extracted != rendered {
			t.Errorf("%s drifts from templates/enrollment/* — run `agentchute init` to resync.\n  diff hint: file is %d bytes, template is %d bytes",
				c.wrapperFile, len(extracted), len(rendered))
		}
	}
}
