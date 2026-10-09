package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGuardHubShellDifferentials(t *testing.T) {
	cases := []struct{ name, text string }{
		{"plain", "agentchute hub join"},
		{"quote_split", `a"gent"chute h'u'b j"oi"n`},
		{"word_continuation", "agent\\\nchute hub jo\\\nin"},
		{"quoted_continuation", "\"agent\\\nchute\" hub join"},
		{"brace_words", `{agentchute,hub,join}`},
		{"brace_empty", `{agentchute,} hub join`},
		{"brace_letters", `agent{chute,x} hub join`},
		{"brace_single_zsh", `agentchu{te} hub join`},
		{"ansi_hex", `$'\x61gentchute' hub join`},
		{"ansi_octal", `$'\141gentchute' hub join`},
		{"env_split", `env -S 'agentchute hub join'`},
		{"read_only_env_split", `env -S 'printf %s agentchute hub join'`},
		{"read_only_command_lookup", `command -v agentchute hub join`},
		{"read_only_command_verbose_lookup", `command -V agentchute hub join`},
		{"read_only_quoted_braces", `printf '%s\n' '{agentchute,hub,join}'`},
		{"find_exec", `find . -exec agentchute hub join \;`},
		{"env_split_attached", `env -S'agentchute hub join'`},
		{"command", `command agentchute hub join`},
		{"exec", `exec agentchute hub join`},
		{"xargs", `printf 'x\n' | xargs agentchute hub join`},
		{"nohup", `nohup agentchute hub join`},
		{"time", `time agentchute hub join`},
		{"quoted_semicolon", `printf '%s\n' 'data; agentchute hub join'`},
		{"quoted_and", `printf '%s\n' 'data && agentchute hub join'`},
		{"quoted_pipe", `printf '%s\n' 'data | agentchute hub join'`},
		{"quoted_newline", "printf '%s\\n' 'data\nagentchute hub join'"},
		{"heredoc_same_line", "cat <<'EOF'; agentchute hub join\ndata\nEOF\n"},
		{"heredoc_expansion", "cat <<EOF\n$(agentchute hub join)\nEOF\n"},
		{"heredoc_inert", "cat <<'EOF'\nagentchute hub join\nEOF\n"},
		{"heredoc_then_command", "cat <<'EOF'\ndata\nEOF\nagentchute hub join"},
		{"redirect_expansion", `echo data >"$(agentchute hub join)"`},
		{"process_substitution", `cat <(agentchute hub join)`},
		{"redirect_process_substitution", `cat < <(agentchute hub join)`},
		{"doublequote_substitution", `echo "$(printf '%s' ')'; agentchute hub join)"`},
		{"read_only_env", `env printf '%s\n' agentchute hub join`},
		{"read_only_command", `command printf '%s\n' agentchute hub join`},
		{"read_only_env_word", `printf '%s\n' env -S 'agentchute hub join'`},
		{"unicode_lookalike", `аgentchute hub join`},
	}
	for _, shell := range []string{"/bin/bash", "/bin/zsh", "/bin/sh"} {
		if _, err := os.Stat(shell); err != nil {
			continue
		}
		for _, tc := range cases {
			t.Run(filepath.Base(shell)+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				record := filepath.Join(dir, "record")
				stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$REVIEW_RECORD\"\nprintf fixture-output\n"
				if err := os.WriteFile(filepath.Join(dir, "agentchute"), []byte(stub), 0700); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, shell, "-c", tc.text)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "REVIEW_RECORD="+record)
				out, err := cmd.CombinedOutput()
				data, _ := os.ReadFile(record)
				invoked := strings.HasPrefix(string(data), "hub join")
				denied := guardCommandDenied("Bash " + tc.text)
				t.Logf("input=%q denied=%v invoked=%v shell_error=%v output=%q", tc.text, denied, invoked, err, out)
				if invoked && !denied {
					t.Error("FAIL-OPEN: shell invoked fixture hub command")
				}
				if !invoked && denied && strings.HasPrefix(tc.name, "read_only") {
					t.Error("FALSE DENIAL: shell only printed data")
				}
			})
		}
	}
}

func TestGuardHubLargeInputs(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"wrappers", strings.Repeat("env ", 100000) + "true"},
		{"substitutions", strings.Repeat("echo $(", 10000) + "true" + strings.Repeat(")", 10000)},
		{"quoted_word", "echo " + strings.Repeat("'x'", 100000)},
		{"command_list", strings.Repeat("echo ok;", 100000)},
		{"oversize", strings.Repeat("x", guardHubBudget+1)},
		{"braces", strings.Repeat("{", 100000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Exclude allocating/copying the input from the guard's timing.
			input := "Bash " + tc.text
			// The parsing rows keep a generous bound for race-instrumented CI;
			// oversized input must take the constant-time entry check instead.
			limit := 15 * time.Second
			if tc.name == "oversize" {
				limit = 50 * time.Millisecond
			}
			start := time.Now()
			denied := guardCommandDenied(input)
			elapsed := time.Since(start)
			t.Logf("bytes=%d denied=%v elapsed=%s limit=%s", len(tc.text), denied, elapsed, limit)
			if tc.name == "oversize" && !denied {
				t.Fatal("oversized command was allowed")
			}
			if elapsed > limit {
				t.Fatalf("guard took %s, exceeded %s", elapsed, limit)
			}
		})
	}
}
