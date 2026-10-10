package slack

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
)

// Manifest is the manifest of the Slack app for this hub, as JSON that Slack's "From a manifest" accepts. It is made from the
// commands the hub knows, so that the app and the hub cannot drift apart; deploy/slack/manifest.json is this output.
//
// The rights are the least the client uses: chat:write (messages, and buttons removed), im:history (the user's messages),
// im:write (to find the direct message, unless it is given), files:read and files:write, reactions:write and commands. Nothing
// reads channels, users or the workspace.
func Manifest(name, prefix string, cmds []telegram.Command) ([]byte, error) {
	if !ValidCommandPrefix(prefix) {
		return nil, fmt.Errorf("slack: bad command prefix %q", prefix)
	}
	type command struct {
		Command      string `json:"command"`
		Description  string `json:"description"`
		UsageHint    string `json:"usage_hint,omitempty"`
		ShouldEscape bool   `json:"should_escape"`
	}
	var slash []command
	for _, c := range cmds {
		if !slashNameRe.MatchString(prefix + c.Name) {
			return nil, fmt.Errorf("slack: %q is not a name Slack allows for a command", prefix+c.Name)
		}
		slash = append(slash, command{Command: "/" + prefix + c.Name, Description: cutRunes(strings.TrimSpace(c.Description), 200), ShouldEscape: false})
	}
	m := map[string]any{
		"display_information": map[string]any{
			"name":        name,
			"description": "A bridge between this chat and a terminal assistant (Claude Code in tmux) on one machine.",
		},
		"features": map[string]any{
			"app_home":       map[string]any{"home_tab_enabled": false, "messages_tab_enabled": true, "messages_tab_read_only_enabled": false},
			"bot_user":       map[string]any{"display_name": name, "always_online": false},
			"slash_commands": slash,
		},
		"oauth_config": map[string]any{
			"scopes": map[string]any{"bot": []string{"chat:write", "commands", "files:read", "files:write", "im:history", "im:write", "reactions:write"}},
		},
		"settings": map[string]any{
			"event_subscriptions":    map[string]any{"bot_events": []string{"message.im"}},
			"interactivity":          map[string]any{"is_enabled": true},
			"org_deploy_enabled":     false,
			"socket_mode_enabled":    true,
			"token_rotation_enabled": false,
		},
	}
	return json.MarshalIndent(m, "", "  ")
}
