package sysfiles

import "syscall"

func statBlockSize(sf *syscall.Statfs_t) uint64 {
	return uint64(sf.Bsize)
}
