// internal/aitx/socketpath_unix.go
//go:build !windows

package aitx

import (
	"fmt"
	"os"
)

// defaultSocketPath keeps the historical Unix socket location, scoped to the
// current user, so existing running servers remain discoverable.
func defaultSocketPath() string {
	return fmt.Sprintf("/tmp/ai-tmux-%d.sock", os.Getuid())
}
