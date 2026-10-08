package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func codexSpec(t *testing.T) wrapperSpec {
	t.Helper()
	spec, ok := wrapperSpecForName("codex")
	if !ok {
		t.Fatal("codex wrapper spec missing")
	}
	return spec
}

func claudeSpec(t *testing.T) wrapperSpec {
	t.Helper()
	spec, ok := wrapperSpecForName("claude")
	if !ok {
		t.Fatal("claude wrapper spec missing")
	}
	return spec
}

// probeYes / probeNo stand in for `codex --help` advertising (or not) the flag.
func probeYes(string) bool { return true }
func probeNo(string) bool  { return false }

func TestEnsureCodexNoDaemon_Argv(t *testing.T) {
	codex := codexSpec(t)
	claude := claudeSpec(t)
	rows := []struct {
		name  string
		spec  wrapperSpec
		args  []string
		probe func(string) bool
		want  []string
	}{
		{
			name:  "bare codex gains the flag as a top-level option",
			spec:  codex,
			args:  []string{"/opt/homebrew/bin/codex"},
			probe: probeYes,
			want:  []string{"/opt/homebrew/bin/codex", "--no-daemon"},
		},
		{
			name:  "flag lands before the resume subcommand, after argv0",
			spec:  codex,
			args:  []string{"codex", "--dangerously-bypass-approvals-and-sandbox", "resume"},
			probe: probeYes,
			want:  []string{"codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox", "resume"},
		},
		{
			name:  "flag lands before exec too",
			spec:  codex,
			args:  []string{"codex", "exec", "hello"},
			probe: probeYes,
			want:  []string{"codex", "--no-daemon", "exec", "hello"},
		},
		{
			name:  "operator already passed it: not duplicated",
			spec:  codex,
			args:  []string{"codex", "--dangerously-bypass-approvals-and-sandbox", "--no-daemon", "resume"},
			probe: probeYes,
			want:  []string{"codex", "--dangerously-bypass-approvals-and-sandbox", "--no-daemon", "resume"},
		},
		{
			name:  "operator passed it after the subcommand: still not duplicated",
			spec:  codex,
			args:  []string{"codex", "resume", "--no-daemon"},
			probe: probeYes,
			want:  []string{"codex", "resume", "--no-daemon"},
		},
		{
			name:  "probe says the installed codex lacks the flag: argv untouched",
			spec:  codex,
			args:  []string{"codex", "resume"},
			probe: probeNo,
			want:  []string{"codex", "resume"},
		},
		{
			name:  "other wrappers untouched even when the probe would say yes",
			spec:  claude,
			args:  []string{"claude", "--model", "sonnet"},
			probe: probeYes,
			want:  []string{"claude", "--model", "sonnet"},
		},
		{
			name:  "unknown wrapper untouched",
			spec:  wrapperSpec{},
			args:  []string{"something", "else"},
			probe: probeYes,
			want:  []string{"something", "else"},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			probed := false
			probe := func(bin string) bool {
				probed = true
				if bin != row.args[0] {
					t.Errorf("probe ran against %q, want argv0 %q", bin, row.args[0])
				}
				return row.probe(bin)
			}
			orig := append([]string(nil), row.args...)
			got := ensureCodexNoDaemon(row.spec, row.args, probe)
			if strings.Join(got, "\x00") != strings.Join(row.want, "\x00") {
				t.Fatalf("argv = %q, want %q", got, row.want)
			}
			if strings.Join(row.args, "\x00") != strings.Join(orig, "\x00") {
				t.Fatalf("input argv mutated: %q", row.args)
			}
			wantProbe := row.spec.Key == "codex" && !dispatchHasFlag(row.args[1:], codexNoDaemonFlag)
			if probed != wantProbe {
				t.Fatalf("probe ran = %v, want %v (nothing is cached, and no probe when the flag is present or the wrapper is not codex)", probed, wantProbe)
			}
		})
	}
}

// Both launch forms funnel through serve's wrapper-argv step: `ac serve codex ...`
// (dispatch → serve) and a hand-typed `agentchute serve -- codex ...`.
func TestServeWrapperArgs_CoversDispatchAndHandTypedServe(t *testing.T) {
	restore := codexHelpOutput
	t.Cleanup(func() { codexHelpOutput = restore })
	var probedBin string
	codexHelpOutput = func(bin string) (string, error) {
		probedBin = bin
		return "Usage: codex [OPTIONS] [PROMPT]\n\n      --no-daemon\n          Run without the shared background server\n", nil
	}

	// dispatch layer: `ac serve codex --dangerously-bypass-approvals-and-sandbox resume`
	plan, err := parseDispatch([]string{"serve", "codex", "--dangerously-bypass-approvals-and-sandbox", "resume"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &loop.Config{ControlRepo: "/repo", LoopDir: "/repo/.agentchute/loop"}
	runArgs := buildDispatchRunArgs("/bin/agentchute", plan.Wrapper.Vendor, nil, cfg, append([]string{"/opt/homebrew/bin/codex"}, plan.WrapperArgs...))
	sep := -1
	for i, a := range runArgs {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf("no -- separator in %q", runArgs)
	}
	got, spec := serveWrapperArgs(runArgs[sep+1:])
	want := []string{"/opt/homebrew/bin/codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox", "resume"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("dispatch→serve argv = %q, want %q", got, want)
	}
	if spec.Key != "codex" || probedBin != "/opt/homebrew/bin/codex" {
		t.Fatalf("spec=%q probed=%q", spec.Key, probedBin)
	}

	// hand-typed layer: `agentchute serve -- codex resume`
	probedBin = ""
	got, _ = serveWrapperArgs([]string{"codex", "resume"})
	want = []string{"codex", "--no-daemon", "resume"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("hand-typed serve argv = %q, want %q", got, want)
	}
	if probedBin != "codex" {
		t.Fatalf("probe ran against %q, want the launched binary", probedBin)
	}
}

func TestCodexSupportsNoDaemon_Probe(t *testing.T) {
	restore := codexHelpOutput
	t.Cleanup(func() { codexHelpOutput = restore })

	rows := []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{name: "advertised", out: "Options:\n      --no-daemon\n          Run without the shared background server\n", want: true},
		{name: "older codex without the flag", out: "Options:\n      --model <MODEL>\n", want: false},
		{name: "substring inside another flag does not count", out: "      --no-daemon-ish\n", want: false},
		{name: "help fails", out: "", err: errors.New("exec: not found"), want: false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			codexHelpOutput = func(string) (string, error) { return row.out, row.err }
			if got := codexSupportsNoDaemon("codex"); got != row.want {
				t.Fatalf("codexSupportsNoDaemon = %v, want %v", got, row.want)
			}
		})
	}
}

func TestParseCodexDaemonProcessTable(t *testing.T) {
	table := strings.Join([]string{
		"13818     1 /Users/alex/.codex/bin/codex app-server --listen unix:// --analytics-default-enabled --managed-daemon",
		"13833     1 /Users/alex/.codex/bin/codex app-server daemon pid-update-loop",
		"16007 16006 codex --dangerously-bypass-approvals-and-sandbox",
		"  424     1 /usr/bin/grep codex app-server --managed-daemon", // argv0 is not codex
		"",
	}, "\n")
	got := parseCodexDaemonProcessTable(table)
	if len(got) != 1 || got[0].PID != 13818 {
		t.Fatalf("parsed %+v, want exactly pid 13818", got)
	}
	if !strings.Contains(got[0].Command, "--managed-daemon") {
		t.Fatalf("command not kept: %q", got[0].Command)
	}
}

func TestParseProcessEnv(t *testing.T) {
	// `ps -E -o command=` appends the environment as KEY=VALUE words after the argv.
	line := "/Users/alex/.codex/bin/codex app-server --managed-daemon HOME=/Users/alex AGENTCHUTE_SERVE_TOKEN=d7ab11fe AGENTCHUTE_CONTROL_REPO=/Users/alex/code"
	env := parseProcessEnvWords(line)
	if env["AGENTCHUTE_SERVE_TOKEN"] != "d7ab11fe" || env["AGENTCHUTE_CONTROL_REPO"] != "/Users/alex/code" {
		t.Fatalf("env = %v", env)
	}
	// /proc/<pid>/environ form: NUL-separated.
	env = parseProcessEnvBytes([]byte("AGENTCHUTE_SERVE_TOKEN=abc\x00AGENTCHUTE_CONTROL_REPO=/r\x00PATH=/bin\x00"))
	if env["AGENTCHUTE_SERVE_TOKEN"] != "abc" || env["AGENTCHUTE_CONTROL_REPO"] != "/r" {
		t.Fatalf("environ = %v", env)
	}
}

func writeServeClaim(t *testing.T, cfg *loop.Config, id, token string) {
	t.Helper()
	dir := cfg.AgentStateDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := loop.ServeClaim{ID: id, Host: "h", PID: 1, ServeToken: token, StartedAt: time.Now(), LastSeen: time.Now()}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "serve.claim"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorCodexDaemonEnv(t *testing.T) {
	restore := listCodexDaemons
	t.Cleanup(func() { listCodexDaemons = restore })

	const live = "eccdff90eccdff90eccdff90eccdff90"
	const dead = "d7ab11fed7ab11fed7ab11fed7ab11fe"

	rows := []struct {
		name         string
		daemons      []codexDaemonProcess
		listErr      error
		wantSeverity string
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "no daemon running",
			wantSeverity: severityOK,
			wantContains: []string{"no shared codex app-server daemon"},
		},
		{
			name:         "process table unreadable is a warning, not a failure",
			listErr:      errors.New("ps: boom"),
			wantSeverity: severityWarn,
			wantContains: []string{"could not enumerate", "ps: boom"},
		},
		{
			name: "daemon pinned to this pool's live serve",
			daemons: []codexDaemonProcess{{PID: 13818, Command: "codex app-server --managed-daemon",
				Env: map[string]string{"AGENTCHUTE_SERVE_TOKEN": live, "AGENTCHUTE_CONTROL_REPO": "{repo}"}}},
			wantSeverity: severityOK,
			wantContains: []string{"13818", "eccdff90"},
		},
		{
			name: "token mismatch: daemon carries a fenced serve's token",
			daemons: []codexDaemonProcess{{PID: 13818, Command: "codex app-server --managed-daemon",
				Env: map[string]string{"AGENTCHUTE_SERVE_TOKEN": dead, "AGENTCHUTE_CONTROL_REPO": "{repo}"}}},
			wantSeverity: severityWarn,
			wantContains: []string{"13818", "d7ab11fe", "token mismatch", "--no-daemon", "codex app-server daemon stop"},
			wantAbsent:   []string{dead}, // tokens are shown as a prefix, never in full
		},
		{
			name: "control repo mismatch: daemon belongs to another pool",
			daemons: []codexDaemonProcess{{PID: 13818, Command: "codex app-server --managed-daemon",
				Env: map[string]string{"AGENTCHUTE_SERVE_TOKEN": live, "AGENTCHUTE_CONTROL_REPO": "/Users/alex/code"}}},
			wantSeverity: severityWarn,
			wantContains: []string{"/Users/alex/code", "control repo mismatch"},
		},
		{
			name: "daemon without agentchute env at all: not ours, still a hazard",
			daemons: []codexDaemonProcess{{PID: 13818, Command: "codex app-server --managed-daemon",
				Env: map[string]string{"HOME": "/Users/alex"}}},
			wantSeverity: severityWarn,
			wantContains: []string{"13818", "no AGENTCHUTE_SERVE_TOKEN"},
		},
		{
			name: "env unreadable: warning that names the pid",
			daemons: []codexDaemonProcess{{PID: 13818, Command: "codex app-server --managed-daemon",
				EnvErr: errors.New("permission denied")}},
			wantSeverity: severityWarn,
			wantContains: []string{"13818", "permission denied", "could not read"},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			cfg := newDoctorCfg(t)
			writeServeClaim(t, cfg, "codex", live)
			daemons := make([]codexDaemonProcess, 0, len(row.daemons))
			for _, d := range row.daemons {
				env := map[string]string{}
				for k, v := range d.Env {
					env[k] = strings.ReplaceAll(v, "{repo}", cfg.ControlRepo)
				}
				d.Env = env
				daemons = append(daemons, d)
			}
			listCodexDaemons = func() ([]codexDaemonProcess, error) { return daemons, row.listErr }

			r := runDoctorChecks(cfg, "", doctorOptions{Now: time.Now().UTC()})
			c := findCheck(t, r, "codex_daemon_env")
			if c.Severity != row.wantSeverity {
				t.Fatalf("severity = %s, want %s; message: %s", c.Severity, row.wantSeverity, c.Message)
			}
			for _, want := range row.wantContains {
				if !strings.Contains(c.Message, want) {
					t.Errorf("message lacks %q: %s", want, c.Message)
				}
			}
			for _, absent := range row.wantAbsent {
				if strings.Contains(c.Message, absent) {
					t.Errorf("message leaks %q: %s", absent, c.Message)
				}
			}
			if r.Blockers != 0 {
				t.Fatalf("codex daemon check must never block; blockers=%d", r.Blockers)
			}
		})
	}
}
