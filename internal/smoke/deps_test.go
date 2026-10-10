package smoke

import (
	"os/exec"
	"strings"
	"testing"
)

// The agent runs on a laptop and has no use for the Slack client or its WebSocket library: the settings only need the leaf package.
func TestTheAgentDoesNotLinkTheSlackClient(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/n0rm4l-me/petrvs-bridge/cmd/agent").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	for _, line := range strings.Fields(string(out)) {
		if line == "github.com/n0rm4l-me/petrvs-bridge/internal/slack" || strings.Contains(line, "coder/websocket") {
			t.Errorf("the agent links %s", line)
		}
	}
}
