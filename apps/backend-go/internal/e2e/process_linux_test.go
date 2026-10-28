//go:build integration

package e2e

import "syscall"

func childProcessAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
