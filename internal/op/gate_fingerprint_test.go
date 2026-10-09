package op

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGateFingerprintTracksBlockingIdentities(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	actor := Context{ActorID: "claude-code"}
	get := func() GateResp {
		g, err := Gate(cfg, actor, GateReq{Phase: GatePhaseFinish})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	deliver(t, cfg, "codex", "claude-code", "one")
	one := get()
	if same := get(); one.BlockingFingerprint == "" || same.BlockingFingerprint != one.BlockingFingerprint {
		t.Fatal("identical gate state did not keep a stable fingerprint")
	}
	var c collector
	if _, err := Claim(cfg, actor, ClaimReq{}, c.emit); err != nil {
		t.Fatal(err)
	}
	deliver(t, cfg, "codex", "claude-code", "two")
	two := get()
	if one.UnreadCount != two.UnreadCount || one.BlockingFingerprint == two.BlockingFingerprint {
		t.Fatal("equal counts hid a new message identity")
	}
	bad := filepath.Join(cfg.AgentInboxDir(actor.ActorID), "bad-one.md")
	if err := os.WriteFile(bad, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	malformed := get()
	if err := os.Rename(bad, filepath.Join(filepath.Dir(bad), "bad-two.md")); err != nil {
		t.Fatal(err)
	}
	changed := get()
	if malformed.MalformedCount != changed.MalformedCount || malformed.BlockingFingerprint == changed.BlockingFingerprint {
		t.Fatal("equal malformed counts hid a different name")
	}
	if err := os.Remove(cfg.AgentRegistrationPath(actor.ActorID)); err != nil {
		t.Fatal(err)
	}
	missing := get()
	if !missing.MissingReg || missing.BlockingFingerprint == changed.BlockingFingerprint {
		t.Fatal("missing registration not fingerprinted")
	}
	enroll(t, cfg, actor.ActorID)
	if restored := get(); restored.BlockingFingerprint != changed.BlockingFingerprint {
		t.Fatal("registration heartbeat contaminated the fingerprint")
	}
	data, err := json.Marshal(missing)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), missing.BlockingFingerprint) {
		t.Fatal("local fingerprint leaked into the wire shape")
	}
	var remote GateResp
	if err := json.Unmarshal(data, &remote); err != nil {
		t.Fatal(err)
	}
	if remote.BlockingFingerprint != "" {
		t.Fatal("a wire response claims an identity snapshot")
	}
}
