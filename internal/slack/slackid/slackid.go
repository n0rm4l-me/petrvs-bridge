// Package slackid holds what the settings of the hub need to know about Slack without the client: the shape of ids and tokens, the
// emoji that have a name, the address of the API. It imports nothing but the standard library, so a program that only reads
// settings (the agent) does not link the client and its WebSocket library.
package slackid

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// DefaultAPIBase is where the Web API lives.
const DefaultAPIBase = "https://slack.com/api"

// The hub tells chats and users apart by number. Slack's ids are strings, so the client keeps the one chat and the one user that
// are allowed under the fixed numbers below, and gives every other channel and user a number of its own (never these two).
const (
	ChatID int64 = 1
	UserID int64 = 1
)

var (
	userIDRe        = regexp.MustCompile(`^[UW][A-Z0-9]{8,31}$`)
	channelIDRe     = regexp.MustCompile(`^D[A-Z0-9]{8,31}$`) // a direct message
	commandPrefixRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,9}$`)
)

// ValidUserID says that s can be the id of a Slack user (U... or W...).
func ValidUserID(s string) bool { return userIDRe.MatchString(s) }

// ValidChannelID says that s can be the id of a direct message (D...).
func ValidChannelID(s string) bool { return channelIDRe.MatchString(s) }

// ValidCommandPrefix says that s can be put before the names of the slash commands.
func ValidCommandPrefix(s string) bool { return commandPrefixRe.MatchString(s) }

// ValidBotToken and ValidAppToken check only the shape: xoxb- for the bot, xapp- for the app-level token of Socket Mode.
func ValidBotToken(s string) bool { return tokenShape(s, "xoxb-") }
func ValidAppToken(s string) bool { return tokenShape(s, "xapp-") }

func tokenShape(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix) || len(s) < len(prefix)+10 || len(s) > 400 {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r >= 0x7f {
			return false
		}
	}
	return true
}

// IsLoopback says that h is the name of this machine.
func IsLoopback(h string) bool { return h == "localhost" || h == "127.0.0.1" || h == "::1" }

// ValidAPIBase says that s can be the address of the Web API: https on slack.com (the tokens go there), or plain http to a loopback
// address (tests).
func ValidAPIBase(s string) bool {
	u, err := url.Parse(strings.TrimRight(s, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	switch u.Scheme {
	case "https":
		return (u.Port() == "" || u.Port() == "443") && (h == "slack.com" || strings.HasSuffix(h, ".slack.com"))
	case "http":
		return IsLoopback(h)
	}
	return false
}

// reactionNames are the emoji the hub may use as marks and the names Slack knows them by. Slack's API takes a name ("eyes"), not
// the character, and a name it does not know is refused for good, so the set is fixed here and the settings are checked against it
// before the hub starts.
var reactionNames = map[string]string{
	"👀": "eyes", "👍": "thumbsup", "👎": "thumbsdown", "✅": "white_check_mark", "❌": "x", "⚠": "warning", "🔥": "fire", "❤": "heart",
	"🎉": "tada", "🤔": "thinking_face", "👌": "ok_hand", "🙏": "pray", "👏": "clap", "🚀": "rocket", "⚡": "zap", "💯": "100",
	"🙈": "see_no_evil", "😱": "scream", "😢": "cry", "🤝": "handshake", "⏳": "hourglass_flowing_sand", "🛑": "octagonal_sign",
	"📨": "incoming_envelope", "📥": "inbox_tray", "🆗": "ok",
}

// ReactionName gives the Slack name of an emoji the hub may use as a mark. A variation selector after the emoji (as in ❤️) does not
// matter.
func ReactionName(emoji string) (string, bool) {
	emoji = strings.TrimSpace(strings.ReplaceAll(emoji, "\uFE0F", ""))
	n, ok := reactionNames[emoji]
	return n, ok
}

// Reactions lists the emoji that can be used as marks, in a fixed order, for the error that names the choices.
func Reactions() []string {
	out := make([]string, 0, len(reactionNames))
	for e := range reactionNames {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// ReactionNames lists the Slack names of the marks, sorted.
func ReactionNames() []string {
	out := make([]string, 0, len(reactionNames))
	for _, n := range reactionNames {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
