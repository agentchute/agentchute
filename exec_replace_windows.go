//go:build windows

package main

import (
	"os"
	"os/exec"
)

func execReplace(path string, argv []string, env []string) error {
	cmd := exec.Command(path, argv[1:]...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if cmd.ProcessState != nil {
		os.Exit(cmd.ProcessState.ExitCode())
	}
	return err
}
