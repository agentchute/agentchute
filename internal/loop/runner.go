package loop

import (
	"encoding/json"
	"fmt"
	"io"
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

// RunnerPingResponse is the runner socket health response. It proves the
// socket is an agentchute runner, not just any process accepting connections.
type RunnerPingResponse struct {
	OK          bool   `json:"ok"`
	AgentID     string `json:"agent_id,omitempty"`
	RunnerPID   int    `json:"runner_pid"`
	ChildPID    int    `json:"child_pid,omitempty"`
	PendingWake bool   `json:"pending_wake"`
	Status      string `json:"status,omitempty"`
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

// RunnerSocketReachable reports whether a local runner socket answers the
// ping/ack protocol. It does not enqueue a wake.
func RunnerSocketReachable(target string, timeout time.Duration) bool {
	_, err := PingRunner(target, timeout)
	return err == nil
}

// PingRunner asks an agentchute-run socket for its health payload.
func PingRunner(target string, timeout time.Duration) (*RunnerPingResponse, error) {
	path, err := ParseRunnerWakeTarget(target)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = time.Second
	}
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := json.NewEncoder(conn).Encode(map[string]any{"op": "ping"}); err != nil {
		return nil, err
	}
	var resp RunnerPingResponse
	if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("runner ping returned ok=false")
	}
	return &resp, nil
}

// RegistrationReachable reports whether reg's wake target is currently reachable
// from this host, WITHOUT ever dialing a socket the recipient does not
// legitimately own.
//
// Simple-again Gate 6a (pull-only): the wake-adapter dispatch + the tmux/herdr
// reachability probes were removed (senders no longer poke; those transports are
// retiring). The only endpoint still probeable in-package is the runner RECEIVE
// socket, which survives until Gate 6b — so this is now a runner-socket liveness
// probe. The WI-3 recipient-binding invariant is preserved: the owned-check
// (cfg.RunnerWakeTargetOwnedBy) runs FIRST and an unowned target is reported
// unreachable WITHOUT a dial; only an owned target is dialed
// (RunnerSocketReachable). Any non-runner method (or nil/empty/unknown) reports
// unreachable. Vestigial callers (status REACHABLE column, register
// runner-primary selection, poller ensure) keep compiling unchanged; their
// wake-field reads are stripped in Gate 6c.
func RegistrationReachable(cfg *Config, reg *Registration, timeout time.Duration) bool {
	if reg == nil {
		return false
	}
	if strings.TrimSpace(reg.WakeTarget) == "" {
		return false
	}
	if strings.TrimSpace(reg.WakeMethod) != RunnerWakeMethod {
		return false
	}
	if cfg == nil {
		return false
	}
	if err := cfg.RunnerWakeTargetOwnedBy(reg.AgentID, reg.WakeTarget); err != nil {
		// Not a socket this recipient owns: never dial it.
		return false
	}
	return RunnerSocketReachable(reg.WakeTarget, timeout)
}
