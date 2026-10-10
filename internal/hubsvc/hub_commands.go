package hubsvc

import "github.com/n0rm4l-me/petrvs-bridge/internal/telegram"

// ChatCommands lists every command the hub understands from the chat, with a line on what it does, for a chat that has to be
// told the commands in advance (Slack registers them with the app). The names have no leading slash. Telegram's own menu
// (fullMenu) is shorter on purpose, since a tap there sends the bare command; here a command can carry its argument.
func ChatCommands() []telegram.Command {
	out := append([]telegram.Command(nil), fullMenu()...)
	have := map[string]bool{}
	for _, c := range out {
		have[c.Name] = true
	}
	for _, c := range []telegram.Command{
		{Name: "agents", Description: "Agents: online or offline, last heard"},
		{Name: "up", Description: "Press the Up arrow in the session"},
		{Name: "down", Description: "Press the Down arrow in the session"},
		{Name: "tab", Description: "Press Tab in the session"},
		{Name: "key", Description: "Press a key by number or name: /key 2, /key btab"},
		{Name: "clear", Description: "Clear the session: types Claude Code's own clear command"},
		{Name: "compact", Description: "Compact the session: types Claude Code's own compact command"},
		{Name: "resume", Description: "Resume an older session: types Claude Code's own resume command"},
		{Name: "continue", Description: "Start Claude Code again on the machine; /continue NAME for one agent"},
	} {
		if !have[c.Name] {
			out = append(out, c)
		}
	}
	return out
}
