package slack

import "github.com/n0rm4l-me/petrvs-bridge/internal/slack/slackid"

// ReactionName gives the Slack name of an emoji the hub may use as a mark (see slackid).
func ReactionName(emoji string) (string, bool) { return slackid.ReactionName(emoji) }

// Reactions lists the emoji that can be used as marks.
func Reactions() []string { return slackid.Reactions() }
