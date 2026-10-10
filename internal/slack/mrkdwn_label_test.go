package slack

import "testing"

func TestALinkWhoseLabelNamesAnotherHostIsShownByItsAddress(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"[https://good.example](https://evil.example/x)", "<https://evil.example/x>"},
		{"[good.example](https://evil.example)", "<https://evil.example>"},
		{"[www.good.example/path](https://evil.example)", "<https://evil.example>"},
		{"[ftp://good.example](https://evil.example)", "<https://evil.example>"},
		{"[GOOD.example](https://evil.example)", "<https://evil.example>"},
		{"[good.example](https://good.example.evil.example)", "<https://good.example.evil.example>"},
		{"[https://good.example@evil.example](https://evil.example)", "<https://evil.example>"},
		// the label names the host of the address: it is kept
		{"[example.com](https://example.com/docs)", "<https://example.com/docs|example.com>"},
		{"[https://www.example.com/a](https://example.com/b)", "<https://example.com/b|https://www.example.com/a>"},
		// a label that is words is kept
		{"[the docs](https://example.com)", "<https://example.com|the docs>"},
		{"[good site](https://evil.example)", "<https://evil.example|good site>"},
	} {
		if got := FromMarkdown(c.in); got != c.want {
			t.Errorf("%q -> %q, want %q", c.in, got, c.want)
		}
	}
}
