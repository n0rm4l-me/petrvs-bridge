package slack

import (
	"net/url"
	"strings"
	"unicode"
)

// Slack shows only the label of <address|label>, so a label can say one place and the address go to another. Judging whether a
// label is deceptive does not work (it can be bold, in code, in brackets, with a zero-width space in it, or a sentence that
// contains a host), so the rule does not try: unless the label already contains the host of the address, the host is added after
// it, "label (host)", where the label cannot hide it.

// mdShowHost gives the label that is shown for a link to dest: the label, and the host of dest in brackets when the label does not
// hold it. An address without a host (mailto:) is shown as it is.
func mdShowHost(label, dest string) string {
	host := mdHost(dest)
	if host == "" {
		return label
	}
	if mdNamesHost(strings.ToLower(mdVisible(label)), host) {
		return label
	}
	return label + " (" + Escape(host) + ")" // the host comes from the address and can hold an &, which Slack reads
}

// mdNamesHost says that text holds host as a name of its own, not as the tail of another name or after an @ (good.example@host is
// a user part, and example.com.evil.example is another host).
func mdNamesHost(text, host string) bool {
	for from := 0; ; {
		i := strings.Index(text[from:], host)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(host)
		before := i == 0 || !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789.@_-", rune(text[i-1]))
		after := end == len(text) || !(strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789_-@", rune(text[end])) ||
			(text[end] == '.' && end+1 < len(text) && strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789", rune(text[end+1]))))
		if before && after {
			return true
		}
		from = i + 1
	}
}

// mdVisible is the text of a label as a person reads it: no invisible characters (zero-width, bidi marks and the like), so that
// a host cannot be spelled across them to look present, or hidden among them.
func mdVisible(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) || unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// mdHost is the host of an address: lower case, without a leading www., without a port or a user part, and without a trailing dot.
// What cannot be read gives "".
func mdHost(a string) string {
	if !strings.Contains(a, "://") {
		return "" // mailto:
	}
	u, err := url.Parse(a)
	if err != nil {
		return ""
	}
	h := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	return strings.TrimSuffix(h, ".")
}
