package main

import "testing"

// Asking for help is not a failure.
func TestHelpExitsWithZero(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}, {"gen", "-h"}, {"gen", "--help"}, {"totp-secret", "-h"}} {
		if code := run(args); code != 0 {
			t.Errorf("run(%v) = %d, want 0", args, code)
		}
	}
}

func TestMistakesExitWithTwo(t *testing.T) {
	for _, args := range [][]string{nil, {"frobnicate"}, {"gen", "-frobnicate"}, {"totp-secret"}, {"totp-secret", "-frobnicate"}, {"fingerprint"}} {
		if code := run(args); code != 2 {
			t.Errorf("run(%v) = %d, want 2", args, code)
		}
	}
}
