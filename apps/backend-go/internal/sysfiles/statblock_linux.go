package sysfiles

import "syscall"

func statBlockSize(sf *syscall.Statfs_t) uint64 {
	if sf.Frsize != 0 {
		return uint64(sf.Frsize)
	}
	return uint64(sf.Bsize)
}
