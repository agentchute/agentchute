package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

const (
	RunnerWakeMethod = "agentchute-run"
	runnerTargetUnix = "unix:"
)

// RunnerState is local diagnostic/recovery state for `agentchute run`.
// Registration last_seen remains the liveness source of truth.
type RunnerState struct {
	AgentID       string    `json:"agent_id"`
	RunnerPID     int       `json:"runner_pid"`
	ChildPID      int       `json:"child_pid,omitempty"`
	SocketPath    string    `json:"socket_path"`
	StartedAt     time.Time `json:"started_at"`
	LastPoll      time.Time `json:"last_poll,omitempty"`
	LastInjection time.Time `json:"last_injection,omitempty"`
	PendingWake   bool      `json:"pending_wake"`
	Status        string    `json:"status"`
}

// RunnerWakeTarget formats a local Unix socket target for registrations.
func RunnerWakeTarget(socketPath string) string {
	return runnerTargetUnix + socketPath
}

// ParseRunnerWakeTarget parses an agentchute-run wake target.
func ParseRunnerWakeTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if !strings.HasPrefix(target, runnerTargetUnix) {
		return "", fmt.Errorf("agentchute-run wake target must start with %q", runnerTargetUnix)
	}
	path := strings.TrimSpace(strings.TrimPrefix(target, runnerTargetUnix))
	if path == "" {
		return "", fmt.Errorf("agentchute-run wake target has empty socket path")
	}
	return path, nil
}

// SaveRunnerState writes runner state atomically under state/<agent>/.
func SaveRunnerState(cfg *Config, st RunnerState) error {
	if err := ValidateAgentID(st.AgentID); err != nil {
		return err
	}
	if st.StartedAt.IsZero() {
		st.StartedAt = time.Now().UTC()
	}
	st.StartedAt = st.StartedAt.UTC()
	if !st.LastPoll.IsZero() {
		st.LastPoll = st.LastPoll.UTC()
	}
	if !st.LastInjection.IsZero() {
		st.LastInjection = st.LastInjection.UTC()
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWriteFile(cfg.RunnerStatePath(st.AgentID), data)
}

// LoadRunnerState reads state/<agent>/runner.json.
func LoadRunnerState(cfg *Config, agentID string) (*RunnerState, error) {
	if err := ValidateAgentID(agentID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(cfg.RunnerStatePath(agentID))
	if err != nil {
		return nil, err
	}
	var st RunnerState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	if st.AgentID != agentID {
		return nil, fmt.Errorf("runner state reports agent_id=%q, expected %q", st.AgentID, agentID)
	}
	return &st, nil
}

// RunnerSocketReachable reports whether a local runner socket accepts a
// connection. It does not send a wake.
func RunnerSocketReachable(target string, timeout time.Duration) bool {
	path, err := ParseRunnerWakeTarget(target)
	if err != nil {
		return false
	}
	if timeout <= 0 {
		timeout = time.Second
	}
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

type runnerWakeAdapter struct{}

func (runnerWakeAdapter) Poke(ctx context.Context, target string) error {
	path, err := ParseRunnerWakeTarget(target)
	if err != nil {
		return err
	}
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	req := map[string]any{
		"op":     "wake",
		"reason": "new_mail",
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	return nil
}

func init() {
	RegisterWakeAdapter(RunnerWakeMethod, runnerWakeAdapter{})
}
