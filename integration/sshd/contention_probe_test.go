//go:build sshd_integration

package sshd

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// SCRATCH PROBE — not a matrix row, delete after red #3 closes.
//
// Tests the live hypothesis directly instead of waiting for the flake: if
// concurrent `hub session` processes for ONE agent can end each other's channel
// through hub-side state contention, then driving that concurrency hard should
// reproduce it on any platform, including the one where the flake never fires.
//
// If this stays green under heavy concurrency, contention between one-shots is
// not sufficient to produce E_CHANNEL_LOST and the hypothesis needs the serve
// lane specifically, not merely a second session.
func TestSSHDConcurrentOneShotsForOneAgentProbe(t *testing.T) {
	h := newSSHDHarness(t)
	checkout, agentID := joinNamedCodex(t, h)
	// Same setup as the failing row: a serve lane registers the agent (Deliver
	// needs a registration row) and keeps polling the same hub as the same agent
	// throughout, which is the condition the hypothesis is about.
	writeFakeCodex(t, h, filepath.Join(h.root, "probe-child.log"))
	serve := startServe(t, h, checkout, false)
	defer serve.stop()
	first := waitChildStarts(t, filepath.Join(h.root, "probe-child.log"), 1, 15*time.Second)[0]

	const workers = 8
	const rounds = 4

	for round := 0; round < rounds; round++ {
		for i := 0; i < workers; i++ {
			body := fmt.Sprintf("contention probe round %d worker %d", round, i)
			if err := h.State().Deliver("sshd-fixture-peer", agentID, body); err != nil {
				t.Fatal(err)
			}
		}

		var wg sync.WaitGroup
		failures := make([]string, workers)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				// A mix, so the probe covers a streaming terminal frame (check) and
				// non-streaming ones, all for the same agent at the same instant.
				args := [][]string{{"check"}, {"pending"}, {"status"}, {"doctor"}}[i%4]
				stdout, stderr, err := runWithChildEnv(h, checkout, first, nil, args...)
				if err != nil {
					failures[i] = fmt.Sprintf("worker %d (%v): %v\nstdout:\n%s\nstderr:\n%s", i, args, err, stdout, stderr)
				}
			}(i)
		}
		wg.Wait()

		var lost, other []string
		for _, f := range failures {
			if f == "" {
				continue
			}
			if strings.Contains(f, "channel to the hub was lost") {
				lost = append(lost, f)
			} else {
				other = append(other, f)
			}
		}
		if len(lost) > 0 {
			t.Fatalf("REPRODUCED under concurrency alone, round %d:\n%s", round, strings.Join(lost, "\n---\n"))
		}
		for _, f := range other {
			t.Logf("round %d non-channel failure (expected for some verbs): %s", round, f)
		}
	}
}
