package slack

import "strings"

// reactionNames are the emoji the hub may use as marks and the names Slack knows them by. Slack's API takes a name ("eyes"),
// not the character, and a name it does not know is refused for good, so the set is fixed here and the settings are checked
// against it before the hub starts.
var reactionNames = map[string]string{
	"👀": "eyes",
	"👍": "thumbsup",
	"👎": "thumbsdown",
	"✅": "white_check_mark",
	"❌": "x",
	"⚠": "warning",
	"🔥": "fire",
	"❤": "heart",
	"🎉": "tada",
	"🤔": "thinking_face",
	"👌": "ok_hand",
	"🙏": "pray",
	"👏": "clap",
	"🚀": "rocket",
	"⚡": "zap",
	"💯": "100",
	"🙈": "see_no_evil",
	"😱": "scream",
	"😢": "cry",
	"🤝": "handshake",
	"⏳": "hourglass_flowing_sand",
	"🛑": "octagonal_sign",
	"📨": "incoming_envelope",
	"📥": "inbox_tray",
	"🆗": "ok",
}

// ReactionName gives the Slack name of an emoji the hub may use as a mark. A variation selector after the emoji (as in ❤️) does
// not matter.
func ReactionName(emoji string) (string, bool) {
	emoji = strings.TrimSpace(strings.ReplaceAll(emoji, "\uFE0F", ""))
	n, ok := reactionNames[emoji]
	return n, ok
}

// Reactions lists the emoji that can be used as marks, for the error that names the choices.
func Reactions() []string {
	out := make([]string, 0, len(reactionNames))
	for e := range reactionNames {
		out = append(out, e)
	}
	return out
}
