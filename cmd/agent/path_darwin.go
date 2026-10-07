package main

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fdPath says where the open file really is: the kernel's own answer for the descriptor (F_GETPATH), not a path that was
// looked at earlier. opened is the path it was opened by, which only platforms without an answer use.
func fdPath(f *os.File, opened string) (string, error) {
	rc, err := f.SyscallConn() // not Fd, which would put a descriptor opened without blocking back into blocking mode
	if err != nil {
		return "", err
	}
	buf := make([]byte, unix.PathMax)
	var errno unix.Errno
	if err := rc.Control(func(fd uintptr) {
		// unix has no helper that takes a buffer for F_GETPATH; the pointer is converted inside the call expression, as
		// the unsafe rules require.
		_, _, errno = unix.Syscall(unix.SYS_FCNTL, fd, unix.F_GETPATH, uintptr(unsafe.Pointer(&buf[0])))
	}); err != nil {
		return "", err
	}
	if errno != 0 {
		return "", errno
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	p := string(buf[:n])
	if !filepath.IsAbs(p) {
		return "", errors.New("the file is no longer where it was opened")
	}
	return p, nil
}
