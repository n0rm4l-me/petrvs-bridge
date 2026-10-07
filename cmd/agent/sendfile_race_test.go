package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// realTemp is a temporary directory whose name has no links in it, so that the paths a test builds are the paths the
// system reports.
func realTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustWrite(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// A parent directory that is swapped, again and again, between a real directory and a link to ~/.aws makes the path pass
// the checks when it is a real directory and open the secret when it has become the link: O_NOFOLLOW covers only the last
// component. The file that is opened must be judged by where the open descriptor is, not by the path looked at before.
// (Before the fix a run of two seconds leaked hundreds of times.)
func TestADirectorySwappedForALinkBetweenTheCheckAndTheOpenIsNotReadThrough(t *testing.T) {
	root := realTemp(t)
	const secret = "SECRET-FROM-THE-AWS-DIRECTORY"
	aws := filepath.Dir(mustWrite(t, filepath.Join(root, ".aws", "x.json"), secret))
	real := filepath.Dir(mustWrite(t, filepath.Join(root, "real", "x.json"), "an ordinary file"))
	swap := filepath.Join(root, "swap")
	if err := os.Symlink(aws, swap); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the attacker: swap is a link, then a directory, then a link...
		defer wg.Done()
		isLink := true
		for {
			select {
			case <-stop:
				return
			default:
			}
			if isLink {
				os.Remove(swap)
				os.Rename(real, swap)
			} else {
				os.Rename(swap, real)
				os.Symlink(aws, swap)
			}
			isLink = !isLink
		}
	}()
	var leaks, read, refused int
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		data, _, err := sfRead(filepath.Join(swap, "x.json"), state, 1<<20, envOf(nil))
		switch {
		case err != nil:
			refused++
		case strings.Contains(string(data), secret):
			leaks++
		default:
			read++
		}
	}
	close(stop)
	wg.Wait()
	if leaks != 0 {
		t.Fatalf("%d reads returned the content of the .aws directory (%d ordinary reads, %d refusals)", leaks, read, refused)
	}
	if read == 0 || refused == 0 {
		t.Fatalf("the swapping did not happen as intended: %d ordinary reads, %d refusals", read, refused)
	}
}
