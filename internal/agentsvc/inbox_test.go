package agentsvc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The default inbox lies inside the project (<workdir>/.petrvs-inbox): the assistant's `git add -A` would commit the
// photos and files that came from the chat, so a new inbox ignores itself.
func TestANewInboxIgnoresItselfInGit(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "project", ".petrvs-inbox")
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	path, err := saveInbox(dir, "shot.png", "aabbccddeeff", []byte("png"), now)
	if err != nil {
		t.Fatal(err)
	}
	gi := filepath.Join(filepath.Dir(path), ".gitignore")
	if got, err := os.ReadFile(gi); err != nil || string(got) != "*\n" {
		t.Fatalf(".gitignore = %q, %v", got, err)
	}
	if fi, _ := os.Stat(gi); fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// the second file does not touch it
	if _, err := saveInbox(dir, "two.png", "aabbccddeeff", []byte("png"), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(gi); string(got) != "*\n" {
		t.Fatalf(".gitignore = %q", got)
	}
	// tidying never removes it, however old it is
	if err := tidyInbox(dir, now.Add(400*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(gi); err != nil {
		t.Fatalf("tidying removed the .gitignore: %v", err)
	}

	// and git really leaves the files alone
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git here")
	}
	project := filepath.Join(root, "project")
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=all")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("git sees the inbox: %v: %q", err, out)
	}
}

// An existing .gitignore is the user's; an inbox that was there before (it may be a directory of the user's own) is left
// as it is.
func TestAnExistingInboxAndItsGitignoreAreLeftAlone(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "inbox")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := saveInbox(dir, "a.png", "aabbccddeeff", []byte("x"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); err == nil {
		t.Fatal("a directory that was already there got a .gitignore")
	}

	// a directory with a .gitignore of the user's: it stays as it is
	dir2 := filepath.Join(t.TempDir(), "inbox")
	if err := os.Mkdir(dir2, 0o700); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(dir2, ".gitignore")
	if err := os.WriteFile(mine, []byte("*.tmp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := saveInbox(dir2, "a.png", "aabbccddeeff", []byte("x"), now); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(mine); string(got) != "*.tmp\n" {
		t.Fatalf(".gitignore = %q", got)
	}
	// keepOutOfGit itself never overwrites, and does not follow a link that is put in its place
	keepOutOfGit(dir2)
	if got, _ := os.ReadFile(mine); string(got) != "*.tmp\n" {
		t.Fatalf("overwritten: %q", got)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir3 := t.TempDir()
	if err := os.Symlink(victim, filepath.Join(dir3, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	keepOutOfGit(dir3)
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Fatalf("a link was followed: %q", got)
	}
}
