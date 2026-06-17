package loop

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// ActiveSession is local lifecycle state for a visible wrapper process. Hook
// commands write it from boot/self-check so gates do not need a hidden
// background wrapper just to prove that the current agent is alive.
type ActiveSession struct {
	AgentID  string    `json:"agent_id"`
	Source   string    `json:"source,omitempty"`
	Host     string    `json:"host,omitempty"`
	PID      int       `json:"pid,omitempty"`
	LastSeen time.Time `json:"last_seen"`
}

func SaveActiveSession(cfg *Config, session ActiveSession) error {
	if err := ValidateAgentID(session.AgentID); err != nil {
		return err
	}
	if session.LastSeen.IsZero() {
		session.LastSeen = time.Now().UTC()
	}
	session.LastSeen = session.LastSeen.UTC()
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWriteFile(cfg.ActiveSessionPath(session.AgentID), data)
}

func LoadActiveSession(cfg *Config, agentID string) (*ActiveSession, error) {
	if err := ValidateAgentID(agentID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(cfg.ActiveSessionPath(agentID))
	if err != nil {
		return nil, err
	}
	var session ActiveSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}
	if session.AgentID != agentID {
		return nil, fmt.Errorf("active session reports agent_id=%q, expected %q", session.AgentID, agentID)
	}
	return &session, nil
}
