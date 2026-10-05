//go:build linux

package process

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// hasProcScan reports that processes can be enumerated through /proc.
const hasProcScan = true

func setPdeathsig(attr *syscall.SysProcAttr, on bool) {
	if on {
		attr.Pdeathsig = syscall.SIGKILL
	}
}

// hardenParent clears PR_SET_DUMPABLE. A non-dumpable process's /proc entries
// are owned by root, and it cannot be ptraced, so plugins running as the same
// user cannot read the worker's environment or memory. Children are not
// affected: execve makes an ordinary binary dumpable again.
func hardenParent() error {
	const prSetDumpable = 4
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return errno
	}
	return nil
}

type procInfo struct {
	ppid, pgrp int
	state      byte
	start      uint64
}

// readStat parses /proc/<pid>/stat. The command name may contain spaces and
// parentheses, so fields are located after the last ')'.
func readStat(pid int) (procInfo, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procInfo{}, false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return procInfo{}, false
	}
	f := strings.Fields(s[i+2:]) // f[0]=state f[1]=ppid f[2]=pgrp ... f[19]=starttime
	if len(f) < 20 {
		return procInfo{}, false
	}
	ppid, e1 := strconv.Atoi(f[1])
	pgrp, e2 := strconv.Atoi(f[2])
	start, e3 := strconv.ParseUint(f[19], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return procInfo{}, false
	}
	return procInfo{ppid: ppid, pgrp: pgrp, state: f[0][0], start: start}, true
}

// procStart returns a process's start time in clock ticks since boot, which
// together with the pid identifies it even after PID reuse.
func procStart(pid int) (uint64, bool) {
	st, ok := readStat(pid)
	return st.start, ok
}

func procTable() map[int]procInfo {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make(map[int]procInfo, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if st, ok := readStat(pid); ok {
			out[pid] = st
		}
	}
	return out
}

// treeMembers returns the live (non-zombie) processes that belong to a run:
// members of its process group, processes carrying its environment marker and
// every descendant of those.
func treeMembers(pgid int, token string) []int {
	table := procTable()
	self := os.Getpid()
	members := map[int]bool{}
	for pid, st := range table {
		if pid == self || st.state == 'Z' || st.state == 'X' {
			continue
		}
		if pgid > 1 && st.pgrp == pgid {
			members[pid] = true
		}
	}
	if token != "" {
		for pid, st := range table {
			if members[pid] || pid == self || st.state == 'Z' || st.state == 'X' {
				continue
			}
			env, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
			if err == nil && envHasMarker(env, token) {
				members[pid] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for pid, st := range table {
			if !members[pid] && pid != self && st.state != 'Z' && st.state != 'X' && members[st.ppid] {
				members[pid] = true
				changed = true
			}
		}
	}
	out := make([]int, 0, len(members))
	for pid := range members {
		out = append(out, pid)
	}
	return out
}

// groupGone is true when treeMembers found nothing: zombies awaiting a
// reaper do not count.
func groupGone(int) bool { return true }

// procMatches reports whether pid is the process started at start ticks.
func procMatches(pid int, start uint64) bool {
	got, ok := procStart(pid)
	return ok && got == start
}
