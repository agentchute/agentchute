//go:build !windows

package main

import "syscall"

func execReplace(path string, argv []string, env []string) error {
	return syscall.Exec(path, argv, env)
}
