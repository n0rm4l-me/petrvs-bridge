//go:build !linux && !darwin

package main

import "os"

// fdPath on a system where the path of an open descriptor cannot be asked for: the path it was opened by stands in, so
// a directory that is swapped for a link between the checks and the open is not caught here.
func fdPath(f *os.File, opened string) (string, error) { return opened, nil }
