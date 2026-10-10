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
		return withLabel(strings.TrimPrefix(body, "mailto:"), label, hasLabel)
	case strings.HasPrefix(body, "http://"), strings.HasPrefix(body, "https://"), strings.HasPrefix(body, "tel:"):
		return withLabel(body, label, hasLabel)
	}
	return body
}

// withLabel gives the text of a link as the user wrote it. Slack writes a bare name that looks like an address (setup.py, README.md)
// as <http://setup.py|setup.py>: the label is what was typed, and the address only what Slack made of it, so the label is the text.
// A link with another label (a pasted rich link) is the label and, so that nothing is hidden, the address in brackets.
func withLabel(address, label string, hasLabel bool) string {
	if !hasLabel || label == "" {
		return address
	}
	bare := address
	for _, scheme := range []string{"https://", "http://", "tel:"} {
		bare = strings.TrimPrefix(bare, scheme)
	}
	if strings.EqualFold(strings.TrimRight(label, "/"), strings.TrimRight(bare, "/")) {
		return label
	}
	return label + " (" + address + ")"
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
