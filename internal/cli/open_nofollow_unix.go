//go:build !windows

package cli

import "syscall"

// openNoFollow makes an open fail when the last path component is a symlink.
const openNoFollow = syscall.O_NOFOLLOW
