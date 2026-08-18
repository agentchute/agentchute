//go:build sshd_integration

package sshd

import "testing"

func TestSSHDLauncherPathsPreserveRemoteness(t *testing.T) {
	runLauncherPathsPreserveRemoteness(t)
}
