//go:build !darwin && !linux

package main

import "os"

func runnerMakeRaw(stdin *os.File) (func() error, bool, error) {
	return func() error { return nil }, false, nil
}

func runnerIsTerminal(stdin *os.File) bool {
	return false
}
