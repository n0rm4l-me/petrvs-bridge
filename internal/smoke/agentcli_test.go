package smoke

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runEmpty runs the built agent under `env -i`: nothing but the given variables, as in a hook whose shell has none of the
// agent's settings. It returns standard output, standard error and the exit code.
func runEmpty(t *testing.T, bin, stdin string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("env", append(append([]string{"-i"}, env...), append([]string{bin}, args...)...)...)
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

func spooled(t *testing.T, dir string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

// `agent send` runs in Claude Code's hooks and `agent unlock` is the way out of a lockdown: neither may depend on the
// settings that only `agent run` needs (PB_PROJECT, PB_NAME, the identity files, the alarm secret).
func TestSendAndUnlockNeedNoMoreThanTheStateDirectory(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agent")
	must(t, exec.Command("go", "build", "-o", bin, "github.com/n0rm4l-me/petrvs-bridge/cmd/agent"))

	t.Run("send with only PB_STATE_DIR", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "state")
		_, errs, code := runEmpty(t, bin, "the answer\n", []string{"PB_STATE_DIR=" + state}, "send", "-")
		got := spooled(t, filepath.Join(state, "spool"))
		if code != 0 || len(got) != 1 || !strings.Contains(got[0], `"the answer"`) {
			t.Fatalf("exit %d, spooled %q, stderr %q", code, got, errs)
		}
	})

	t.Run("send with the default state directory and no PB_PROJECT", func(t *testing.T) {
		home := t.TempDir()
		_, errs, code := runEmpty(t, bin, "from a hook\n", []string{"HOME=" + home}, "send", "-type", "notice", "-")
		got := spooled(t, filepath.Join(home, ".local", "state", "petrvs-bridge", "spool"))
		if code != 0 || len(got) != 1 || !strings.Contains(got[0], `"from a hook"`) || !strings.Contains(got[0], `"notice"`) {
			t.Fatalf("exit %d, spooled %q, stderr %q", code, got, errs)
		}
	})

	t.Run("send does not read the alarm secret", func(t *testing.T) {
		state := t.TempDir()
		env := []string{"HOME=" + t.TempDir(), "PB_PROJECT=p", "PB_ALARM_SECRET_FILE=" + filepath.Join(state, "no-such-file")}
		if _, errs, code := runEmpty(t, bin, "x\n", env, "send", "-"); code != 0 {
			t.Fatalf("exit %d: %s", code, errs)
		}
	})

	t.Run("unlock with only PB_STATE_DIR", func(t *testing.T) {
		state := t.TempDir()
		lock := filepath.Join(state, "locked")
		if err := os.WriteFile(lock, []byte("locked by hub request\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, errs, code := runEmpty(t, bin, "", []string{"PB_STATE_DIR=" + state}, "unlock")
		if code != 0 || !strings.Contains(out, "unlocked") || !strings.Contains(out, lock) {
			t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
		}
		if _, err := os.Stat(lock); !os.IsNotExist(err) {
			t.Fatalf("the lock file is still there: %v", err)
		}
	})

	t.Run("unlock without a lock file fails and says where it looked", func(t *testing.T) {
		state := t.TempDir()
		out, errs, code := runEmpty(t, bin, "", []string{"PB_STATE_DIR=" + state}, "unlock")
		if code != 1 || strings.Contains(out, "unlocked") || !strings.Contains(errs, "no lock file at "+filepath.Join(state, "locked")) {
			t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
		}
	})

	t.Run("help is not a configuration problem", func(t *testing.T) {
		for _, args := range [][]string{{"-h"}, {"--help"}, {"run", "-h"}, {"unlock", "-h"}, {"send", "-h"}} {
			_, errs, code := runEmpty(t, bin, "", nil, args...)
			if code != 0 || strings.Contains(errs, "configuration problems") {
				t.Errorf("agent %v: exit %d, stderr %q", args, code, errs)
			}
		}
	})

	t.Run("missing settings for run are still reported", func(t *testing.T) {
		_, errs, code := runEmpty(t, bin, "", nil)
		if code != 2 || !strings.Contains(errs, "configuration problems") {
			t.Fatalf("exit %d, stderr %q", code, errs)
		}
	})
}
