package hubsvc_test

import (
	"strings"
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// What an agent says reaches the chat as Markdown (shown formatted); what the hub says itself stays plain text.
func TestAnAgentsReplyIsSentAsMarkdownAndTheHubsOwnWordsAreNot(t *testing.T) {
	r := testrig.New(t, testrig.Options{})
	r.StartAll()
	r.Agents["laptop"].Spool.Add(wire.TypeReply, "**done** with `code`")
	r.Eventually("the reply in the chat", func() bool { return count(r, "done") >= 1 })
	var reply, own *telegram.Sent
	for _, s := range r.TG.Sent() {
		s := s
		switch {
		case strings.Contains(s.Text, "**done**"):
			reply = &s
		case strings.Contains(s.Text, "connected"):
			own = &s
		}
	}
	if reply == nil || !reply.Markdown {
		t.Fatalf("the agent's reply was not sent as Markdown: %+v", reply)
	}
	if own != nil && own.Markdown {
		t.Fatalf("a message of the hub's own was sent as Markdown: %+v", own)
	}
}
