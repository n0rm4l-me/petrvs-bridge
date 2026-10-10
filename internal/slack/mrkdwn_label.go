package slack

import (
	"net/url"
	"regexp"
	"strings"
)

// A link whose label reads as an address of its own ([https://good.example](https://evil.example)) is how a link is made to say
// where it goes while going elsewhere. Slack shows only the label, so such a label is not trusted: if it names another host than
// the address does, the link is written without a label and Slack shows the address itself.

var mdAddressLabel = regexp.MustCompile(`(?i)^(?:[a-z][a-z0-9+.-]*://)?(?:[a-z0-9-]+\.)+[a-z]{2,}(?::\d+)?(?:[/?#]\S*)?$|^[a-z][a-z0-9+.-]*://\S+$|^www\.\S+$`)

// mdLabelLies says that the label looks like an address and names another host than dest (the address of the link) does.
func mdLabelLies(label, dest string) bool {
	label = strings.TrimSpace(label)
	if label == "" || strings.ContainsAny(label, " \t\n") || !mdAddressLabel.MatchString(label) {
		return false
	}
	if strings.Contains(label, "://") && strings.Contains(label, "@") {
		return true // https://good.example@evil.example: the part before the @ is not the host, and reads as one
	}
	return mdHost(label) != mdHost(dest)
}

// mdHost is the host of an address or of a label that looks like one: lower case, without a leading www., a port or a user part.
// What cannot be read gives "", which differs from any real host, so that an odd label is not trusted either.
func mdHost(a string) string {
	if !strings.Contains(a, "://") {
		a = "https://" + a
	}
	u, err := url.Parse(a)
	if err != nil {
		return ""
	}
	h := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	return strings.TrimSuffix(h, ".")
}
