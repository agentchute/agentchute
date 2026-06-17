package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func withFakeTmuxTargets(t *testing.T, targets ...string) {
	t.Helper()
	old := tmuxProbeBinary
	path := filepath.Join(t.TempDir(), "tmux")
	var cases strings.Builder
	for _, target := range targets {
		cases.WriteString("  '")
		cases.WriteString(target)
		cases.WriteString("') exit 0 ;;\n")
	}
	script := "#!/bin/sh\n" +
		"target=\"\"\n" +
		"while [ \"$#\" -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"-t\" ]; then shift; target=\"$1\"; fi\n" +
		"  shift || true\n" +
		"done\n" +
		"case \"$target\" in\n" +
		cases.String() +
		"  *) exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tmuxProbeBinary = path
	t.Cleanup(func() {
		tmuxProbeBinary = old
	})
}

func mustWriteCanonicalHook(t *testing.T, root, wrapper string) {
	t.Helper()
	for _, h := range hookWrappers {
		if h.Name != wrapper {
			continue
		}
		data, err := hooksFS.ReadFile(h.Src)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(root, h.Dest), data)
		return
	}
	t.Fatalf("unknown hook wrapper %q", wrapper)
}

func mustWriteFreshPollerHeartbeat(t *testing.T, cfg *loop.Config, agentID string) {
	t.Helper()
	if err := loop.SavePollerHeartbeat(cfg, loop.PollerHeartbeat{
		AgentID:         agentID,
		Method:          "test",
		Host:            "test-host",
		IntervalSeconds: loop.DefaultPollerIntervalSeconds,
		LaunchEnabled:   true,
		LastSeen:        time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}
