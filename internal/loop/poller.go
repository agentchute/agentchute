package loop

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

const (
	DefaultPollerIntervalSeconds = 30
	MinPollerIntervalSeconds     = 5
)

// PollerHeartbeat is recipient-owned liveness proof for non-pokable agents.
// It is intentionally small: enough to prove a poller on some host can see the
// shared inbox, without storing wrapper prompts or other operator secrets.
type PollerHeartbeat struct {
	AgentID         string    `json:"agent_id"`
	Method          string    `json:"method"`
	Host            string    `json:"host,omitempty"`
	PID             int       `json:"pid,omitempty"`
	IntervalSeconds int       `json:"interval_seconds"`
	LaunchEnabled   bool      `json:"launch_enabled,omitempty"`
	LastSeen        time.Time `json:"last_seen"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	// LastError records the most recent poll-computation failure. It is set
	// when a tick's mail-consumption step errors WITHOUT refreshing LastSeen,
	// so liveness consumers can distinguish a "beating but failing" poller
	// from a healthy one. Cleared on the next successful tick.
	LastError string `json:"last_error,omitempty"`
}

// SavePollerHeartbeat writes the heartbeat atomically under state/<agent>/.
func SavePollerHeartbeat(cfg *Config, hb PollerHeartbeat) error {
	if err := ValidateAgentID(hb.AgentID); err != nil {
		return err
	}
	if hb.IntervalSeconds < MinPollerIntervalSeconds {
		return fmt.Errorf("poller interval must be >= %d seconds", MinPollerIntervalSeconds)
	}
	if hb.Method == "" {
		hb.Method = "poller"
	}
	if hb.LastSeen.IsZero() {
		hb.LastSeen = time.Now().UTC()
	}
	hb.LastSeen = hb.LastSeen.UTC()
	if !hb.StartedAt.IsZero() {
		hb.StartedAt = hb.StartedAt.UTC()
	}
	data, err := json.MarshalIndent(hb, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWriteFile(cfg.PollerHeartbeatPath(hb.AgentID), data)
}

// LoadPollerHeartbeat reads state/<agent>/poller.json.
func LoadPollerHeartbeat(cfg *Config, agentID string) (*PollerHeartbeat, error) {
	if err := ValidateAgentID(agentID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(cfg.PollerHeartbeatPath(agentID))
	if err != nil {
		return nil, err
	}
	var hb PollerHeartbeat
	if err := json.Unmarshal(data, &hb); err != nil {
		return nil, err
	}
	if hb.AgentID != agentID {
		return nil, fmt.Errorf("poller heartbeat reports agent_id=%q, expected %q", hb.AgentID, agentID)
	}
	return &hb, nil
}

// PollerFreshness returns whether a heartbeat is fresh, plus its age and
// threshold. The threshold scales with the declared interval so a 5-minute
// scheduler is not falsely marked stale after 2 minutes, while fast pollers
// still fail quickly when they die.
func PollerFreshness(hb *PollerHeartbeat, now time.Time) (fresh bool, age, threshold time.Duration) {
	if hb == nil || hb.LastSeen.IsZero() {
		return false, 0, PollerFreshnessThreshold(DefaultPollerIntervalSeconds)
	}
	age = now.UTC().Sub(hb.LastSeen.UTC())
	threshold = PollerFreshnessThreshold(hb.IntervalSeconds)
	// Clamp a small negative age (a future-dated, clock-skewed heartbeat) to
	// fresh. Without this, a heartbeat a few seconds in the future reads
	// stale and false-blocks the gate.
	if age < 0 {
		age = 0
	}
	return age <= threshold, age, threshold
}

func PollerFreshnessThreshold(intervalSeconds int) time.Duration {
	if intervalSeconds < MinPollerIntervalSeconds {
		intervalSeconds = DefaultPollerIntervalSeconds
	}
	threshold := time.Duration(intervalSeconds*3) * time.Second
	minimum := 2 * time.Minute
	if threshold < minimum {
		return minimum
	}
	return threshold
}
