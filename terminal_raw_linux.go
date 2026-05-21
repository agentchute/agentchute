//go:build linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

func runnerMakeRaw(stdin *os.File) (func() error, bool, error) {
	fd := int(stdin.Fd())
	if !runnerIsTerminal(stdin) {
		return func() error { return nil }, false, nil
	}
	oldState, err := runnerGetTermios(fd)
	if err != nil {
		return nil, false, err
	}
	raw := *oldState
	raw.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Cflag |= syscall.CS8
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := runnerSetTermios(fd, &raw); err != nil {
		return nil, false, err
	}
	return func() error { return runnerSetTermios(fd, oldState) }, true, nil
}

func runnerIsTerminal(stdin *os.File) bool {
	_, err := runnerGetTermios(int(stdin.Fd()))
	return err == nil
}

func runnerGetTermios(fd int) (*syscall.Termios, error) {
	var t syscall.Termios
	if err := runnerIoctl(fd, syscall.TCGETS, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	return &t, nil
}

func runnerSetTermios(fd int, t *syscall.Termios) error {
	return runnerIoctl(fd, syscall.TCSETS, unsafe.Pointer(t))
}

func runnerIoctl(fd int, req uint, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}
