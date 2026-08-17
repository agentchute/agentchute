//go:build linux

package loop

import "strings"

func platformBootRef() string {
	b, err := ReadFileLimit("/proc/sys/kernel/random/boot_id", 64)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
