package slack

import "strings"

// mdStripBidi removes the characters that change the direction of the text around them: the marks, embeddings, overrides and
// isolates (U+200E, U+200F, U+061C, U+202A to U+202E, U+2066 to U+2069). In text that an agent writes they have no honest use,
// and an override can make the text after it, such as the address that follows a label, read backwards as another name. (The
// joiners of emoji sequences are not touched.)
func mdStripBidi(s string) string {
	if !strings.ContainsFunc(s, mdIsBidi) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if mdIsBidi(r) {
			return -1
		}
		return r
	}, s)
}

func mdIsBidi(r rune) bool {
	return r == 0x200e || r == 0x200f || r == 0x061c || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}
