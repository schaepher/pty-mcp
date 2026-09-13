// internal/mcp/getuid_unix.go
//go:build !windows

package mcp

import "os"

// currentUID returns the real user ID on Unix.
func currentUID() int {
	return os.Getuid()
}
