package loop

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// A planted variable is gone after Strip, and nothing else is touched.
func TestStripTestEnvRemovesEveryAgentchuteVariable(t *testing.T) {
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "/live/pool")
	t.Setenv("AGENTCHUTE_SERVE_TOKEN", "live-token")
	t.Setenv("ACTEST_KEEP", "1")
	if err := StripTestEnv(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"AGENTCHUTE_CONTROL_REPO", "AGENTCHUTE_SERVE_TOKEN"} {
		if _, ok := os.LookupEnv(name); ok {
			t.Fatalf("%s survived Strip", name)
		}
	}
	if os.Getenv("ACTEST_KEEP") != "1" {
		t.Fatal("Strip removed a variable outside the AGENTCHUTE_ namespace")
	}
}

// When a variable cannot be removed, the suite refuses to run.
func TestStripTestEnvRefusesWhenAVariableSurvives(t *testing.T) {
	env := []string{"PATH=/bin", "AGENTCHUTE_CONTROL_REPO=/live/pool", "AGENTCHUTE_AGENT_ID=codex"}
	err := stripTestEnv(func() []string { return env }, func(string) error { return errors.New("read-only environment") })
	if err == nil || !strings.Contains(err.Error(), "refusing to run tests") ||
		!strings.Contains(err.Error(), "AGENTCHUTE_AGENT_ID, AGENTCHUTE_CONTROL_REPO") {
		t.Fatalf("err = %v, want a refusal naming both survivors", err)
	}
}
