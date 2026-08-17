//go:build !linux && !darwin

package loop

func platformBootRef() string { return "" }
