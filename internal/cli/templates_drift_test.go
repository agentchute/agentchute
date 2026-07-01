package cli

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
		// GROK.md tracks the standard wrapper.md template like every
		// other wrapper. The grok CLI has no repo hook system, so its
		// wake path is the runner shim (`agentchute setup --wake runner
		// --wrappers grok`) rather than lifecycle hooks — but the
		// enrollment block itself is identical to the shared template,
		// so it must not drift.
		{
			wrapperFile: "GROK.md",
			render:      func() string { return renderWrapperBlock("grok", "xai") },
		},
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
