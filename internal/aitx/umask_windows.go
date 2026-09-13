// internal/aitx/umask_windows.go
//go:build windows

package aitx

// setUmask is a no-op on Windows, which has no process umask.
func setUmask(mask int) int {
	return 0
}
