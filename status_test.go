package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestPrintStatusIncludesAgentsAndInboxDepth(t *testing.T) {
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	mustMkdir(t, cfg.AgentsDir())
	mustMkdir(t, cfg.AgentInboxDir("codex"))
	mustWrite(t, filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-32-00-123456Z_from-claude-code_msg-abcd.md"), []byte("hi"))

	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)
	regs := map[string]*loop.Registration{
		"codex": {
			AgentID:     "codex",
			Vendor:      "openai",
			ControlRepo: root,
			WakeMethod:  "tmux",
			WakeTarget:  "%1",
			LastSeen:    now.Add(-2 * time.Minute),
			Status:      loop.StatusActive,
		},
	}

	var out bytes.Buffer
	printStatus(&out, cfg, regs, now)
	text := out.String()
	for _, want := range []string{"control_repo:", "codex", "active", "%1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status output missing %q:\n%s", want, text)
		}
	}
	foundDepth := false
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "codex" && fields[2] == "1" {
			foundDepth = true
		}
	}
	if !foundDepth {
		t.Fatalf("status output missing inbox depth 1 for codex:\n%s", text)
	}
}
