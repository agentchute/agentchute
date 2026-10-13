package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// Muse strips AGENTCHUTE_* from hook environments. A private record indexed
// by the runner PID lets descendants recover the same identity and fence,
// without choosing a pool from their working directory.
type serveIdentity struct {
	AgentID     string    `json:"agent_id"`
	Vendor      string    `json:"vendor"`
	ControlRepo string    `json:"control_repo"`
	LoopDir     string    `json:"loop_dir"`
	ServeToken  string    `json:"serve_token"`
	Guarded     bool      `json:"guarded"`
	StartedAt   time.Time `json:"started_at"`
}

func serveIdentityDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agentchute", "serve"), nil
}

func writeServeIdentity(cfg *loop.Config, opts runnerOptions, token string) (func(), error) {
	if len(opts.WrapperArgs) == 0 || filepath.Base(opts.WrapperArgs[0]) != "muse" {
		return func() {}, nil
	}
	dir, err := serveIdentityDir()
	if err != nil {
		return nil, err
	}
	if err := loop.EnsurePrivateDir(dir); err != nil {
		return nil, err
	}
	sweepServeIdentities(dir)
	record := serveIdentity{opts.AgentID, opts.Vendor, cfg.ControlRepo, cfg.LoopDir, token, opts.Guarded, time.Now().UTC()}
	if cfg.Remote != nil {
		record.ControlRepo, record.LoopDir = cfg.Remote.URL, ""
	}
	data, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".identity-")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, strconv.Itoa(os.Getpid())+".json")
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	return func() {
		// A remote relaunch can replace the record in the same process.
		data, err := os.ReadFile(path)
		var current serveIdentity
		if err == nil && json.Unmarshal(data, &current) == nil && current.ServeToken == token {
			_ = os.Remove(path)
		}
	}, nil
}

func sweepServeIdentities(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".json"))
		if err == nil && pid > 0 && !processAlive(pid) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

func museProcess() bool {
	return os.Getenv("MUSE_SESSION_ID") != "" || os.Getenv("MUSE_THREAD_ID") != ""
}

var serveIdentityArgv = readProcessArgv

// Existing environment identity wins unchanged. Only a Muse hook/process
// requires recovery; ordinary hand-run commands retain their explicit-ID path.
func recoverServeIdentity(museHook bool) error {
	if strings.TrimSpace(os.Getenv("AGENTCHUTE_AGENT_ID")) != "" || (!museHook && !museProcess()) {
		return nil
	}
	dir, err := serveIdentityDir()
	if err != nil {
		return fmt.Errorf("%w: recover serve identity: %v", errForeignRunnerEnv, err)
	}
	pid := os.Getpid()
	child, nearestMuse := 0, 0
	for hop := 0; hop < maxAncestryHops; hop++ {
		path := filepath.Join(dir, strconv.Itoa(pid)+".json")
		data, readErr := os.ReadFile(path)
		if readErr == nil {
			// S7 strips identity from nested wrapper launches. Only the Muse
			// directly launched by this serve may recover it; a nested Muse
			// must never commit the parent lane's mail from its Stop hook.
			if nearestMuse == 0 || child != nearestMuse {
				return fmt.Errorf("%w: nearest Muse process is not a direct child of serve %d", errForeignRunnerEnv, pid)
			}
			var record serveIdentity
			if json.Unmarshal(data, &record) != nil || loop.ValidateAgentID(record.AgentID) != nil || record.ServeToken == "" || record.ControlRepo == "" || record.StartedAt.IsZero() || !processAlive(pid) {
				return fmt.Errorf("%w: invalid serve identity for ancestor %d", errForeignRunnerEnv, pid)
			}
			if !strings.HasPrefix(record.ControlRepo, "ssh://") {
				cfg := &loop.Config{ControlRepo: record.ControlRepo, LoopDir: record.LoopDir}
				claim, err := loop.ReadServeClaim(cfg, record.AgentID)
				if err != nil || claim.PID != pid || claim.ServeToken != record.ServeToken || loop.ClaimIsStale(claim, time.Now().UTC()) {
					return fmt.Errorf("%w: stale serve identity for ancestor %d", errForeignRunnerEnv, pid)
				}
			}
			values := map[string]string{
				"AGENTCHUTE_AGENT_ID":         record.AgentID,
				"AGENTCHUTE_CONTROL_REPO":     record.ControlRepo,
				"AGENTCHUTE_LOOP_DIR":         record.LoopDir,
				"AGENTCHUTE_SERVE_TOKEN":      record.ServeToken,
				"AGENTCHUTE_RUNNER_PID":       strconv.Itoa(pid),
				"AGENTCHUTE_RUNNER":           "1",
				"AGENTCHUTE_GUARD":            "0",
				"AGENTCHUTE_RECOVERED_VENDOR": record.Vendor,
			}
			if record.Guarded {
				values["AGENTCHUTE_GUARD"] = "1"
			}
			for name, value := range values {
				if err := os.Setenv(name, value); err != nil {
					return fmt.Errorf("%w: restore %s: %v", errForeignRunnerEnv, name, err)
				}
			}
			return nil
		}
		if !os.IsNotExist(readErr) {
			return fmt.Errorf("%w: read serve identity for ancestor %d: %v", errForeignRunnerEnv, pid, readErr)
		}
		if pid <= 1 {
			break
		}
		argv, err := serveIdentityArgv(pid)
		if err != nil || len(argv) == 0 {
			return fmt.Errorf("%w: Muse identity unavailable: cannot identify ancestor %d", errForeignRunnerEnv, pid)
		}
		name := filepath.Base(argv[0])
		if nearestMuse == 0 && (name == "muse" || strings.HasPrefix(name, "muse-bin")) {
			nearestMuse = pid
		}
		parent, err := parentPIDOf(pid)
		if err != nil || parent == pid {
			break
		}
		child, pid = pid, parent
	}
	return fmt.Errorf("%w: Muse identity unavailable; launch with ac serve muse", errForeignRunnerEnv)
}
