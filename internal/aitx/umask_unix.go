// internal/aitx/umask_unix.go
//go:build !windows

package aitx

import "syscall"

// setUmask sets the process file-creation mask and returns the previous value.
func setUmask(mask int) int {
	return syscall.Umask(mask)
}
