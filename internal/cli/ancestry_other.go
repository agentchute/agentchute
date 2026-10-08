//go:build !darwin && !linux

package cli

func platformParentPID(int) (int, error) { return 0, errAncestryUnsupported }
