package cli

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
			name:  "daemon-only form `queue`: codex refuses the combination, so skipped",
			spec:  codex,
			args:  []string{"codex", "queue", "list"},
			probe: probeYes,
			want:  []string{"codex", "queue", "list"},
		},
		{
			name:  "daemon-only form `agents`: skipped",
			spec:  codex,
			args:  []string{"codex", "agents"},
			probe: probeYes,
			want:  []string{"codex", "agents"},
		},
		{
			name:  "--remote: skipped",
			spec:  codex,
			args:  []string{"codex", "--remote", "resume"},
			probe: probeYes,
			want:  []string{"codex", "--remote", "resume"},
		},
		{
			name:  "--remote=value: skipped",
			spec:  codex,
			args:  []string{"codex", "--remote=hub"},
			probe: probeYes,
			want:  []string{"codex", "--remote=hub"},
		},
		{
			name:  "a prompt that merely contains the word agents is not the subcommand",
			spec:  codex,
			args:  []string{"codex", "list my agents"},
			probe: probeYes,
			want:  []string{"codex", "--no-daemon", "list my agents"},
		},
		{
			name:  "queue as a later word after the resume subcommand: inserted",
			spec:  codex,
			args:  []string{"codex", "resume", "queue"},
			probe: probeYes,
			want:  []string{"codex", "--no-daemon", "resume", "queue"},
		},
		{
			name:  "queue as a separate prompt word: inserted",
			spec:  codex,
			args:  []string{"codex", "review", "the", "queue"},
			probe: probeYes,
			want:  []string{"codex", "--no-daemon", "review", "the", "queue"},
		},
		{
			name:  "bool flag then agents subcommand: skipped",
			spec:  codex,
			args:  []string{"codex", "--full-auto", "agents"},
			probe: probeYes,
			want:  []string{"codex", "--full-auto", "agents"},
		},
		{
			name:  "agents after -- is a prompt, not the subcommand: inserted",
			spec:  codex,
			args:  []string{"codex", "--", "agents"},
			probe: probeYes,
			want:  []string{"codex", "--no-daemon", "--", "agents"},
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
			wantProbe := row.spec.Key == "codex" && !dispatchHasFlag(row.args[1:], codexNoDaemonFlag) && !codexNoDaemonIncompatible(row.args[1:])
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
	codexHelpOutput = func(bin string, _ []string) (string, error) {
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
	got, spec := serveWrapperArgs(runArgs[sep+1:], nil)
	want := []string{"/opt/homebrew/bin/codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox", "resume"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("dispatch→serve argv = %q, want %q", got, want)
	}
	if spec.Key != "codex" || probedBin != "/opt/homebrew/bin/codex" {
		t.Fatalf("spec=%q probed=%q", spec.Key, probedBin)
	}

	// hand-typed layer: `agentchute serve -- codex resume`
	probedBin = ""
	got, _ = serveWrapperArgs([]string{"codex", "resume"}, nil)
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
			codexHelpOutput = func(string, []string) (string, error) { return row.out, row.err }
			if got := codexSupportsNoDaemon("codex", nil); got != row.want {
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
	// /proc/<pid>/environ form: NUL-separated; a value keeps its spaces.
	env := parseProcessEnvBytes([]byte("AGENTCHUTE_SERVE_TOKEN=abc\x00AGENTCHUTE_CONTROL_REPO=/Users/alex/code/Tmux workflow\x00PATH=/bin\x00"))
	if env["AGENTCHUTE_SERVE_TOKEN"] != "abc" || env["AGENTCHUTE_CONTROL_REPO"] != "/Users/alex/code/Tmux workflow" {
		t.Fatalf("environ = %v", env)
	}
	if _, ok := env["PATH"]; ok {
		t.Fatalf("non-agentchute key kept: %v", env)
	}
	if env := parseProcessEnvBytes([]byte("HOME=/Users/alex\x00")); len(env) != 0 {
		t.Fatalf("env without agentchute keys = %v, want empty (readable, no token)", env)
	}

	// kern.procargs2 form: int32 argc, exec path, NUL padding, argv, then env.
	var buf []byte
	buf = binary.NativeEndian.AppendUint32(buf, 3)
	buf = append(buf, "/Users/alex/.codex/bin/codex\x00\x00\x00\x00"...)
	buf = append(buf, "codex\x00app-server\x00--managed-daemon\x00"...)
	buf = append(buf, "HOME=/Users/alex\x00AGENTCHUTE_SERVE_TOKEN=d7ab11fe\x00AGENTCHUTE_CONTROL_REPO=/Users/alex/code/Tmux workflow\x00\x00"...)
	block, err := parseProcArgs2Env(buf)
	if err != nil {
		t.Fatal(err)
	}
	env = parseProcessEnvBytes(block)
	if env["AGENTCHUTE_SERVE_TOKEN"] != "d7ab11fe" || env["AGENTCHUTE_CONTROL_REPO"] != "/Users/alex/code/Tmux workflow" {
		t.Fatalf("procargs2 env = %v", env)
	}
	if _, err := parseProcArgs2Env([]byte{1, 0}); err == nil {
		t.Fatal("truncated procargs2 buffer accepted")
	}
}

// The real reader against this test's own process: the exact-source path on
// each OS, with a value that contains a space.
func TestReadProcessEnv_Self(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no exact process-environment source on " + runtime.GOOS)
	}
	// Read a child we start with the value we want. The child is this test
	// binary re-executed into the sleep branch below, not `sleep`: macOS hides
	// the environment of platform (restricted) binaries from kern.procargs2,
	// and codex is not one.
	if os.Getenv("ACTEST_SLEEP_CHILD") == "1" {
		time.Sleep(30 * time.Second)
		return
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run", "^TestReadProcessEnv_Self$")
	cmd.Env = append(os.Environ(), "ACTEST_SLEEP_CHILD=1", "AGENTCHUTE_CONTROL_REPO=/Users/alex/code/Tmux workflow", "AGENTCHUTE_SERVE_TOKEN=abc123")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var env map[string]string
	deadline := time.Now().Add(5 * time.Second)
	for {
		env, err = readProcessEnv(cmd.Process.Pid)
		if err == nil && env["AGENTCHUTE_SERVE_TOKEN"] == "abc123" || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if env["AGENTCHUTE_CONTROL_REPO"] != "/Users/alex/code/Tmux workflow" {
		t.Fatalf("control repo with a space = %q", env["AGENTCHUTE_CONTROL_REPO"])
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

	const live = "eccdff90eccdff90eccdff90eccdff90"  // codex's live claim
	const other = "0fa78b310fa78b310fa78b310fa78b31" // codex-l2's live claim
	const dead = "d7ab11fed7ab11fed7ab11fed7ab11fe"  // nobody's
	daemon := func(env map[string]string) []codexDaemonProcess {
		return []codexDaemonProcess{{PID: 13818, Command: "codex app-server --managed-daemon", Env: env}}
	}

	rows := []struct {
		name         string
		agent        string
		daemons      []codexDaemonProcess
		listErr      error
		wantSeverity string
		wantContains []string
	}{
		{
			name: "no daemon running", agent: "codex",
			wantSeverity: severityOK,
			wantContains: []string{"no shared codex app-server daemon"},
		},
		{
			name: "process table unreadable is a warning, not a failure", agent: "codex",
			listErr:      errors.New("ps: boom"),
			wantSeverity: severityWarn,
			wantContains: []string{"could not enumerate", "ps: boom"},
		},
		{
			name: "daemon pinned to this lane's live serve", agent: "codex",
			daemons:      daemon(map[string]string{"AGENTCHUTE_SERVE_TOKEN": live, "AGENTCHUTE_CONTROL_REPO": "{repo}"}),
			wantSeverity: severityOK,
			wantContains: []string{"13818", "token matches codex's live serve"},
		},
		{
			name: "token mismatch: daemon carries a fenced serve's token", agent: "codex",
			daemons:      daemon(map[string]string{"AGENTCHUTE_SERVE_TOKEN": dead, "AGENTCHUTE_CONTROL_REPO": "{repo}"}),
			wantSeverity: severityWarn,
			wantContains: []string{"13818", "token mismatch: not codex's live serve token", "matches no live serve.claim", "--no-daemon", "codex app-server daemon stop"},
		},
		{
			name: "token is another lane's: a false OK before (codex vs codex-l2)", agent: "codex",
			daemons:      daemon(map[string]string{"AGENTCHUTE_SERVE_TOKEN": other, "AGENTCHUTE_CONTROL_REPO": "{repo}"}),
			wantSeverity: severityWarn,
			wantContains: []string{"token matches codex-l2's live serve, not codex's", "token mismatch"},
		},
		{
			name: "the requested lane has no live claim", agent: "codex-l3",
			daemons:      daemon(map[string]string{"AGENTCHUTE_SERVE_TOKEN": live, "AGENTCHUTE_CONTROL_REPO": "{repo}"}),
			wantSeverity: severityWarn,
			wantContains: []string{"codex-l3 has no live serve.claim to match", "token matches codex's live serve"},
		},
		{
			name: "control repo mismatch: daemon belongs to another pool", agent: "codex",
			daemons:      daemon(map[string]string{"AGENTCHUTE_SERVE_TOKEN": live, "AGENTCHUTE_CONTROL_REPO": "/Users/alex/code"}),
			wantSeverity: severityWarn,
			wantContains: []string{"/Users/alex/code", "control repo mismatch"},
		},
		{
			name: "matching token but no control repo var: a false OK before", agent: "codex",
			daemons:      daemon(map[string]string{"AGENTCHUTE_SERVE_TOKEN": live}),
			wantSeverity: severityWarn,
			wantContains: []string{"no AGENTCHUTE_CONTROL_REPO"},
		},
		{
			name: "daemon without agentchute env at all: not ours, still a hazard", agent: "codex",
			daemons:      daemon(map[string]string{"HOME": "/Users/alex"}),
			wantSeverity: severityWarn,
			wantContains: []string{"13818", "no AGENTCHUTE_SERVE_TOKEN", "no AGENTCHUTE_CONTROL_REPO"},
		},
		{
			name: "env unreadable: warning that names the pid", agent: "codex",
			daemons:      []codexDaemonProcess{{PID: 13818, Command: "codex app-server --managed-daemon", EnvErr: errors.New("permission denied")}},
			wantSeverity: severityWarn,
			wantContains: []string{"13818", "permission denied", "could not read"},
		},
		{
			name: "no --as: listed, not verified", agent: "",
			daemons:      daemon(map[string]string{"AGENTCHUTE_SERVE_TOKEN": other, "AGENTCHUTE_CONTROL_REPO": "{repo}"}),
			wantSeverity: severitySkip,
			wantContains: []string{"token matches codex-l2's live serve", "cannot be verified against a specific lane"},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			cfg := newDoctorCfg(t)
			writeServeClaim(t, cfg, "codex", live)
			writeServeClaim(t, cfg, "codex-l2", other)
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

			r := runDoctorChecks(cfg, row.agent, doctorOptions{Now: time.Now().UTC()})
			c := findCheck(t, r, "codex_daemon_env")
			if c.Severity != row.wantSeverity {
				t.Fatalf("severity = %s, want %s; message: %s", c.Severity, row.wantSeverity, c.Message)
			}
			for _, want := range row.wantContains {
				if !strings.Contains(c.Message, want) {
					t.Errorf("message lacks %q: %s", want, c.Message)
				}
			}
			// No token characters, ever: not the full token, not a prefix.
			for _, tok := range []string{live, other, dead} {
				if strings.Contains(c.Message, tok[:4]) {
					t.Errorf("message leaks token characters %q: %s", tok[:4], c.Message)
				}
			}
			if c.Severity == severityBlocker {
				t.Fatal("codex daemon check must never block")
			}
		})
	}
}

// The real probe against a wrapper whose background child inherits stdout:
// without WaitDelay, CombinedOutput waits for that child to close the pipe
// (codex measured 13 s past a 10 s deadline). With it, the probe returns
// shortly after the help process exits, with the output it printed.
func TestCodexHelpProbeDoesNotWaitForInheritedPipes(t *testing.T) {
	script := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 20 &\nprintf '      --no-daemon\\n'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got := codexSupportsNoDaemon(script, os.Environ())
	if took := time.Since(start); took > codexHelpProbeWaitDelay+5*time.Second {
		t.Fatalf("probe took %s; the inherited pipe held it open", took)
	}
	if !got {
		t.Fatal("probe lost the help output that was printed before the pipe was held open")
	}
}

// A duplicate lane is refused at lease admission BEFORE the probe runs: the
// wrapper's --help must never execute for a serve that will not launch.
func TestServeProbesCodexOnlyAfterLeaseAdmission(t *testing.T) {
	root := setupShortRunFixture(t)
	invokedPath := filepath.Join(root, "invoked")
	wrapper := filepath.Join(root, "codex")
	mustWrite(t, wrapper, []byte("#!/bin/sh\nprintf -- \"$*\" > "+shellQuote(invokedPath)+"\n"))
	if err := os.Chmod(wrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := loop.AcquireServeLease(cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loop.ReleaseLease(lease) })

	var serveErr error
	withCwd(t, root, func() {
		serveErr = cmdServe([]string{"--as", "codex", "--control-repo", root, "--loop-dir", filepath.Join(root, ".agentchute", "loop"), "--interval", "5", "--idle-grace", "100ms", "--", wrapper})
	})
	if serveErr == nil {
		t.Fatal("cmdServe succeeded while another live serve owns the id")
	}
	if _, err := os.Stat(invokedPath); !os.IsNotExist(err) {
		t.Fatalf("wrapper was executed (--help probe) before lease admission refused the launch: stat err = %v", err)
	}
}
