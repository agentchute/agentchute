//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func signalNotifyResize(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGWINCH)
}

func signalStopResize(ch chan<- os.Signal) {
	signal.Stop(ch)
}

func signalNotifyShutdown(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
}

func signalStopShutdown(ch chan<- os.Signal) {
	signal.Stop(ch)
}
