// internal/aitx/socketpath_windows.go
//go:build windows

package aitx

import (
	"os"
	"path/filepath"
)

// defaultSocketPath uses the per-user temp directory on Windows.
func defaultSocketPath() string {
	return filepath.Join(os.TempDir(), "ai-tmux.sock")
}
