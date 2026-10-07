package main

import "testing"

func noEnv(string) string { return "" }

// Asking for help is not a configuration problem: exit 0, before anything is read or started.
func TestHelpExitsWithZero(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "-help"} {
		if code := run([]string{arg}, noEnv); code != 0 {
			t.Errorf("run(%s) = %d, want 0", arg, code)
		}
	}
}

func TestAnUnknownFlagAndAMissingSettingExitWithTwo(t *testing.T) {
	if code := run([]string{"-frobnicate"}, noEnv); code != 2 {
		t.Errorf("unknown flag: %d, want 2", code)
	}
	if code := run(nil, noEnv); code != 2 {
		t.Errorf("empty configuration: %d, want 2", code)
	}
}
