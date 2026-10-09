//go:build windows

package cli

// openNoFollow is not available here; the fstat identity check still refuses
// a file swapped between the check and the open.
const openNoFollow = 0
