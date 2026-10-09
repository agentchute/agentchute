package op

import (
	"os"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// TestMain strips every AGENTCHUTE_* variable before any test runs, so a bare
// `go test` typed in a serve lane cannot reach the live pool (opus-xhigh C4).
func TestMain(m *testing.M) {
	loop.MustStripTestEnv()
	os.Exit(m.Run())
}
