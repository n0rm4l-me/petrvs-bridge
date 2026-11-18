package worker

import (
	"strings"
	"testing"
)

// The example files in examples/ are the ones people copy: they must load, and say what they claim.
func TestTheExamplePolicyLoadsAndBehaves(t *testing.T) {
	p, err := LoadPolicy("../../examples/worker-policy.json")
	if err != nil {
		t.Fatal(err)
	}
	bash := func(cmd string) map[string]any { return map[string]any{"command": cmd} }
	for _, c := range []struct {
		cmd  string
		want Action
	}{
		{"git status", Allow}, {"git diff HEAD~1", Allow}, {"go test ./...", Allow}, {"make test", Allow},
		{"git status; rm -rf x", Ask}, {"git push origin main", Ask}, {"kubectl apply -f x", Ask}, {"curl https://example.com", Ask},
		{"rm -rf /", Deny},
	} {
		if d := p.Decide("Bash", bash(c.cmd)); d.Action != c.want {
			t.Errorf("%q: %+v, want %d", c.cmd, d, c.want)
		}
	}
	if d := p.Decide("Write", map[string]any{"file_path": "/home/me/.ssh/config"}); d.Risk == "" {
		t.Errorf("a write to .ssh is not flagged: %+v", d)
	}
}

func TestTheExamplePromptTellsHowToAnswerThroughTheChat(t *testing.T) {
	b, err := readFile("../../examples/worker-telegram-prompt.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"agent send-file", "-voice", "/clear", "Never print secrets", "## No interaction", "Never write a table", "under 34 characters", "one complete answer"} {
		if !strings.Contains(b, want) {
			t.Errorf("the example prompt does not mention %q", want)
		}
	}
}
