package alarm

import (
	"os"
	"path/filepath"
	"testing"
)

// The lock treats a state it cannot check as locked; the alarm must do the same, or an unreadable state directory
// starts the agent with the alarm off.
func TestAnAlarmWhoseStateFileCannotBeCheckedStartsArmed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Sensors: []Sensor{&fake{name: "input"}}, StateFile: filepath.Join(dir, "armed")})
	if err != nil {
		t.Fatal(err)
	}
	if !a.Armed() {
		t.Fatal("the state file could not be checked and the alarm started disarmed")
	}
}

func TestAnAlarmWithoutAStateFileStartsDisarmed(t *testing.T) {
	a, err := New(Config{Sensors: []Sensor{&fake{name: "input"}}, StateFile: filepath.Join(t.TempDir(), "armed")})
	if err != nil {
		t.Fatal(err)
	}
	if a.Armed() {
		t.Fatal("no state file, yet the alarm started armed")
	}
}
