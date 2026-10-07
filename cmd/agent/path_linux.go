package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// fdPath says where the open file really is: the kernel's own answer for the descriptor, not a path that was looked at
// earlier. opened is the path it was opened by, which only platforms without an answer use.
func fdPath(f *os.File, opened string) (string, error) {
	var fd int
	// SyscallConn, not Fd: Fd would put a descriptor opened without blocking back into blocking mode.
	rc, err := f.SyscallConn()
	if err != nil {
		return "", err
	}
	if err := rc.Control(func(n uintptr) { fd = int(n) }); err != nil {
		return "", err
	}
	p, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return "", err
	}
	// A file that was removed after it was opened is reported with this added to its name, which would hide the name.
	if !filepath.IsAbs(p) || strings.HasSuffix(p, " (deleted)") {
		return "", errors.New("the file is no longer where it was opened")
	}
	return p, nil
}
