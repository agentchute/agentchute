//go:build linux

package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// platformParentPID reads field 4 of /proc/<pid>/stat. The comm field (2) is
// parenthesized and may itself contain spaces or ')', so parse after the LAST
// ')'.
func platformParentPID(pid int) (int, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	s := string(data)
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return 0, fmt.Errorf("/proc/%d/stat: no comm field", pid)
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("/proc/%d/stat: short record", pid)
	}
	return strconv.Atoi(fields[1])
}
