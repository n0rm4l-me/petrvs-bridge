package slack

import "testing"

func TestALinkShowsWhereItGoesWhateverItsLabelSays(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		// a label that names another place (the known ways of hiding it, and more)
		{"[https://good.example](https://evil.example/x)", "<https://evil.example/x|https://good.example (evil.example)>"},
		{"[`slack.com`](https://evil.example)", "<https://evil.example|slack.com (evil.example)>"},
		{"[**slack.com**](https://evil.example)", "<https://evil.example|slack.com (evil.example)>"},
		{"[_slack.com_](https://evil.example)", "<https://evil.example|slack.com (evil.example)>"},
		{`[slack\.com](https://evil.example)`, "<https://evil.example|slack.com (evil.example)>"},
		{"[slack.com\u200b](https://evil.example)", "<https://evil.example|slack.com\u200b (evil.example)>"},
		{"[(slack.com)](https://evil.example)", "<https://evil.example|(slack.com) (evil.example)>"},
		{"[Sign in to slack.com](https://evil.example)", "<https://evil.example|Sign in to slack.com (evil.example)>"},
		{"[good site](https://evil.example)", "<https://evil.example|good site (evil.example)>"},
		{"[https://good.example@evil.example](https://evil.example)", "<https://evil.example|https://good.example@evil.example (evil.example)>"},
		// a label that holds the host is left alone; so is a bare address, and an address that has no host
		{"[example.com](https://example.com/docs)", "<https://example.com/docs|example.com>"},
		{"[EXAMPLE.com docs](https://www.example.com/docs)", "<https://www.example.com/docs|EXAMPLE.com docs>"},
		{"[https://example.com/a](https://example.com/a)", "<https://example.com/a>"},
		{"[mail me](mailto:a@b.example)", "<mailto:a@b.example|mail me>"},
	} {
		if got := FromMarkdown(c.in); got != c.want {
			t.Errorf("%q\n  -> %q\n want %q", c.in, got, c.want)
		}
	}
}

func TestTheHostAddedToALabelIsEscaped(t *testing.T) {
	// found by the fuzzer: the host of http://& is &
	if got := FromMarkdown("[0](http://&)"); got != "<http://&amp;|0 (&amp;)>" {
		t.Errorf("%q", got)
	}
}

func TestAZeroWidthSpaceDoesNotHideOrFakeAHostInALabel(t *testing.T) {
	// the host is spelled across a zero-width space: it does not count as present, so the real host is added
	got := FromMarkdown("[evil.\u200bexample](https://evil.example)")
	if got != "<https://evil.example|evil.\u200bexample>" { // the visible text is evil.example, which is the host: nothing to add
		t.Errorf("%q", got)
	}
	got = FromMarkdown("[good.\u200bexample](https://evil.example)")
	if got != "<https://evil.example|good.\u200bexample (evil.example)>" {
		t.Errorf("%q", got)
	}
}
