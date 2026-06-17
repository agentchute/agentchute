package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupRunnerInstallsAllFourShimsRegardlessOfDetection(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	home := t.TempDir()
	realDir := filepath.Join(t.TempDir(), "real")
	mustMkdir(t, realDir)
	realCodex := filepath.Join(realDir, "codex")
	mustWrite(t, realCodex, []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Chmod(realCodex, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", realDir)
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	profile := filepath.Join(home, ".zshrc")

	withCwd(t, root, func() {
		for i := 0; i < 2; i++ {
			if err := cmdSetup([]string{
				"--wake", "runner",
				"--wrappers", "all",
				"--profile", profile,
				"--yes",
			}); err != nil {
				t.Fatalf("cmdSetup run %d: %v", i+1, err)
			}
		}
	})

	if _, err := os.Stat(filepath.Join(root, "AGENTCHUTE.md")); err != nil {
		t.Fatalf("AGENTCHUTE.md not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".codex", "hooks.json")); err != nil {
		t.Fatalf("codex hooks not installed: %v", err)
	}

	// INVARIANT: In runner mode, all four shims are installed even if only one
	// wrapper is detected on PATH.
	for _, name := range []string{"claude", "claude-code", "codex", "gemini", "gemini-cli", "grok"} {
		if _, err := os.Stat(filepath.Join(home, ".agentchute", "bin", name)); err != nil {
			t.Fatalf("%s shim not installed: %v", name, err)
		}
	}
	data, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(data), setupPathBlockBegin); count != 1 {
		t.Fatalf("profile block count = %d, want 1\n%s", count, data)
	}
}

func TestSetupTmuxDoesNotInstallShims(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")

	withCwd(t, root, func() {
		if err := cmdSetup([]string{"--wake", "tmux", "--wrappers", "none", "--yes"}); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := os.Stat(filepath.Join(root, "AGENTCHUTE.md")); err != nil {
		t.Fatalf("AGENTCHUTE.md not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agentchute", "bin")); !os.IsNotExist(err) {
		t.Fatalf("shim dir should not exist for tmux-only setup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".zshrc")); !os.IsNotExist(err) {
		t.Fatalf("profile should not be written for tmux-only setup: %v", err)
	}
}

func TestSetupClearsExistingLiveRegistrations(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")

	agentsDir := filepath.Join(root, ".agentchute", "loop", "agents")
	mustMkdir(t, agentsDir)
	mustWrite(t, filepath.Join(agentsDir, "codex-agentchute.md"), []byte("---\nagent_id: codex-agentchute\nvendor: openai\ncontrol_repo: "+root+"\nhost: test\nlast_seen: 2026-01-01T00:00:00Z\nstatus: active\n---\n"))
	mustWrite(t, filepath.Join(agentsDir, "codex.example.md"), []byte("tracked example\n"))
	mustWrite(t, filepath.Join(agentsDir, "README.md"), []byte("format reference\n"))

	withCwd(t, root, func() {
		if err := cmdSetup([]string{"--wake", "tmux", "--wrappers", "none", "--yes"}); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := os.Stat(filepath.Join(agentsDir, "codex-agentchute.md")); !os.IsNotExist(err) {
		t.Fatalf("live registration should be cleared by setup: %v", err)
	}
	for _, keep := range []string{"codex.example.md", "README.md"} {
		if _, err := os.Stat(filepath.Join(agentsDir, keep)); err != nil {
			t.Fatalf("%s should be preserved: %v", keep, err)
		}
	}
}

func TestSetupRefreshesExistingEnrollmentBlocks(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")

	stale := "<!-- agentchute-enrollment v10 begin -->\nstale identity instructions\n<!-- agentchute-enrollment v10 end -->\n\nLocal notes.\n"
	mustWrite(t, filepath.Join(root, "CODEX.md"), []byte(stale))

	withCwd(t, root, func() {
		if err := cmdSetup([]string{"--wake", "tmux", "--wrappers", "none", "--yes"}); err != nil {
			t.Fatal(err)
		}
	})

	got, err := os.ReadFile(filepath.Join(root, "CODEX.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, "stale identity instructions") {
		t.Fatalf("setup did not replace stale enrollment block:\n%s", text)
	}
	if !strings.Contains(text, "agentchute-enrollment v12 begin") || !strings.Contains(text, "AGENTCHUTE_AGENT_ID") {
		t.Fatalf("setup did not refresh CODEX.md to v12 env identity guidance:\n%s", text)
	}
	if !strings.Contains(text, "Local notes.") {
		t.Fatalf("setup lost non-enrollment content:\n%s", text)
	}
}

func TestSetupModeSwitchToTmuxRemovesPriorSetupShimsAndProfileBlock(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	home := t.TempDir()
	realDir := filepath.Join(t.TempDir(), "real")
	mustMkdir(t, realDir)
	realCodex := filepath.Join(realDir, "codex")
	mustWrite(t, realCodex, []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Chmod(realCodex, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", realDir)
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	profile := filepath.Join(home, ".zshrc")

	withCwd(t, root, func() {
		if err := cmdSetup([]string{"--wake", "runner", "--wrappers", "all", "--profile", profile, "--yes"}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", filepath.Join(home, ".agentchute", "bin")+string(os.PathListSeparator)+realDir)
		if err := cmdSetup([]string{"--wake", "runner", "--wrappers", "all", "--profile", profile, "--yes"}); err != nil {
			t.Fatal(err)
		}
		if err := cmdSetup([]string{"--wake", "tmux", "--wrappers", "none", "--yes"}); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := os.Stat(filepath.Join(home, ".agentchute", "bin", "codex")); !os.IsNotExist(err) {
		t.Fatalf("codex shim should be removed on tmux switch: %v", err)
	}
	data, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), setupPathBlockBegin) {
		t.Fatalf("profile block should be removed on tmux switch:\n%s", data)
	}
}

func TestSetupRefusesNonProjectInitWithYes(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")

	withCwd(t, root, func() {
		err := cmdSetup([]string{"--wake", "tmux", "--wrappers", "none", "--yes"})
		if err == nil {
			t.Fatal("expected non-project init guard")
		}
		if !strings.Contains(err.Error(), "refusing to initialize non-project directory") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	if _, err := os.Stat(filepath.Join(root, "AGENTCHUTE.md")); !os.IsNotExist(err) {
		t.Fatalf("AGENTCHUTE.md should not be written: %v", err)
	}
}

func TestSetupWrapperNarrowingRemovesDroppedHooksAndShims(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	home := t.TempDir()
	realDir := filepath.Join(t.TempDir(), "real")
	mustMkdir(t, realDir)
	for _, name := range []string{"codex", "gemini"} {
		path := filepath.Join(realDir, name)
		mustWrite(t, path, []byte("#!/bin/sh\nexit 0\n"))
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", realDir)
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	profile := filepath.Join(home, ".zshrc")

	withCwd(t, root, func() {
		if err := cmdSetup([]string{"--wake", "runner", "--wrappers", "codex,gemini-cli", "--profile", profile, "--yes"}); err != nil {
			t.Fatal(err)
		}
		if err := cmdSetup([]string{"--wake", "runner", "--wrappers", "codex", "--profile", profile, "--yes"}); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := os.Stat(filepath.Join(root, ".codex", "hooks.json")); err != nil {
		t.Fatalf("codex hook should remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".gemini", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("gemini hook should be removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agentchute", "bin", "codex")); err != nil {
		t.Fatalf("codex shim should remain: %v", err)
	}
	for _, name := range []string{"gemini", "gemini-cli"} {
		if _, err := os.Stat(filepath.Join(home, ".agentchute", "bin", name)); err != nil {
			t.Fatalf("%s shim should remain (INVARIANT: all four shims in runner mode): %v", name, err)
		}
	}
}
