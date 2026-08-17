package op

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// newPool builds an empty control repo + loop dir in a temp tree. Every op test
// runs against its own pool; none of them ever touches a real one.
func newPool(t *testing.T) *loop.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".agentchute", "loop"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// enroll registers id through the seam itself, so every fixture is built by the
// code under test rather than by a second, drifting path.
func enroll(t *testing.T, cfg *loop.Config, id string) RegisterResp {
	t.Helper()
	vendor := "test"
	resp, err := Register(cfg, Context{ActorID: id}, RegisterReq{Vendor: &vendor, Host: "test-host"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("enroll %s: %v", id, err)
	}
	return resp
}

// backdate rewrites id's registration with an older last_seen, which is how a
// row becomes stale without waiting for one.
func backdate(t *testing.T, cfg *loop.Config, id string, lastSeen time.Time) {
	t.Helper()
	path := cfg.AgentRegistrationPath(id)
	reg, err := loop.ReadRegistration(path)
	if err != nil {
		t.Fatal(err)
	}
	reg.LastSeen = lastSeen
	if err := loop.WriteRegistration(path, reg); err != nil {
		t.Fatal(err)
	}
}

// deliver drops one real message from -> to through the same delivery primitive
// Send uses.
func deliver(t *testing.T, cfg *loop.Config, from, to, body string) loop.TsID {
	t.Helper()
	id, _, err := loop.SendTsMessageWithCommit(cfg, from, to, loop.ComposeMessage(from, "", body), "")
	if err != nil {
		t.Fatalf("deliver %s -> %s: %v", from, to, err)
	}
	return id
}

// collector records the event stream in order. Everything a positional
// assertion needs is a plain slice of what arrived, in the order it arrived.
type collector struct {
	events []Event
	failAt int // 1-based index to fail on; 0 = never
	err    error
}

func (c *collector) emit(ev Event) error {
	c.events = append(c.events, ev)
	if c.failAt > 0 && len(c.events) == c.failAt {
		c.err = errEmitFailed
		return c.err
	}
	return nil
}

// kinds renders the stream as a compact shape string, so an order assertion
// reads as the sequence it is checking.
func (c *collector) kinds() []string {
	out := make([]string, 0, len(c.events))
	for _, ev := range c.events {
		switch {
		case ev.Message != nil:
			out = append(out, "message")
		case ev.Note != nil:
			out = append(out, "note/"+ev.Note.Level)
		case ev.Owed != nil:
			out = append(out, "owed")
		case ev.Ack != nil:
			out = append(out, "ack")
		default:
			out = append(out, "empty")
		}
	}
	return out
}

func (c *collector) notes(level string) []string {
	var out []string
	for _, ev := range c.events {
		if ev.Note != nil && ev.Note.Level == level {
			out = append(out, ev.Note.Msg)
		}
	}
	return out
}

func (c *collector) messages() []MessageEvent {
	var out []MessageEvent
	for _, ev := range c.events {
		if ev.Message != nil {
			out = append(out, *ev.Message)
		}
	}
	return out
}

func (c *collector) assertUnionInvariant(t *testing.T) {
	t.Helper()
	for i, ev := range c.events {
		if !ev.Valid() {
			t.Fatalf("event %d has %d arms set, want exactly 1", i, ev.Arms())
		}
	}
}

var errEmitFailed = &emitError{}

type emitError struct{}

func (*emitError) Error() string { return "emit failed" }

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n
}
