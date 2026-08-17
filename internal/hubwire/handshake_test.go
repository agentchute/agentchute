package hubwire

import (
	"errors"
	"testing"
	"time"
)

func TestHandshakeMatrix(t *testing.T) {
	now := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		clientV   int
		clientMin int
		hubMax    int
		wantV     int
		wantCode  string
	}{
		{"v1-v1", 1, 1, 1, 1, ""},
		{"v1 client v2 hub", 1, 1, 2, 1, ""},
		{"v2 client v1 hub", 2, 1, 1, 1, ""},
		{"client requires v2", 2, 2, 1, 0, CodeVersion},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := NegotiateHello(Hello{RequestBase: RequestBase{T: "hello", ID: 1}, Proto: Protocol, V: tc.clientV, MinV: tc.clientMin, Agent: "codex"}, HandshakeOptions{PinnedAgent: "codex", Pool: t.TempDir(), Pool12: "0123456789ab", HubMax: tc.hubMax, Now: func() time.Time { return now }})
			if tc.wantCode != "" {
				assertCode(t, err, tc.wantCode)
				return
			}
			if err != nil || resp.V != tc.wantV || resp.Pool12 != "0123456789ab" || !resp.HubTime.Equal(now) {
				t.Fatalf("resp=%+v err=%v", resp, err)
			}
		})
	}
}

func TestHandshakeIdentityMismatch(t *testing.T) {
	_, err := NegotiateHello(Hello{RequestBase: RequestBase{T: "hello", ID: 1}, Proto: Protocol, V: 1, MinV: 1, Agent: "grok"}, HandshakeOptions{PinnedAgent: "codex"})
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Code != CodeIdentity {
		t.Fatalf("err = %v", err)
	}
}
