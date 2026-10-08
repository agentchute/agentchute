package loop

import (
	"os"
	"testing"
)

// TestMain strips every AGENTCHUTE_* variable before any test runs, so a bare
// `go test` typed in a serve lane cannot reach the live pool (opus-xhigh C4).
func TestMain(m *testing.M) {
	MustStripTestEnv()
	os.Exit(m.Run())
}
