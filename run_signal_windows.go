//go:build windows

package main

import "os"

func signalNotifyResize(ch chan<- os.Signal) {}

func signalStopResize(ch chan<- os.Signal) {}

func signalNotifyShutdown(ch chan<- os.Signal) {}

func signalStopShutdown(ch chan<- os.Signal) {}
