package testrig

import (
	"fmt"
	"strings"

	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
)

// PressOn taps a button of the chat message with this id, as the authorised user. Press, which names no message, cannot
// tell the hub which message to take the buttons off.
func (r *Rig) PressOn(messageID int64, data string) { r.PressAs(ChatID, UserID, messageID, data) }

// PressAs taps a button of a message as someone else.
func (r *Rig) PressAs(chat, user, messageID int64, data string) {
	r.TG.Push(telegram.Update{ID: nextID(), ChatID: chat, UserID: user, MessageID: messageID, CallbackID: fmt.Sprintf("cb%d", nextID()), CallbackData: data})
}

// SentWithButtons returns the messages the hub sent that have buttons, oldest first.
func (r *Rig) SentWithButtons() []telegram.Sent {
	var out []telegram.Sent
	for _, s := range r.TG.Sent() {
		if len(s.Buttons) > 0 {
			out = append(out, s)
		}
	}
	return out
}

// WaitButtons waits for a message with buttons whose text contains sub, and returns it.
func (r *Rig) WaitButtons(sub string) telegram.Sent {
	r.T.Helper()
	var found telegram.Sent
	r.Eventually(fmt.Sprintf("a message with buttons containing %q", sub), func() bool {
		for _, s := range r.SentWithButtons() {
			if strings.Contains(s.Text, sub) {
				found = s
				return true
			}
		}
		return false
	})
	return found
}
