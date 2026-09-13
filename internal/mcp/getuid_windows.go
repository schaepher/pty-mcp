// internal/mcp/getuid_windows.go
//go:build windows

package mcp

// currentUID has no Windows equivalent; callers only use it to build a
// Linux XDG_RUNTIME_DIR path, which is unreachable on Windows.
func currentUID() int {
	return -1
}
