//go:build darwin

package loop

import (
	"strings"

	"golang.org/x/sys/unix"
)

func platformBootRef() string {
	ref, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(ref)
}
