package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

func TestRemoteNoteProductionRendererGolden(t *testing.T) {
	stdout, stderr, err := captureStdoutStderr(t, func() error {
		renderOpNote(op.NoteEvent{Level: op.NoteWarn, Msg: "first warning"})
		renderOpNote(op.NoteEvent{Level: op.NoteInfo, Msg: "then info"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "then info\n" || stderr != "warning: first warning\n" {
		t.Fatalf("stdout/stderr = %q/%q", stdout, stderr)
	}
}

func TestPrintRemoteStatusUsesHubRowsAndPinnedNotices(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	cfg := &loop.Config{
		ControlRepo: "/local/repo", ControlRepoOrigin: "env",
		LoopDir: "/local/shadow/.agentchute/loop", LoopDirOrigin: "remote", Vendor: "agentchute",
		Remote: &loop.RemoteConfig{URL: "ssh://hub.example/remote/pool"},
	}
	resp := op.StatusResp{Now: now, Truncated: true, Agents: []op.StatusAgent{{
		AgentID: "codex-tiny", LastSeen: now.Add(-2 * time.Second), Host: "tiny", ProtocolVersion: loop.CurrentProtocolVersion, InboxDepth: 7, Status: "lease-held",
	}}}
	var out bytes.Buffer
	printRemoteStatus(&out, cfg, resp)
	text := out.String()
	for _, want := range []string{
		"control_repo: ssh://hub.example/remote/pool   (via env)",
		"loop_dir:     /local/shadow/.agentchute/loop   (via remote)",
		"  (local shadow: this process's own loop dir, not the hub's)",
		"codex-tiny  lease-held  7",
		"note: listing truncated by the hub at the first row that would exceed 64 rows or a 64 KiB response; later agent ids are missing.\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("remote status missing %q:\n%s", want, text)
		}
	}
}

func TestRemoteSendUnknownSpoolsOutsideShadowWithBodyFileRetry(t *testing.T) {
	hubDir := t.TempDir()
	cfg := &loop.Config{
		LoopDir: filepath.Join(hubDir, ".agentchute", "loop"),
		Remote:  &loop.RemoteConfig{HubDir: hubDir},
	}
	err := preserveRemoteSendBody(cfg, "codex-tiny", "claude-code", "body", time.Now().UTC(), sendRetryOptions{}, &hubclient.Error{Code: "E_SEND_UNKNOWN", Msg: "unknown"})
	if err == nil {
		t.Fatal("expected E_SEND_UNKNOWN")
	}
	if !strings.Contains(err.Error(), "DELIVERY UNKNOWN") || !strings.Contains(err.Error(), "--body-file") {
		t.Fatalf("error = %v", err)
	}
	entries, readErr := os.ReadDir(filepath.Join(hubDir, "spool"))
	if readErr != nil || len(entries) != 1 {
		t.Fatalf("spool entries = %v, err=%v", entries, readErr)
	}
	spool := filepath.Join(hubDir, "spool", entries[0].Name())
	if strings.HasPrefix(spool, cfg.LoopDir+string(filepath.Separator)) {
		t.Fatalf("spool %s is inside shadow %s", spool, cfg.LoopDir)
	}
	data, readErr := os.ReadFile(spool)
	if readErr != nil || string(data) != "body" {
		t.Fatalf("spool body = %q, err=%v", data, readErr)
	}
	retryBody, retryErr := readSendBodyFile(cfg, spool)
	if retryErr != nil || retryBody != "body" {
		t.Fatalf("printed --body-file retry was not accepted: body=%q err=%v", retryBody, retryErr)
	}
}

func TestW6ClientRemoteCheckClaimedHeldArmsLocalLatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	t.Setenv("AGENTCHUTE_GUARD", "1")
	t.Setenv("AGENTCHUTE_SERVE_TOKEN", "session-token")
	t.Setenv("AGENTCHUTE_W6_TEST_BINARY", os.Args[0])
	t.Setenv("AGENTCHUTE_W6_TEST_HELPER", "1")

	remote, err := loop.ParseRemoteURL("ssh://hub.example/remote/pool")
	if err != nil {
		t.Fatal(err)
	}
	if err := hubclient.WriteHubConfig(remote.HubID, &hubclient.HubConfig{
		URL: remote.URL, JoinedAs: []string{"codex"}, Pool: "/remote/pool", Pool12: "0123456789ab",
	}); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	ssh := filepath.Join(binDir, "ssh")
	script := "#!/bin/sh\nexec \"$AGENTCHUTE_W6_TEST_BINARY\" -test.run '^TestW6ClientRemoteCheckClaimedHeldSSHHelper$'\n"
	if err := os.WriteFile(ssh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	err = cmdCheck([]string{"--as", "codex", "--control-repo", remote.URL})
	if hubclient.ErrorCode(err) != "E_HUB_IO" || !hubclient.ClaimedHeld(err) {
		t.Fatalf("check error = %v code=%s claimed_held=%v", err, hubclient.ErrorCode(err), hubclient.ClaimedHeld(err))
	}
	cfg := &loop.Config{LoopDir: remote.ShadowLoopDir}
	latch, readErr := loop.ReadGuardLatch(cfg, "codex")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if latch.Session != "session-token" {
		t.Fatalf("latch session = %q", latch.Session)
	}
}

func TestW6ClientRemoteCheckClaimedHeldSSHHelper(t *testing.T) {
	if os.Getenv("AGENTCHUTE_W6_TEST_HELPER") != "1" {
		return
	}
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	r := hubwire.NewReader(os.Stdin)
	w := hubwire.NewWriter(os.Stdout)
	raw, err := r.Read()
	if err != nil || raw.T != "hello" {
		fail(fmt.Errorf("read hello: type=%q err=%v", raw.T, err))
	}
	var hello hubwire.Hello
	if err := raw.Decode(&hello); err != nil {
		fail(err)
	}
	if err := w.Write(hubwire.HelloOK{
		ResponseBase: hubwire.ResponseBase{T: "hello-ok", Re: raw.ID},
		V:            hubwire.Version, Agent: hello.Agent, Pool: "/remote/pool", Pool12: "0123456789ab",
		Writable: true, HubBin: "test", HubTime: time.Now().UTC(),
	}, nil); err != nil {
		fail(err)
	}
	raw, err = r.Read()
	if err != nil || raw.T != "check" {
		fail(fmt.Errorf("read check: type=%q err=%v", raw.T, err))
	}
	if err := w.Write(hubwire.Error{
		ResponseBase: hubwire.ResponseBase{T: "error", Re: raw.ID},
		Code:         "E_HUB_IO", Msg: "fixture unreadable claim", ClaimedHeld: true,
	}, nil); err != nil {
		fail(err)
	}
	os.Exit(0)
}

func TestRecipientReachabilityFromWireError(t *testing.T) {
	rr, ok := recipientReachabilityFromWireError(`op: stale: recipient "grok" registration is stale (last_seen=2026-08-17T11:59:00Z age=1m0s > stale_after=30s)`)
	if !ok || rr.Age != time.Minute || rr.Threshold != 30*time.Second {
		t.Fatalf("reachability = %#v, ok=%v", rr, ok)
	}
	if _, ok := recipientReachabilityFromWireError(errors.New("bad").Error()); ok {
		t.Fatal("malformed error parsed")
	}
}
