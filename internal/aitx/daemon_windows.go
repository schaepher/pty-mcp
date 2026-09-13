// internal/aitx/daemon_windows.go
//go:build windows

package aitx

import "syscall"

// newDaemonAttr returns process attributes that let the spawned ai-tmux
// server outlive the client, mirroring the Unix Setsid behavior.
func newDaemonAttr() *syscall.SysProcAttr {
	const createNewProcessGroup = 0x00000200
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
}
