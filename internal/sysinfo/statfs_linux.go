package sysinfo

import "syscall"

func statfs(path string) (free, total uint64, ok bool) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, 0, false
	}
	bs := uint64(s.Frsize) // the unit of the block counts; Bsize is only the best size for transfers
	if bs == 0 {
		bs = uint64(s.Bsize)
	}
	return uint64(s.Bavail) * bs, uint64(s.Blocks) * bs, true
}
