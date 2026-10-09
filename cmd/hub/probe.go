package main

import (
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/health"
)

// probe is `hub probe live|ready`: what the container runtime runs as an exec probe. It reads the files that the running hub
// touches (see internal/health) and says nothing but the reason of a failure. It needs no settings of the hub: only the
// directory, from -dir or PB_HEALTH_DIR.
//
// Exit codes: 0 the check passed, 1 it failed, 2 the command was used wrongly.
func probe(args []string, getenv func(string) string, now time.Time, stderr io.Writer) int {
	fs := flag.NewFlagSet("hub probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", getenv("PB_HEALTH_DIR"), "directory of the health files (env PB_HEALTH_DIR)")
	maxAge := fs.Duration("max-age", 30*time.Second, "how old a health file may be; the hub touches them every 5 seconds")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *dir == "" || *maxAge <= 0 {
		fmt.Fprintln(stderr, "usage: hub probe [-dir DIR] [-max-age 30s] live|ready  (DIR is PB_HEALTH_DIR by default)")
		return 2
	}
	if err := health.Check(*dir, fs.Arg(0), *maxAge, now); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
