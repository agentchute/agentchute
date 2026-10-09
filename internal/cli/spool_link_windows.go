//go:build windows

package cli

import "os"

// singleLink cannot read a link count here, so no file qualifies for the spool
// exemption: a failed send's spool is retried through stdin instead.
func singleLink(os.FileInfo) bool { return false }
