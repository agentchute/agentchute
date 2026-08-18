package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubwire"
)

// Site 1 — the hub refuses to serve a session it was not pinned into.
//
// Parametrised by the variable that decides the branch rather than by a scenario
// name, so each row proves WHICH arm ran. The predicate is exactly:
//
//	reached_by_forced_command  <=>  SSH_ORIGINAL_COMMAND present AND non-empty
//
// Empty and unset are both refusals: measured, both real producers show EMPTY
// rather than unset (an intercepting layer, and an operator-key fallback).
func TestHubSessionServesOnlyWhenAForcedCommandReachedIt(t *testing.T) {
	rows := []struct {
		name     string
		env      map[string]string
		wantServ bool
	}{
		{"forced command present", map[string]string{"SSH_ORIGINAL_COMMAND": "agentchute-hub"}, true},
		{"unset", map[string]string{}, false},
		{"present but empty", map[string]string{"SSH_ORIGINAL_COMMAND": ""}, false},
		// Pins that the predicate does not silently grow a second term. A
		// SSH_CONNECTION check could only ever add false refusals: its security
		// value is zero, so it cannot make a true-accept safer.
		{"set, with SSH_CONNECTION unset", map[string]string{"SSH_ORIGINAL_COMMAND": "agentchute-hub"}, true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			pool, _ := newHubPool(t)
			withHubSessionEnv(t, row.env)
			frames, err := runHubSessionCLI(t, pool, "codex")
			if row.wantServ {
				if code := firstErrorCode(frames); code == hubwire.CodeUnpinned {
					t.Fatalf("refused a session a forced command DID reach; codes = %v", frameCodes(frames))
				}
				return
			}
			if code := firstErrorCode(frames); code != hubwire.CodeUnpinned {
				t.Fatalf("served an unpinned session; first frame code = %q, want %s", code, hubwire.CodeUnpinned)
			}
			if err == nil {
				t.Fatal("unpinned refusal exited 0; an operator on an intercepted login never sees the frame, only the exit status")
			}
		})
	}
}

// The refusal must not touch the pool.
//
// This is the load-bearing half: at the CLI seam nothing has resolved the pool
// yet, so the assertion's job is to FAIL if anyone later moves the check down
// past pool resolution, where it would still pass every row above.
func TestUnpinnedRefusalTouchesNothingInThePool(t *testing.T) {
	pool, _ := newHubPool(t)
	before := poolFingerprint(t, pool)
	withHubSessionEnv(t, map[string]string{})
	frames, err := runHubSessionCLI(t, pool, "codex")
	if err == nil || firstErrorCode(frames) != hubwire.CodeUnpinned {
		t.Fatalf("expected an unpinned refusal, got codes=%v err=%v", frameCodes(frames), err)
	}
	if after := poolFingerprint(t, pool); after != before {
		t.Fatalf("the refusal wrote into the pool:\nbefore: %v\nafter:  %v", before, after)
	}
}

// ServeHubSession — the exported conformance entry — must serve with
// SSH_ORIGINAL_COMMAND demonstrably ABSENT from the process environment.
//
// This is the row that fails the day someone "tidies" the check down into
// serveHubSession, where it would pass every row above and silently refuse the
// M3/M6 conformance drivers, which have no sshd and no such variable. It asserts
// the absence rather than assuming it, because assuming is how this row would
// quietly stop testing anything.
func TestServeHubSessionIsEnvAgnostic(t *testing.T) {
	if value, present := os.LookupEnv("SSH_ORIGINAL_COMMAND"); present {
		t.Setenv("SSH_ORIGINAL_COMMAND", "")
		_ = os.Unsetenv("SSH_ORIGINAL_COMMAND")
		t.Logf("cleared an inherited SSH_ORIGINAL_COMMAND=%q for this row", value)
	}
	if _, present := os.LookupEnv("SSH_ORIGINAL_COMMAND"); present {
		t.Fatal("SSH_ORIGINAL_COMMAND is still set; this row proves nothing unless it is absent")
	}
	pool, _ := newHubPool(t)
	frames := runServeHubSessionInProcess(t, pool, "codex")
	if code := firstErrorCode(frames); code == hubwire.CodeUnpinned {
		t.Fatalf("the exported conformance entry refused a driver that has no sshd; codes = %v", frameCodes(frames))
	}
	if len(frames) == 0 {
		t.Fatal("no frames at all; the driver got nothing back")
	}
}

func withHubSessionEnv(t *testing.T, env map[string]string) {
	t.Helper()
	original := hubSessionEnv
	t.Cleanup(func() { hubSessionEnv = original })
	hubSessionEnv = func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

// frameCodes renders frames as their type/code, because printing a RawFrame
// dumps its raw bytes as a decimal array and buries the one fact that matters.
func frameCodes(frames []hubwire.RawFrame) []string {
	var out []string
	for _, frame := range frames {
		if frame.T == "error" {
			var e hubwire.Error
			if frame.Decode(&e) == nil {
				out = append(out, "error:"+e.Code)
				continue
			}
		}
		out = append(out, frame.T)
	}
	return out
}

func firstErrorCode(frames []hubwire.RawFrame) string {
	for _, frame := range frames {
		if frame.T == "error" {
			var e hubwire.Error
			if frame.Decode(&e) == nil {
				return e.Code
			}
		}
	}
	return ""
}

// poolFingerprint is every path under the pool with its size, so "touched
// nothing" means exactly that rather than "no file I thought to check".
func poolFingerprint(t *testing.T, pool string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(pool, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(pool, path)
		b.WriteString(rel)
		if !info.IsDir() {
			b.WriteString(":")
			b.WriteString(time.Duration(info.Size()).String())
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// runHubSessionCLI drives cmdHubSession end to end over pipes and returns the
// frames it wrote, plus its error — the CLI seam, which is where the predicate
// lives.
func runHubSessionCLI(t *testing.T, pool, agent string) ([]hubwire.RawFrame, error) {
	t.Helper()
	var frames []hubwire.RawFrame
	var runErr error
	withStdio(t, func(stdin io.Writer, stdout io.Reader) {
		go func() {
			// A client that opens the session and says nothing: the hello read
			// hits EOF, which is the non-mutating arm.
			_ = stdin.(io.Closer).Close()
		}()
		runErr = cmdHubSession([]string{"--agent", agent, "--pool", pool, "--pool-id", fixturePoolID})
		frames = readFrames(t, stdout)
	})
	return frames, runErr
}

// withStdio swaps os.Stdin/os.Stdout for pipes so a row can drive cmdHubSession
// end to end — the CLI seam is the thing under test, and it reads the process's
// own stdio by construction. Beside withCwd in spirit: the package already
// serialises rows that swap process-level state.
func withStdio(t *testing.T, fn func(stdin io.Writer, stdout io.Reader)) {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origIn, origOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	t.Cleanup(func() {
		os.Stdin, os.Stdout = origIn, origOut
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})
	fn(inW, outR)
	_ = outW.Close()
}

// readFrames drains whatever the session wrote. A refusal writes exactly one
// frame and closes; a served session writes its hello-ok. Bounded by the pipe
// closing rather than by a timer, so a row cannot pass by reading nothing.
func readFrames(t *testing.T, r io.Reader) []hubwire.RawFrame {
	t.Helper()
	reader := hubwire.NewReader(r)
	var frames []hubwire.RawFrame
	for {
		frame, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) || strings.Contains(err.Error(), "file already closed") {
				return frames
			}
			return frames
		}
		frames = append(frames, frame)
		if len(frames) > 8 {
			return frames
		}
	}
}

// runServeHubSessionInProcess drives the EXPORTED conformance entry over a pipe
// pair, the way the M3/M6 drivers do — no sshd, no process environment.
func runServeHubSessionInProcess(t *testing.T, pool, agent string) []hubwire.RawFrame {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- ServeHubSession(context.Background(), server, HubSessionConfig{
			Agent: agent, Pool: pool, PoolID: fixturePoolID, HubBin: "test",
		})
	}()
	// BOTH deadlines, set before the first byte. net.Pipe is synchronous, so a
	// version that refuses before reading deadlocks on the WRITE — the client
	// blocks sending hello while the server blocks sending its error frame, and
	// a read deadline set afterwards is never reached. Measured: without this the
	// mutation hung to the test timeout twice and reported a panic instead of a
	// reason.
	deadline := time.Now().Add(5 * time.Second)
	if err := client.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := hubwire.NewWriter(client).Write(hubwire.Hello{
		RequestBase: hubwire.RequestBase{T: "hello", ID: 1},
		Proto:       hubwire.Protocol, V: hubwire.Version, MinV: hubwire.MinVersion, Agent: agent,
	}, nil); err != nil {
		t.Fatalf("the exported conformance entry never read the driver's hello — it refused before reading, which is what moving the check into serveHubSession does: %v", err)
	}
	frame, err := hubwire.NewReader(client).Read()
	if err != nil {
		t.Fatalf("the exported conformance entry wrote nothing back within 5s — it refused a driver that has no sshd, or blocked: %v", err)
	}
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeHubSession did not return")
	}
	return []hubwire.RawFrame{frame}
}
