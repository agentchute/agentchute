//go:build !darwin

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// readProcessEnvBlock returns pid's NUL-separated environment strings from
// /proc/<pid>/environ (Linux). Elsewhere the file does not exist and the
// caller reports an unreadable environment (WARN).
func readProcessEnvBlock(pid int) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil, fmt.Errorf("read environment of pid %d: %w", pid, err)
	}
	return data, nil
}
