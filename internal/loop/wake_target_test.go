package loop

import "testing"

func TestValidateWakeTarget(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		target  string
		wantErr bool
	}{
		// --- live formats that MUST pass ---
		{"tmux pane id %0", "tmux", "%0", false},
		{"tmux pane id %1", "tmux", "%1", false},
		{"tmux pane id multi-digit", "tmux", "%123", false},
		{"tmux session:win.pane", "tmux", "main:0.0", false},
		{"tmux session-with-dash:win.pane", "tmux", "my-session:1.2", false},
		{"tmux session_with_underscore:win.pane", "tmux", "my_session:10.3", false},
		{"herdr agent slug", "herdr", "claude-code-agentchute", false},
		{"herdr agent slug codex", "herdr", "codex-agentchute", false},
		{"herdr agent slug single", "herdr", "grok", false},
		{"runner unix abs path", RunnerWakeMethod, "unix:/Users/alex/code/agentchute/.agentchute/loop/state/grok-agentchute/runner.sock", false},
		{"runner unix tmp path", RunnerWakeMethod, "unix:/tmp/agentchute-run/abc-grok.sock", false},
		{"unknown method permissive", "wezterm", "anything-goes", false},
		{"unknown method permissive symbols", "kitty", "@something/weird:1", false},

		// --- attacks / malformed that MUST fail ---
		{"empty target", "tmux", "", true},
		{"empty herdr target", "herdr", "", true},
		{"empty runner target", RunnerWakeMethod, "", true},
		{"leading dash tmux", "tmux", "-t", true},
		{"leading dash herdr", "herdr", "-rf", true},
		{"leading dash runner", RunnerWakeMethod, "-evil", true},
		{"leading dash unknown method", "wezterm", "-x", true},
		{"tmux foreign malformed pane no percent", "tmux", "0", true},
		{"tmux pane percent then non-digit", "tmux", "%a", true},
		{"tmux pane with space", "tmux", "%0 ; rm -rf", true},
		{"tmux session missing pane", "tmux", "main:0", true},
		{"tmux session with dot only", "tmux", "main:0.x", true},
		{"tmux injection semicolon", "tmux", "%0;reboot", true},
		{"tmux newline", "tmux", "%0\nmalicious", true},
		{"herdr with slash traversal", "herdr", "../../etc/passwd", true},
		{"herdr uppercase", "herdr", "Claude-Code", true},
		{"herdr with colon", "herdr", "agent:pane", true},
		{"herdr newline", "herdr", "agent\nx", true},
		{"runner no unix prefix", RunnerWakeMethod, "/tmp/evil.sock", true},
		{"runner relative path", RunnerWakeMethod, "unix:relative/runner.sock", true},
		{"runner empty path after prefix", RunnerWakeMethod, "unix:", true},
		{"runner newline in path", RunnerWakeMethod, "unix:/tmp/evil\n.sock", true},
		{"runner NUL in path", RunnerWakeMethod, "unix:/tmp/evil\x00.sock", true},
		{"runner non-clean dotdot path", RunnerWakeMethod, "unix:/tmp/../evil.sock", true},
		{"runner non-clean dot path", RunnerWakeMethod, "unix:/a/./b", true},
		{"runner non-clean trailing slash", RunnerWakeMethod, "unix:/tmp/x/", true},
		{"runner non-clean double slash", RunnerWakeMethod, "unix://tmp/x.sock", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWakeTarget(tc.method, tc.target)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateWakeTarget(%q, %q) = nil, want error", tc.method, tc.target)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateWakeTarget(%q, %q) = %v, want nil", tc.method, tc.target, err)
			}
		})
	}
}
