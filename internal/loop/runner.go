package loop

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// RunnerState is local diagnostic/recovery state for `agentchute run`.
// Registration last_seen remains the liveness source of truth.
//
// Pull-only (simple-again Gate 6c): the runner no longer owns a receive socket
// and a registration publishes no wake target, so there is no SocketPath field.
type RunnerState struct {
	AgentID       string    `json:"agent_id"`
	Host          string    `json:"host,omitempty"`
	RunnerPID     int       `json:"runner_pid"`
	ChildPID      int       `json:"child_pid,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	LastPoll      time.Time `json:"last_poll,omitempty"`
	LastInjection time.Time `json:"last_injection,omitempty"`
	PendingWake   bool      `json:"pending_wake"`
	Status        string    `json:"status"`
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
