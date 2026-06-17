package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

const activeSessionMaxAge = 10 * time.Minute

func saveActiveSessionHeartbeat(cfg *loop.Config, agentID, source string, now time.Time) error {
	return loop.SaveActiveSession(cfg, loop.ActiveSession{
		AgentID:  agentID,
		Source:   source,
		Host:     localHostname(),
		PID:      activeSessionPID(),
		LastSeen: now,
	})
}

func activeSessionPID() int {
	if raw := strings.TrimSpace(os.Getenv("AGENTCHUTE_RUNNER_PID")); raw != "" {
		pid, err := strconv.Atoi(raw)
		if err == nil && pid > 0 && processAlive(pid) {
			return pid
		}
		return 0
	}
	return activeSessionPIDFrom(os.Getppid(), lookupProcessInfo)
}

type processInfo struct {
	ParentPID int
	Command   string
}

var lookupProcessInfo = psProcessInfo

func activeSessionPIDFrom(pid int, lookup func(int) (processInfo, bool)) int {
	if pid <= 0 {
		return 0
	}
	for i := 0; i < 12 && pid > 1; i++ {
		info, ok := lookup(pid)
		if !ok {
			return 0
		}
		if isKnownWrapperProcess(info.Command) {
			return pid
		}
		if info.ParentPID <= 1 || info.ParentPID == pid {
			return 0
		}
		pid = info.ParentPID
	}
	return 0
}

func psProcessInfo(pid int) (processInfo, bool) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "ppid=", "-o", "comm=").Output()
	if err != nil {
		return processInfo{}, false
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return processInfo{}, false
	}
	ppid, err := strconv.Atoi(fields[0])
	if err != nil {
		return processInfo{}, false
	}
	return processInfo{ParentPID: ppid, Command: strings.Join(fields[1:], " ")}, true
}

func isHookShellProcess(command string) bool {
	base := filepath.Base(strings.TrimSpace(command))
	base = strings.TrimPrefix(base, "-")
	switch base {
	case "sh", "bash", "dash", "zsh", "fish", "ksh":
		return true
	default:
		return false
	}
}

func isKnownWrapperProcess(command string) bool {
	base := filepath.Base(strings.TrimSpace(command))
	base = strings.TrimPrefix(base, "-")
	switch base {
	case "claude", "claude-code", "codex", "gemini", "gemini-cli", "grok":
		return true
	default:
		return false
	}
}

func activeSessionAlive(session *loop.ActiveSession) bool {
	return activeSessionAliveAt(session, time.Now().UTC())
}

func activeSessionAliveAt(session *loop.ActiveSession, now time.Time) bool {
	alive, _ := activeSessionAliveAtWithReason(session, now)
	return alive
}

func activeSessionAliveAtWithReason(session *loop.ActiveSession, now time.Time) (bool, string) {
	if session == nil {
		return false, "no session"
	}
	localHost := strings.TrimSpace(localHostname())
	if strings.TrimSpace(session.Host) != "" && localHost != "" && session.Host != localHost {
		return false, fmt.Sprintf("session host %q != local %q", session.Host, localHost)
	}
	if session.PID > 0 && processAlive(session.PID) {
		return true, "process alive"
	}
	age := now.UTC().Sub(session.LastSeen.UTC())
	if age >= 0 && age <= activeSessionMaxAge {
		return true, fmt.Sprintf("fresh heartbeat (age=%s)", age.Round(time.Second))
	}
	return false, fmt.Sprintf("process dead and heartbeat stale (age=%s threshold=%s)", age.Round(time.Second), activeSessionMaxAge)
}
