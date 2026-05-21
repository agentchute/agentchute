package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/agentchute/agentchute/internal/loop"
)

var tmuxProbeBinary = "tmux"

func currentTmuxPane() string {
	return strings.TrimSpace(os.Getenv("TMUX_PANE"))
}

func tmuxTargetReachable(target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	if _, err := exec.LookPath(tmuxProbeBinary); err != nil {
		return false
	}
	return exec.Command(tmuxProbeBinary, "list-panes", "-t", target).Run() == nil
}

type peerWakeStale struct {
	AgentID string `json:"agent_id"`
	Target  string `json:"target"`
	Path    string `json:"-"`
}

func findStalePeerTmuxWakeTargets(cfg *loop.Config, selfID string) ([]peerWakeStale, error) {
	localHost, _ := os.Hostname()
	if strings.TrimSpace(localHost) == "" {
		return nil, nil
	}
	if _, err := exec.LookPath(tmuxProbeBinary); err != nil {
		return nil, nil
	}

	entries, err := os.ReadDir(cfg.AgentsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var stale []peerWakeStale
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".example.md") || name == "README.md" {
			continue
		}
		path := filepath.Join(cfg.AgentsDir(), name)
		reg, err := loop.ReadRegistration(path)
		if err != nil {
			continue
		}
		if reg.AgentID == selfID || reg.Host != localHost || strings.TrimSpace(reg.WakeMethod) != "tmux" {
			continue
		}
		target := strings.TrimSpace(reg.WakeTarget)
		if target == "" || tmuxTargetReachable(target) {
			continue
		}
		stale = append(stale, peerWakeStale{AgentID: reg.AgentID, Target: target, Path: path})
	}
	return stale, nil
}

func pruneStalePeerTmuxRegistrations(cfg *loop.Config, selfID string) ([]peerWakeStale, error) {
	stale, err := findStalePeerTmuxWakeTargets(cfg, selfID)
	if err != nil {
		return nil, err
	}
	for _, peer := range stale {
		if peer.Path == "" {
			continue
		}
		if err := os.Remove(peer.Path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale tmux registration %q: %w", peer.AgentID, err)
		}
	}
	return stale, nil
}
