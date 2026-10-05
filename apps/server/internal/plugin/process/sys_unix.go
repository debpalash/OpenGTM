//go:build unix

package process

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// socketPair returns a connected unix stream pair: the host's net.Conn and
// the child's file, which becomes descriptor 3. Both ends are close-on-exec
// in the host; exec.Cmd.ExtraFiles clears the flag for the child's copy only.
func socketPair() (net.Conn, *os.File, error) {
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	parent := os.NewFile(uintptr(fds[0]), "plugin-host")
	child := os.NewFile(uintptr(fds[1]), "plugin-child")
	conn, err := net.FileConn(parent) // dups the descriptor
	_ = parent.Close()
	if err != nil {
		_ = child.Close()
		return nil, nil, fmt.Errorf("control socket: %w", err)
	}
	return conn, child, nil
}

// prepareCmd puts the plugin in its own process group, so the whole tree can
// be signalled, and gives it no stdin.
func prepareCmd(cmd *exec.Cmd, pdeathsig bool) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setPdeathsig(cmd.SysProcAttr, pdeathsig)
	cmd.Stdin = nil // /dev/null
}

// killTree kills a plugin's whole process tree. While the leader has not been
// reaped its pid cannot be recycled, so the process group is signalled
// directly. In all cases every process found by group membership, parent
// links or (Linux) the run's environment marker is killed too, which catches
// grandchildren that called setsid() or were re-parented to init. It returns
// when nothing is left or after a short deadline.
func killTree(pgid int, token string, leaderReaped bool) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		if !leaderReaped || !hasProcScan {
			if pgid > 1 {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}
		pids := treeMembers(pgid, token)
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		if len(pids) == 0 && groupGone(pgid) {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// pidAlive reports whether pid exists.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// envHasMarker reports whether a NUL-separated environ blob contains the run
// marker.
func envHasMarker(environ []byte, token string) bool {
	want := []byte(MarkerEnv + "=" + token)
	for _, kv := range bytes.Split(environ, []byte{0}) {
		if bytes.Equal(kv, want) {
			return true
		}
	}
	return false
}

// exitStatus extracts the wait status of an exited process.
func exitStatus(ee *exec.ExitError) (syscall.WaitStatus, bool) {
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ws, ok
}
