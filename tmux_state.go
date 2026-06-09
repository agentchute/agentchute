package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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

func findSamePanePeerTmuxRegistrations(cfg *loop.Config, selfID, host, target string) ([]peerWakeStale, error) {
	host = strings.TrimSpace(host)
	target = strings.TrimSpace(target)
	if host == "" || target == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(cfg.AgentsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var peers []peerWakeStale
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
		if reg.AgentID == selfID || strings.TrimSpace(reg.Host) != host || strings.TrimSpace(reg.WakeMethod) != "tmux" || strings.TrimSpace(reg.WakeTarget) != target {
			continue
		}
		peers = append(peers, peerWakeStale{AgentID: reg.AgentID, Target: target, Path: path})
	}
	return peers, nil
}

func pruneSamePanePeerTmuxRegistrations(cfg *loop.Config, selfID, host, target string) ([]peerWakeStale, error) {
	peers, err := findSamePanePeerTmuxRegistrations(cfg, selfID, host, target)
	if err != nil {
		return nil, err
	}
	for _, peer := range peers {
		if peer.Path == "" {
			continue
		}
		if err := os.Remove(peer.Path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove same-pane tmux registration %q: %w", peer.AgentID, err)
		}
	}
	return peers, nil
}

func withTmuxPaneRegistrationLock(cfg *loop.Config, host, target string, fn func() (*registerResult, error)) (*registerResult, error) {
	host = strings.TrimSpace(host)
	target = strings.TrimSpace(target)
	if host == "" || target == "" {
		return fn()
	}

	lockRoot := filepath.Join(cfg.LoopDir, "state", "locks")
	if err := loop.EnsurePrivateDir(lockRoot); err != nil {
		return nil, fmt.Errorf("create tmux registration lock dir: %w", err)
	}
	sum := sha256.Sum256([]byte(host + "\x00" + target))
	lockPath := filepath.Join(lockRoot, "tmux-"+hex.EncodeToString(sum[:])[:16]+".lock")

	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.Mkdir(lockPath, 0o700)
		if err == nil {
			defer os.Remove(lockPath)
			return fn()
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("acquire tmux registration lock: %w", err)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > 30*time.Second {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for tmux registration lock for %s", target)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
