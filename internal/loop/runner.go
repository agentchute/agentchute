package loop

import (
	"encoding/json"
	"fmt"
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
	Host          string    `json:"host,omitempty"`
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

// ParseRunnerWakeTarget parses an agentchute-run wake target. It also enforces
// the wake_target shape (unix: prefix, non-empty clean absolute path) so every
// caller that dials the socket — the poke adapter AND the liveness pings — is
// protected from a hand-written registration smuggling a relative path or a
// path with embedded control characters.
func ParseRunnerWakeTarget(target string) (string, error) {
	if err := ValidateWakeTarget(RunnerWakeMethod, target); err != nil {
		return "", err
	}
	target = strings.TrimSpace(target)
	path := strings.TrimSpace(strings.TrimPrefix(target, runnerTargetUnix))
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

// RegistrationReachable reports whether reg's agent is reachable under pull-only
// coordination.
//
// Simple-again Gate 6b (pull-only): the runner RECEIVE socket was removed (Gate
// 6a retired every sender; 6b deletes the receive side), so there is no wake
// endpoint to dial — "reachable by poke" no longer exists. Reachability now
// means LIVENESS: the agent's `.live` presence fact is fresh (loop.IsLive, R1).
// An absent/stale/unreadable `.live` reads not-reachable, which is the safe
// direction. The timeout parameter is retained for signature compatibility with
// the vestigial callers (status REACHABLE column, register runner-primary
// selection, poller ensure) but is unused; those callers keep compiling and
// their wake-field reads are stripped in Gate 6c.
func RegistrationReachable(cfg *Config, reg *Registration, _ time.Duration) bool {
	if cfg == nil || reg == nil {
		return false
	}
	return IsLive(cfg, reg.AgentID, liveWindow, time.Now())
}
