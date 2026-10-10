package slack

import (
	"strings"
	"testing"
)

func TestSlackTextBecomesWhatTheUserTyped(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"hello", "hello"},
		{"a &lt; b &amp;&amp; c &gt; d", "a < b && c > d"},
		{"see <https://example.com/a?x=1&amp;y=2>", "see https://example.com/a?x=1&y=2"},
		{"see <https://example.com|the site>", "see the site (https://example.com)"},
		{"fix <http://setup.py|setup.py> and <http://README.md|README.md>", "fix setup.py and README.md"},
		{"open <https://example.com/a/|example.com/a> now", "open example.com/a now"},
		{"<https://example.com/|EXAMPLE.com> ok", "EXAMPLE.com ok"},
		{"<https://good.example|https://evil.example>", "https://evil.example (https://good.example)"},
		{"mail <mailto:a@b.example|a@b.example>", "mail a@b.example"},
		{"mail <mailto:a@b.example|click me>", "mail click me (a@b.example)"},
		{"hey <@U123ABC> and <@U123ABC|petr>", "hey @U123ABC and @petr"},
		{"in <#C123ABC|general>", "in #general"},
		{"<!here> <!channel> <!everyone>", "@here @channel @everyone"},
		{"<!subteam^S123|@devs> go", "@devs go"},
		{"<!date^1700000000^{date}|Nov 14>", "Nov 14"},
		// A typed literal comes through as typed, and is not decoded twice.
		{"&lt;!channel&gt;", "<!channel>"},
		{"&amp;lt;", "&lt;"},
		{"&amp;amp;", "&amp;"},
		{"x \x00 y", "x  y"},
	} {
		if got := decodeText(c.in); got != c.want {
			t.Errorf("%q -> %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDecodingIsBoundedAndNeverPanics(t *testing.T) {
	huge := strings.Repeat("<", 200000) + strings.Repeat("&lt;", 100000) + strings.Repeat("<@U1|a>", 50000)
	_ = decodeText(huge)
	_ = decodeText("<" + strings.Repeat("a", 5000) + ">")
	_ = decodeText("\xff\xfe<@U1>\xc3")
	if got := decodeText("\xff"); got != "�" {
		t.Errorf("a bad byte became %q", got)
	}
}

func TestControlCharactersAreRemovedBeforeTextIsPassedOn(t *testing.T) {
	if got := cleanText("a\x1b[31mb\x07\r\nc\td\x7fe\u0085f"); got != "a[31mb\nc\tdef" {
		t.Errorf("%q", got)
	}
	if got := cleanText("plain ünïcode 日本語 🙂"); got != "plain ünïcode 日本語 🙂" {
		t.Errorf("%q", got)
	}
}

func TestCutDoesNotSplitACharacter(t *testing.T) {
	if got := cut("héllo", 2); got != "h" {
		t.Errorf("%q", got)
	}
	if got := cut("日本語", 4); got != "日" {
		t.Errorf("%q", got)
	}
	if got := cut("abc", 10); got != "abc" {
		t.Errorf("%q", got)
	}
}

func FuzzDecodeText(f *testing.F) {
	for _, s := range []string{"", "plain", "<@U1>", "<!here>", "&lt;!channel&gt;", "<https://a|b>", "<<>>", "&amp;lt;", "\x00\xff<", "<" + strings.Repeat("a", 1100) + ">"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := decodeText(s)
		if len(out) > 3*len(s)+8 {
			t.Fatalf("%d bytes became %d", len(s), len(out))
		}
		if strings.ContainsRune(out, 0) {
			t.Fatalf("a NUL survived: %q", out)
		}
		clean := cleanText(out)
		for _, r := range clean {
			if r < 0x20 && r != '\n' && r != '\t' {
				t.Fatalf("a control character survived: %q", clean)
			}
		}
		if cleanText(clean) != clean {
			t.Fatalf("cleaning is not stable: %q", clean)
		}
	})
}
