//go:build integration

package e2e

import "syscall"

func childProcessAttrs() *syscall.SysProcAttr {
	// macOS has no parent-death signal; the harness cleanup terminates children.
	return &syscall.SysProcAttr{}
}
