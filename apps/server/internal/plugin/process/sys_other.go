//go:build unix && !linux

package process

import "syscall"

// Without /proc, tree tracking falls back to the process group.
const hasProcScan = false

func setPdeathsig(*syscall.SysProcAttr, bool) {}

func hardenParent() error { return nil }

func treeMembers(int, string) []int { return nil }

// groupGone checks the process group directly.
func groupGone(pgid int) bool {
	if pgid <= 1 {
		return true
	}
	err := syscall.Kill(-pgid, 0)
	return err != nil && err != syscall.EPERM
}

func procStart(int) (uint64, bool) { return 0, false }

func procMatches(pid int, _ uint64) bool { return pidAlive(pid) }
