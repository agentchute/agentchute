package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleScripts(t *testing.T) {
	scripts := []string{
		"examples/quickstart.sh",
		"examples/three-agents.sh",
		"examples/with-watchdog.sh",
	}

	// Build agentchute binary once for all tests.
	binDir := t.TempDir()
	agentchutePath := filepath.Join(binDir, "agentchute")
	buildCmd := exec.Command("go", "build", "-o", agentchutePath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build agentchute: %v\n%s", err, out)
	}

	for _, scriptPath := range scripts {
		t.Run(scriptPath, func(t *testing.T) {
			root := t.TempDir()

			// Copy AGENTCHUTE.md to the temp root.
			content, err := os.ReadFile("AGENTCHUTE.md")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "AGENTCHUTE.md"), content, 0o644); err != nil {
				t.Fatal(err)
			}

			// Create fake tmux that just logs.
			tmuxLogPath := filepath.Join(root, "tmux.log")
			tmuxPath := filepath.Join(binDir, "tmux")
			fakeTmux := "#!/bin/sh\necho \"tmux $@\" >> " + tmuxLogPath + "\n"
			if err := os.WriteFile(tmuxPath, []byte(fakeTmux), 0o755); err != nil {
				t.Fatal(err)
			}

			// Prepare environment.
			origPath := os.Getenv("PATH")
			newPath := binDir + string(os.PathListSeparator) + origPath

			// Read and modify the script to be test-friendly.
			scriptContent, err := os.ReadFile(scriptPath)
			if err != nil {
				t.Fatal(err)
			}

			// For with-watchdog.sh, we want it to exit and not hang on the background process.
			// We'll append a kill command and also redirect watchdog output to avoid keeping pipes open.
			modifiedScript := string(scriptContent)
			if strings.Contains(modifiedScript, "agentchute watchdog") {
				modifiedScript = strings.ReplaceAll(modifiedScript, "agentchute watchdog --as watchdog &", "agentchute watchdog --as watchdog >/dev/null 2>&1 &")
				modifiedScript += "\nsleep 1\nkill $WATCHDOG_PID || true\n"
			}

			testScriptPath := filepath.Join(root, "run_test.sh")
			if err := os.WriteFile(testScriptPath, []byte(modifiedScript), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("bash", testScriptPath)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+newPath)

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("script %s failed: %v\nOutput:\n%s", scriptPath, err, out)
			}

			// Verify that some tmux commands were logged (if the script pokes).
			if _, err := os.Stat(tmuxLogPath); err != nil {
				if !strings.Contains(string(out), "non-pokable") {
					t.Errorf("tmux.log missing for %s", scriptPath)
				}
			}
		})
	}
}
