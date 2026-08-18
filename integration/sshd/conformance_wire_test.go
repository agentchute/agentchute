//go:build sshd_integration

package sshd

import (
	"testing"

	"github.com/agentchute/agentchute/internal/spectest"
)

func TestSSHDConformanceW1ThroughW6(t *testing.T) {
	requireSSHDTest(t)
	vectors, err := spectest.LoadVectors("wire.json")
	if err != nil {
		t.Fatal(err)
	}
	build := func(t *testing.T) spectest.SessionFactory { return newSSHDHarness(t) }
	spectest.AssertWireVectors(t, vectors, build)
	spectest.AssertWireClientVectors(t, vectors, build)
}
