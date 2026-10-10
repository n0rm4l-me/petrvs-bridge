package slack

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// decodeText turns the text of a message that Slack delivers into the text the user typed. Slack escapes & < > as entities and
// writes everything it recognises (a mention, a channel, a link) between angle brackets, so a raw angle bracket in the text is
// Slack's and an entity is the user's. The angle-bracket forms are replaced first, the entities after, in one pass, so that a
// typed "&lt;!channel&gt;" ends up as that literal text and nothing is decoded twice.
func decodeText(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	s = strings.ReplaceAll(s, "\x00", "")
	s = angleRe.ReplaceAllStringFunc(s, func(m string) string { return decodeToken(m[1 : len(m)-1]) })
	return entities.Replace(s)
}

var (
	angleRe  = regexp.MustCompile(`<([^<>]{1,1000})>`)
	entities = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")
)

func decodeToken(inner string) string {
	body, label, hasLabel := strings.Cut(inner, "|")
	switch {
	case strings.HasPrefix(body, "@"): // a user: <@U123> or <@U123|name>
		if hasLabel && label != "" {
			return "@" + strings.TrimPrefix(label, "@")
		}
		return body
	case strings.HasPrefix(body, "#"): // a channel: <#C123|name>
		if hasLabel && label != "" {
			return "#" + strings.TrimPrefix(label, "#")
		}
		return body
	case strings.HasPrefix(body, "!"): // here, channel, everyone, a group, a date
		name, _, _ := strings.Cut(body[1:], "^")
		switch name {
		case "here", "channel", "everyone":
			return "@" + name
		}
		if hasLabel {
			return label
		}
		return ""
	case strings.HasPrefix(body, "mailto:"):
		if hasLabel && label != "" {
			return label
		}
		return strings.TrimPrefix(body, "mailto:")
	case strings.HasPrefix(body, "http://"), strings.HasPrefix(body, "https://"), strings.HasPrefix(body, "tel:"):
		return body // the address as it is; a label is only what Slack shows
	}
	return body
}

// cleanText removes what must not reach a terminal from text that is passed on: the escape character and other control
// characters except the line feed and the tab. (The agent filters again; this is the first wall.)
func cleanText(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}
