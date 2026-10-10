package slack

import (
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// mdLinkRe is a link as the converter writes it: angle brackets round an address and perhaps a label without <, > or |.
var mdLinkRe = regexp.MustCompile(`<(?:https?://|mailto:)[^\s<>|]+(?:\|[^<>|\n]*)?>`)

// mdCheck checks what must hold for every output, whatever went in. It returns "" when all is well.
func mdCheck(in, out string) string {
	if !utf8.ValidString(out) {
		return "invalid UTF-8"
	}
	for _, bad := range []string{"<!", "<@", "<#"} {
		if strings.Contains(out, bad) {
			return "contains " + bad
		}
	}
	// What is left when the links are taken out has no angle bracket but the quote mark at the start of a line.
	rest := mdLinkRe.ReplaceAllString(out, "")
	if strings.Contains(rest, "<") {
		return "a raw <"
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] == '>' && !((i == 0 || rest[i-1] == '\n') && (i+1 == len(rest) || rest[i+1] == ' ' || rest[i+1] == '\n')) {
			return "a raw > that is not a quote mark"
		}
	}
	if strings.Contains(strings.NewReplacer("&amp;", "", "&lt;", "", "&gt;", "").Replace(out), "&") {
		return "a raw &"
	}
	// Three backticks come out only as the whole line of a fence, and the fences come in pairs.
	fences := 0
	for _, l := range strings.Split(out, "\n") {
		switch t := strings.TrimPrefix(l, "> "); {
		case t == "```":
			fences++
		case strings.Contains(l, "```"):
			return fmt.Sprintf("three backticks in a line: %q", l)
		}
	}
	if fences%2 != 0 {
		return "a code block that is not closed"
	}
	if len(out) > 12*len(in)+64 {
		return fmt.Sprintf("the output is %d bytes for %d in", len(out), len(in))
	}
	for _, r := range "\u200b\u200c\u200d\u200e\u200f\u2060\ufeff" {
		if strings.ContainsRune(out, r) && !strings.ContainsRune(in, r) {
			return fmt.Sprintf("a zero width character %U was written", r)
		}
	}
	return ""
}

func TestMrkdwnEscape(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		"plain text":             "plain text",
		"a & b < c > d":          "a &amp; b &lt; c &gt; d",
		"*_~`[]|#\\ 汉字 é":        "*_~`[]|#\\ 汉字 é",
		"&amp;":                  "&amp;amp;",
		"<!channel>":             "&lt;!channel&gt;",
		"<https://x.example|go>": "&lt;https://x.example|go&gt;",
	} {
		if got := Escape(in); got != want {
			t.Errorf("%q\n got  %q\n want %q", in, got, want)
		}
	}
}

func TestMrkdwnInlineFormatting(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":                                   "plain text",
		"a **bold** word":                              "a *bold* word",
		"an *italic* word":                             "an _italic_ word",
		"an _italic_ word":                             "an _italic_ word",
		"~~gone~~":                                     "~gone~",
		"run `go test ./...` now":                      "run `go test ./...` now",
		"**bold** and *it* and ~~x~~":                  "*bold* and _it_ and ~x~",
		"**bold with `code` inside**":                  "*bold with `code` inside*",
		"***both***":                                   "*_both_*",
		"*it **bold** it*":                             "_it *bold* it_",
		"**bold *it* bold**":                           "*bold _it_ bold*",
		"~~**both**~~":                                 "~*both*~",
		"a < b && c > d":                               "a &lt; b &amp;&amp; c &gt; d",
		"escaped \\*stars\\* and \\<":                  "escaped *stars* and &lt;",
		"[the docs](https://example.com/a?b=1&c=2)":    "<https://example.com/a?b=1&amp;c=2|the docs>",
		"![pic](https://example.com/p.png)":            "<https://example.com/p.png|pic>",
		"[https://a.example](https://a.example)":       "<https://a.example>",
		"[HTTPS://A.example/X](HTTPS://A.example/X)":   "<https://A.example/X>",
		"mail [me](mailto:me@x.example)":               "mail <mailto:me@x.example|me>",
		"[x](https://a.example/p \"a title\")":         "<https://a.example/p|x>",
		"[w](https://en.wikipedia.org/wiki/Foo_(bar))": "<https://en.wikipedia.org/wiki/Foo_(bar)|w>",
		"**see [docs](https://a.example)**":            "*see <https://a.example|docs>*",
		"see [a](https://a.example), and [b](https://b.example/x_y_z).": "see <https://a.example|a>, and <https://b.example/x_y_z|b>.",
		"[a [b] c](https://x.example)":                                  "<https://x.example|a [b] c>",
		"[**bold** `code` _it_](https://x.example)":                     "<https://x.example|bold code it>",
		"[a [b](https://y.example) c](https://x.example)":               "<https://x.example|a [b](https://y.example) c>",
		"[a & b](https://x.example/?a=1&b=2)":                           "<https://x.example/?a=1&amp;b=2|a &amp; b>",
		"[a](https://ok.example)<!channel>":                             "<https://ok.example|a>&lt;!channel&gt;",
		"``a ` b``":                                                     "`a ˋ b`",
		"`` ``` ``":                                                     "`ˋˋˋ`",
		"`a<b>&c`":                                                      "`a&lt;b&gt;&amp;c`",
		"a ` ` b":                                                       "a   b",
		"`a`\\`\\`\\`":                                                  "`a`ˋˋˋ",
		"\\`":                                                           "ˋ",
		"x ``` y":                                                       "x ˋˋˋ y",
		"x ````` y `` z ` w":                                            "x ˋˋˋˋˋ y `` z ` w",
		"`a``b`":                                                        "`aˋˋb`",
		"[x](http://a.example/p) [y](file:///etc/passwd) [z](https://b.example)": "<http://a.example/p|x> [y](file:///etc/passwd) <https://b.example|z>",
	} {
		if got := FromMarkdown(in); got != want {
			t.Errorf("%q\n got  %q\n want %q", in, got, want)
		}
	}
}

// Text that only looks like Markdown stays as it is, and so does Markdown that is not closed: the marks are written as they
// were, and no span is made out of them.
func TestMrkdwnThingsThatAreNotFormattingAreLeftAlone(t *testing.T) {
	for _, in := range []string{
		"snake_case_name and my_file_name.go",
		"2*3*4 = 24",
		"a * b",
		"x * y * z",
		"1 * 2 + 3 * 4",
		"foo*bar*baz",
		"price: 5*",
		"under_score_ at the end",
		"*not closed",
		"**not closed",
		"***not closed",
		"`not closed",
		"~~not closed",
		"~ single ~ tildes",
		"**a*",
		"*a**",
		"** spaced **",
		"~~ spaced ~~",
		"_a",
		"a_",
		"**",
		"[not a link] (really)",
		"[a](b",
		"[a]",
		"[",
		"![",
		"[bad](javascript:alert(1))",
		"[bad](tg://resolve?domain=x)",
		"[x](file:///etc/passwd)",
		"[x](data:text/html;base64,AAAA)",
		"[x](ftp://a.example/f)",
		"[x](/relative/path)",
		"[x](//a.example/p)",
		"[x](http://)",
		"[x](https://)",
		"[x](mailto:)",
		"[x]()",
		"[](https://example.com)",
		"[ ](https://example.com)",
		"[x](https://a.example/p|q)",
		"[x](https://a.example/<b>)",
		"[x](<https://a.example>)",
		"[x](<!channel>)",
		"[x](https://a.example/a`b)",
		"[x](https://a.example/\x01)",
		"[x](https://a.example/\x7f)",
		"[x](https://a.example/\u200bb)",
		"[x](https://a.example/a\u00a0b)",
		"[x](https://a.example/\u2028)",
		"[x](https://a.example/a b)",
		"[x](https://a.example/p \"unclosed title)",
		"https://example.com/a_b_c is a link",
		"C:\\path\\to\\file",
		"__init__.py",
		"a __dunder__ word",
		"100% of 5 > 3",
		"a < b && c > d",
		"email me@example.com or @here or #general :smile:",
	} {
		got := FromMarkdown(in)
		if got != Escape(in) {
			t.Errorf("%q\n got  %q\n want %q", in, got, Escape(in))
		}
	}
}

// A mark inside a span of its own kind that Slack could take for the end of the span is a look-alike; one that Slack reads as
// text, between two letters or two spaces, is left alone.
func TestMrkdwnMarksInsideASpanOfTheirKind(t *testing.T) {
	for in, want := range map[string]string{
		"**a * b**":                    "*a * b*",
		"**a *b**":                     "*a ∗b*",
		"**snake_case and 2*3*4**":     "*snake_case and 2*3*4*",
		"*use my_var here*":            "_use my_var here_",
		"_a_b_":                        "_a_b_",
		"~~a~b~~":                      "~a~b~",
		"~~a ~b~~":                     "~a ∼b~",
		"# a **":                       "*a ∗∗*",
		"**a *b** c":                   "*a ∗b* c",
		"**a** and a lone * outside":   "*a* and a lone * outside",
		"**bold** then *a* and * lone": "*bold* then _a_ and * lone",
	} {
		if got := FromMarkdown(in); got != want {
			t.Errorf("%q\n got  %q\n want %q", in, got, want)
		}
	}
}

func TestMrkdwnLinkAddressLimits(t *testing.T) {
	base := "https://a.example/"
	ok := base + strings.Repeat("a", 2000-len(base))
	if got := FromMarkdown("[x](" + ok + ")"); got != "<"+ok+"|x>" {
		t.Errorf("an address of 2000 bytes: %.60q", got)
	}
	long := ok + "a"
	if got := FromMarkdown("[x](" + long + ")"); got != Escape("[x]("+long+")") {
		t.Errorf("an address of 2001 bytes became a link: %.60q", got)
	}
	// Non-ASCII letters are fine in an address.
	if got := FromMarkdown("[x](https://a.example/путь)"); got != "<https://a.example/путь|x>" {
		t.Errorf("%q", got)
	}
}

func TestMrkdwnLinkLabels(t *testing.T) {
	for in, want := range map[string]string{
		"[a|b](https://x.example)":                          "<https://x.example|a¦b>",
		"[a > b](https://x.example)":                        "<https://x.example|a &gt; b>",
		"[<!channel>](https://x.example)":                   "<https://x.example|&lt;!channel&gt;>",
		"[<#C123|name>](https://x.example)":                 "<https://x.example|&lt;#C123¦name&gt;>",
		"![<@U12345>](https://x.example)":                   "<https://x.example|&lt;@U12345&gt;>",
		"[<https://evil.example|click>](https://x.example)": "<https://x.example|&lt;https://evil.example¦click&gt;>",
	} {
		if got := FromMarkdown(in); got != want {
			t.Errorf("%q\n got  %q\n want %q", in, got, want)
		}
	}
}

func TestMrkdwnCodeBlocks(t *testing.T) {
	for in, want := range map[string]string{
		"before\n```go\nfmt.Println(\"<hi>\" & x)\n```\nafter": "before\n```\nfmt.Println(\"&lt;hi&gt;\" &amp; x)\n```\nafter",
		"```\nno language\n```":                                "```\nno language\n```",
		"```\"><script>\nx\n```":                               "```\nx\n```",
		"   ```py\nindented\n   ```":                           "```\nindented\n```",
		"~~~\nx\n~~~":                                          "```\nx\n```",
		// Nothing inside is Markdown, and a longer fence holds a shorter one, which is taken apart so that it cannot close.
		"````\n**not bold** ```\n````":       "```\n**not bold** ˋˋˋ\n```",
		"~~~\n```\n~~~":                      "```\nˋˋˋ\n```",
		"```\n`a` ``b`` `````c\n```":         "```\n`a` ``b`` ˋˋˋˋˋc\n```",
		"````\n```\n````":                    "```\nˋˋˋ\n```",
		"```\n<!channel> <@U1> <#C1|n>\n```": "```\n&lt;!channel&gt; &lt;@U1&gt; &lt;#C1|n&gt;\n```",
		// A block that is never closed ends with the text, and an empty one is nothing.
		"```\nopen":      "```\nopen\n```",
		"```\n\n```":     "",
		"x\n```\n```\ny": "x\ny",
		// The text after the closing fence is text again; a mention that tried to be in the block is not one.
		"```\nfoo\n```\n<!channel>\n```":  "```\nfoo\n```\n&lt;!channel&gt;",
		"```\n  keeps  the   indent\n```": "```\n  keeps  the   indent\n```",
		"```\r\nwindows\r\n```":           "```\nwindows\n```",
	} {
		if got := FromMarkdown(in); got != want {
			t.Errorf("%q\n got  %q\n want %q", in, got, want)
		}
	}
}

func TestMrkdwnBlocks(t *testing.T) {
	for in, want := range map[string]string{
		"# Title":                             "*Title*",
		"## Title ##":                         "*Title*",
		"### A **b** title ##":                "*A b title*",
		"   # three spaces":                   "*three spaces*",
		"# `code` and [l](https://a.example)": "*`code` and <https://a.example|l>*",
		"# ":                                  "",
		"#":                                   "#",
		"#tag":                                "#tag",
		"####### seven":                       "####### seven",
		"    # four spaces":                   "    # four spaces",
		"text\n# Title\ntext":                 "text\n*Title*\ntext",
		"- one\n- two":                        "• one\n• two",
		"+ one\n* two":                        "• one\n• two",
		"* one\n  * nested\n    * deeper\n      * deepest": "• one\n  ◦ nested\n    ▪ deeper\n      ▪ deepest",
		"1. first\n2. second\n3) third":                    "1. first\n2. second\n3. third",
		"- [ ] todo\n- [x] done\n- [X] done too":           "• ☐ todo\n• ☑ done\n• ☑ done too",
		"1. one\n   - sub *it*":                            "1. one\n  ◦ sub _it_",
		"1234. not a number":                               "1234. not a number",
		"-no space":                                        "-no space",
		"---":                                              "──────────",
		"***":                                              "──────────",
		"___":                                              "──────────",
		"- - -":                                            "──────────",
		"> quoted **text**":                                "> quoted *text*",
		"> a\n> b":                                         "> a\n> b",
		"> a\n>\n> b":                                      "> a\n>\n> b",
		">no space":                                        "> no space",
		">> nested\n> back":                                "> nested\n> back",
		"> - item\n> 1. num":                               "> • item\n> 1. num",
		"> # head":                                         "> *head*",
		"> ```\n> code <b>\n> ```":                         "> ```\n> code &lt;b&gt;\n> ```",
		"a\n> q\nb":                                        "a\n> q\nb",
		"one\n\n\n\ntwo":                                   "one\n\ntwo",
		"\n\n  \nfirst\n\n":                                "first",
		">":                                                "",
		"> ":                                               "",
	} {
		if got := FromMarkdown(in); got != want {
			t.Errorf("%q\n got  %q\n want %q", in, got, want)
		}
	}
}

func TestMrkdwnTables(t *testing.T) {
	for in, want := range map[string]string{
		"| Name | Result |\n|---|:-:|\n| `alpha` | **pass** |\n| beta | fail |": "```\nName  │ Result\n──────┼───────\nalpha │ pass\nbeta  │ fail\n```",
		"a | b\n---|---\n1 | 2":                           "```\na │ b\n──┼──\n1 │ 2\n```",
		"| a | b |\n|---|---|\n| 1 |\n| 1 | 2 | 3 |":      "```\na │ b\n──┼───┼──\n1\n1 │ 2 │ 3\n```",
		"intro\n| a | b |\n|---|---|\n| 1 | 2 |\n\nafter": "intro\n```\na │ b\n──┼──\n1 │ 2\n```\n\nafter",
		// A line with a bar is not a table without the separator row.
		"a | b | c":                             "a | b | c",
		"a | b\nnot | a | table":                "a | b\nnot | a | table",
		"> | a | b |\n> |---|---|\n> | 1 | 2 |": "> ```\n> a │ b\n> ──┼──\n> 1 │ 2\n> ```",
	} {
		if got := FromMarkdown(in); got != want {
			t.Errorf("%q\n got  %q\n want %q", in, got, want)
		}
	}
	got := FromMarkdown("| a | b |\n|---|---|\n| 汉字 | x |\n| ab | y |")
	if !strings.Contains(got, "汉字 │ x") || !strings.Contains(got, "ab   │ y") {
		t.Errorf("wide characters take two columns: %q", got)
	}
	// Slack reads the entities in code, so a cell has them escaped too, and its text cannot ping.
	got = FromMarkdown("| a | b |\n|---|---|\n| <!channel> | x & y |")
	if !strings.Contains(got, "&lt;!channel&gt; │ x &amp; y") || strings.Contains(got, "<") {
		t.Errorf("%q", got)
	}
}

// One long cell above a thousand short ones would be a million bytes of padding; the table is drawn without it.
func TestMrkdwnATableIsNotPaddedPastItsOwnSize(t *testing.T) {
	var b strings.Builder
	b.WriteString("| " + strings.Repeat("h", 20000) + " | b |\n|---|---|\n")
	for i := 0; i < 3000; i++ {
		b.WriteString("| a | b |\n")
	}
	in := b.String()
	out := FromMarkdown(in)
	if len(out) > 4*len(in) {
		t.Errorf("%d bytes for %d in", len(out), len(in))
	}
	if !strings.Contains(out, "\na │ b\n") {
		t.Errorf("the rows are not there: %.80q", out[20000:])
	}
	if msg := mdCheck(in, out); msg != "" {
		t.Error(msg)
	}
}

// Every way in which text that is not Markdown at all can get to Slack. None of these may come out as a mention or a link.
func TestMrkdwnInjectionsAreInert(t *testing.T) {
	injections := []string{
		"<!channel>", "<!here>", "<!everyone>", "<@U12345>", "<#C123|name>", "<!subteam^S123>",
		"<https://evil.example|click>", "<!date^1392734382^{date}|x>", "<mailto:a@b.example|x>",
	}
	contexts := []struct {
		name, format string
		links        int  // the links that the context really has
		piped        bool // a | in the text is changed (a label) or splits it (a table)
	}{
		{name: "plain", format: "%s"},
		{name: "in text", format: "before %s after"},
		{name: "bold", format: "**%s**"},
		{name: "italic star", format: "*%s*"},
		{name: "italic underscore", format: "_%s_"},
		{name: "bold italic", format: "***%s***"},
		{name: "strike", format: "~~%s~~"},
		{name: "heading", format: "# %s"},
		{name: "quote", format: "> %s"},
		{name: "nested quote", format: ">> %s"},
		{name: "bullet", format: "- %s"},
		{name: "number", format: "1. %s"},
		{name: "task", format: "- [ ] %s"},
		{name: "code span", format: "`%s`"},
		{name: "double code span", format: "``%s``"},
		{name: "code block", format: "```\n%s\n```"},
		{name: "code block with language", format: "```go\n%s\n```"},
		{name: "tilde block", format: "~~~\n%s\n~~~"},
		{name: "block never closed", format: "```\n%s"},
		{name: "block in a quote", format: "> ```\n> %s\n> ```"},
		{name: "table cell", format: "| a | b |\n|---|---|\n| %s | c |", piped: true},
		{name: "table head", format: "| %s | b |\n|---|---|\n| c | d |", piped: true},
		{name: "escaped", format: "\\%s"},
		{name: "after a link", format: "[a](https://ok.example)%s", links: 1},
		{name: "link label", format: "[%s](https://ok.example)", links: 1, piped: true},
		{name: "image alt", format: "![%s](https://ok.example)", links: 1, piped: true},
		{name: "link address", format: "[x](%s)"},
		{name: "link address with a title", format: "[x](%s \"t\")"},
		{name: "label and address", format: "[%s](%[1]s)"},
	}
	for _, ctx := range contexts {
		for _, inj := range injections {
			in := fmt.Sprintf(ctx.format, inj)
			out := FromMarkdown(in)
			if msg := mdCheck(in, out); msg != "" {
				t.Errorf("%s, %q: %s\n in  %q\n out %q", ctx.name, inj, msg, in, out)
				continue
			}
			if n := len(mdLinkRe.FindAllString(out, -1)); n != ctx.links {
				t.Errorf("%s, %q: %d links where %d are written\n in  %q\n out %q", ctx.name, inj, n, ctx.links, in, out)
			}
			want := Escape(inj)
			switch {
			case ctx.piped && strings.HasPrefix(ctx.name, "table"):
				want = Escape(strings.SplitN(inj, "|", 2)[0])
			case ctx.piped:
				want = strings.ReplaceAll(want, "|", "¦")
			}
			if ctx.name != "label and address" && !strings.Contains(out, want) {
				t.Errorf("%s, %q: the text is not there\n in  %q\n out %q", ctx.name, inj, in, out)
			}
		}
	}
	// The text that is already escaped is escaped again, not read back.
	if got := FromMarkdown("&lt;!channel&gt; &amp;"); got != "&amp;lt;!channel&amp;gt; &amp;amp;" {
		t.Errorf("%q", got)
	}
}

func TestMrkdwnCRLFEmptyAndBadUTF8(t *testing.T) {
	for in, want := range map[string]string{
		"":                            "",
		"\n\n":                        "",
		"   ":                         "",
		"a **b**\r\n- c\r\n":          "a *b*\n• c",
		"a\rb":                        "a\nb",
		"a\r\n\r\n\r\nb":              "a\n\nb",
		"a\xffb\xc3":                  "a\ufffdb\ufffd",
		"**\xff**":                    "*\ufffd*",
		"\xe6\xb1":                    "\ufffd",
		"`\xff`":                      "`\ufffd`",
		"[x](https://a.example/\xff)": "[x](https://a.example/\ufffd)",
	} {
		if got := FromMarkdown(in); got != want {
			t.Errorf("%q\n got  %q\n want %q", in, got, want)
		}
	}
	bad := strings.Repeat("\xff\xfe\x80", 100000)
	if out := FromMarkdown(bad); !utf8.ValidString(out) || len(out) > 3*len(bad) {
		t.Errorf("%d bytes of bad UTF-8 gave %d bytes", len(bad), len(out))
	}
}

// What the converter writes as marks is whole: for text that is made of complete spans and words, every mark that is written
// has its partner on the same line.
func TestMrkdwnTheMarksItWritesAreBalanced(t *testing.T) {
	pieces := []string{"**bold text**", "*it*", "_it too_", "~~gone~~", "`code`", "``co`de``", "***both***", "word", "other words", "[l](https://a.example/x_y)", "a, b.", "(x)", "**bold `c` in**", "~~**s**~~"}
	rng := rand.New(rand.NewSource(11))
	for n := 0; n < 3000; n++ {
		var b strings.Builder
		for i, k := 0, 1+rng.Intn(8); i < k; i++ {
			if i > 0 {
				b.WriteByte(" ,;"[rng.Intn(3)])
				if b.String()[b.Len()-1] != ' ' {
					b.WriteByte(' ')
				}
			}
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		in := b.String()
		out := FromMarkdown(in)
		if msg := mdCheck(in, out); msg != "" {
			t.Fatalf("%q\n gave %q\n%s", in, out, msg)
		}
		text := mdLinkRe.ReplaceAllString(out, "")
		for _, m := range "*_~`" {
			if strings.Count(text, string(m))%2 != 0 {
				t.Fatalf("%q\n gave %q\n an odd number of %c", in, out, m)
			}
		}
	}
}

func TestMrkdwnRealisticAnswer(t *testing.T) {
	in := "## Итоги\n\nСделано **три** вещи:\n\n1. Агент досылает очередь (`StopFlush`).\n2. Хук `PreToolUse` шлёт текст до действий.\n3. Таблица ниже.\n\n| Что | Статус |\n|---|---|\n| Хук | ✅ |\n| Тесты | ✅ `go test ./...` |\n\n> Важно: не трогать `kill-server`.\n\n```sh\ntmux -S \"$SOCK\" kill-server   # <- только свой сокет\n```\n\nСм. [README](https://example.com/readme) и snake_case_name."
	got := FromMarkdown(in)
	if msg := mdCheck(in, got); msg != "" {
		t.Fatalf("%s\n%s", msg, got)
	}
	for _, want := range []string{"*Итоги*", "*три*", "`StopFlush`", "> Важно: не трогать `kill-server`.", "```\ntmux -S \"$SOCK\" kill-server   # &lt;- только свой сокет\n```", "<https://example.com/readme|README>", "snake_case_name", "1. Агент", "Хук   │ ✅"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

// A million bytes of the patterns that make a careless converter work for the square of the size, or for ever, or write a
// lot more than it was given. Each must be done in well under a second (the limit here leaves room for the race detector and a
// slow machine), must hold the rules for every output, and must not panic.
func TestMrkdwnPathologicalInputIsLinear(t *testing.T) {
	patterns := []string{
		"*_", "*_*_\n", "*", "_", "~~a ", "*a ", "**a ", "***a ", "*a*", "a*b_c~d",
		"[", "![", "]", "[](", "[a](", "[a](b", "[a]", "[a](https://x.example", "[a](https://x.example)",
		"[[a](https://x.example)](https://x.example)", "[a](https://x.example/p \"", "[a](((((",
		"```", "```\n", "a\n```\n", "~~~\n", "`", "`a", "``a", "```a", "``` `` ` ", "`` ` ``` ```` ", "\\`", "\\",
		"<", "&", "<!channel>", "<https://evil.example|click>", "> ", ">> ", "> > > \n",
		"- ", "- \n", "1. ", "#", "# ", "---\n", "| a |\n|---|\n", "|", "|\n", "||\n", "|-|\n",
		" ", "\n", "\r\n", "\xff", "\xe6\xb1", "汉", "a b ", "*_~`[]()<>&|\\!#-> \n",
	}
	var slowest time.Duration
	for _, p := range patterns {
		in := strings.Repeat(p, (1<<20)/len(p)+1)[:1<<20]
		start := time.Now()
		out := FromMarkdown(in)
		took := time.Since(start)
		slowest = max(slowest, took)
		if took > 2*time.Second {
			t.Errorf("%q repeated: %v", p, took)
		}
		if msg := mdCheck(in, out); msg != "" {
			t.Errorf("%q repeated: %s", p, msg)
		}
	}
	t.Logf("the slowest of %d patterns of a million bytes: %v", len(patterns), slowest)
}

func TestMrkdwnDeepNestingIsCutOff(t *testing.T) {
	n := 100000
	in := strings.Repeat("*_~~", n) + "x" + strings.Repeat("~~_*", n)
	start := time.Now()
	out := FromMarkdown(in)
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("%v", took)
	}
	if msg := mdCheck(in, out); msg != "" {
		t.Error(msg)
	}
	in = strings.Repeat(">", 500000) + " deep"
	if out := FromMarkdown(in); out != "> deep" {
		t.Errorf("%.40q", out)
	}
}

// Random text out of the characters that matter: whatever it is, the output holds the rules for every output.
func TestMrkdwnRandomTextHoldsTheRules(t *testing.T) {
	const alphabet = "*_~`<>&[]()|#- \n:@!^/abchtps12"
	rng := rand.New(rand.NewSource(20260711))
	for n := 0; n < 2000; n++ {
		var b strings.Builder
		for i, k := 0, rng.Intn(80); i < k; i++ {
			b.WriteByte(alphabet[rng.Intn(len(alphabet))])
		}
		in := b.String()
		out := FromMarkdown(in)
		if msg := mdCheck(in, out); msg != "" {
			t.Fatalf("%q\n gave %q\n%s", in, out, msg)
		}
	}
	// And out of the pieces that it takes to make a link, a mention, a fence or a table by chance.
	pieces := []string{
		"*", "**", "***", "_", "__", "~~", "`", "``", "```", "~~~", "\n", "\n", " ", "a", "word", "1. ", "- ", "# ", "> ", ">> ",
		"[", "]", "(", ")", "![", "](https://x.example/p?q=1&r=2)", "](mailto:a@b.example)", "](javascript:x)", "](https://x.example|y)",
		"<", ">", "&", "&amp;", "|", "---", "| a | b |", "|---|---|", "\\", "<!channel>", "<@U1>", "<#C1|n>", "<!here>", "<https://e.example|c>",
		"<!subteam^S1>", "http://", "https://a.example", "mailto:", "\xff", "汉", "é", "\u200b", "\t", "\r\n",
	}
	for n := 0; n < 5000; n++ {
		var b strings.Builder
		for i, k := 0, rng.Intn(30); i < k; i++ {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		in := b.String()
		out := FromMarkdown(in)
		if msg := mdCheck(in, out); msg != "" {
			t.Fatalf("%q\n gave %q\n%s", in, out, msg)
		}
		// Split first and convert the pieces, as the sender does.
		for _, p := range Split(in, 128) {
			if msg := mdCheck(p, FromMarkdown(p)); msg != "" {
				t.Fatalf("%q\n split from %q\n gave %q\n%s", p, in, FromMarkdown(p), msg)
			}
		}
	}
}

// mdEndsInBlock reads the lines of a piece of Markdown and says whether it ends inside a code block.
func mdEndsInBlock(piece string) (open bool) {
	mark := ""
	for _, l := range strings.Split(piece, "\n") {
		if !open {
			if m := mdFence(l); m != "" && len(m) <= mdMaxFence { // Split follows no longer fence
				open, mark = true, m
			}
		} else if mdClosesFence(l, mark) {
			open = false
		}
	}
	return open
}

func TestMrkdwnSplitKeepsEachPieceWhole(t *testing.T) {
	var b strings.Builder
	b.WriteString("intro line\n```go\n")
	for i := 0; i < 400; i++ {
		b.WriteString(fmt.Sprintf("fmt.Println(\"line %04d \", <x> & ", i))
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString(")\n")
	}
	b.WriteString("```\nthe end\n")
	parts := Split(b.String(), 1000)
	if len(parts) < 5 {
		t.Fatalf("%d parts", len(parts))
	}
	for i, p := range parts {
		if len(p) > 1000 {
			t.Errorf("part %d is %d bytes", i, len(p))
		}
		if i < len(parts)-1 && mdEndsInBlock(p) {
			t.Errorf("part %d ends inside a code block", i)
		}
		// The converted piece has its fences in pairs, whatever the piece looked like.
		if out := FromMarkdown(p); mdCheck(p, out) != "" {
			t.Errorf("part %d: %s\n%.200s", i, mdCheck(p, out), out)
		}
	}
	if !strings.HasPrefix(parts[1], "```go\n") {
		t.Errorf("the second part does not open the block again: %.30q", parts[1])
	}
	if !strings.HasPrefix(FromMarkdown(parts[1]), "```\nfmt.Println") {
		t.Errorf("the second part converted: %.30q", FromMarkdown(parts[1]))
	}
	if !strings.Contains(parts[len(parts)-1], "the end") {
		t.Error("the text after the block is lost")
	}
	// Every line of the block is in some piece once, in order.
	var all strings.Builder
	for _, p := range parts {
		all.WriteString(p)
		all.WriteByte('\n')
	}
	prev := -1
	for i := 0; i < 400; i++ {
		at := strings.Index(all.String(), fmt.Sprintf("line %04d ", i))
		if at < 0 || at < prev || strings.Count(all.String(), fmt.Sprintf("line %04d ", i)) != 1 {
			t.Fatalf("line %d is lost, doubled or out of order", i)
		}
		prev = at
	}
}

func TestMrkdwnSplitShortTextIsOnePiece(t *testing.T) {
	if got := Split("short", 100); len(got) != 1 || got[0] != "short" {
		t.Errorf("%q", got)
	}
	exact := strings.Repeat("x", 100)
	if got := Split(exact, 100); len(got) != 1 || got[0] != exact {
		t.Errorf("a text of exactly max bytes: %q", got)
	}
	for _, in := range []string{"", "   \n ", "\n"} {
		if got := Split(in, 100); len(got) != 0 {
			t.Errorf("%q: white space alone is not a message, got %q", in, got)
		}
	}
}

func TestMrkdwnSplitNeverCutsACharacterAndLosesNothing(t *testing.T) {
	long := strings.Repeat("слово ", 700) + "\n" + strings.Repeat("汉", 900) + "\n" + strings.Repeat("é", 1000)
	for _, max := range []int{128, 200, 500, 1001} {
		parts := Split(long, max)
		var back strings.Builder
		for _, p := range parts {
			if len(p) > max {
				t.Errorf("max %d: a part of %d bytes", max, len(p))
			}
			if !utf8.ValidString(p) {
				t.Fatalf("max %d: a character was cut", max)
			}
			back.WriteString(p)
		}
		strip := strings.NewReplacer("\n", "", " ", "")
		if strip.Replace(back.String()) != strip.Replace(long) {
			t.Errorf("max %d: text was lost or changed by splitting", max)
		}
	}
	// A line of one word longer than a piece is cut inside the word, and the pieces put together are the line.
	line := strings.Repeat("x", 10000)
	parts := Split(line, 2800)
	if len(parts) != 4 || strings.Join(parts, "") != line {
		t.Errorf("%d parts, %d bytes", len(parts), len(strings.Join(parts, "")))
	}
}

// Prose is cut at line breaks, and a line that is too long at a space, so that words stay whole.
func TestMrkdwnSplitCutsProseAtLinesAndSpaces(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "paragraph %03d of the answer, which has a few words in it\n\n", i)
	}
	parts := Split(b.String(), 500)
	for i, p := range parts {
		if len(p) > 500 {
			t.Errorf("part %d is %d bytes", i, len(p))
		}
		if !strings.HasSuffix(strings.TrimRight(p, "\n"), "in it") {
			t.Errorf("part %d is cut inside a paragraph: %q", i, p[max(0, len(p)-30):])
		}
	}
	words := strings.TrimSpace(strings.Repeat("word ", 1000))
	for i, p := range Split(words, 300) {
		if i > 0 && !strings.HasPrefix(p, "word") {
			t.Errorf("part %d starts inside a word: %.10q", i, p)
		}
		if t.Failed() {
			break
		}
	}
}

// The text is not trusted: a long line after the fence, a long fence, a tiny max, none of it may make a piece longer than max,
// leave a block open in a piece that is not the last, or stop the loop.
func TestMrkdwnSplitHoldsWhatTheTextDoes(t *testing.T) {
	body := strings.Repeat("line of code here\n", 300)
	long := func(n int, s string) string { return strings.Repeat(s, n) }
	inputs := map[string]string{
		"plain block":               "```go\n" + body + "```\n",
		"tilde block":               "~~~\n" + body + "~~~\n",
		"long language":             "```" + long(2700, "x") + "\n" + body + "```\n",
		"longer than a piece":       "```" + long(2900, "x") + "\n" + body + "```\n",
		"just under a piece":        "```" + long(2790, "x") + "\n" + body + "```\n",
		"long fence":                long(100, "`") + "\n" + body,
		"very long fence":           long(3000, "`") + "\n" + body,
		"fence then long line":      "```" + long(2000, "x") + "\n" + long(5000, "y") + "\n```\n",
		"long line in a block":      "```\n" + long(10000, "y") + "\n```\n",
		"long closing fence":        "```\nx\n" + long(5000, "`") + "\ny\n",
		"fences in a row":           long(500, "```\n"),
		"many languages":            long(300, "```go\nx\n```\n"),
		"wide characters":           "```\n" + long(2000, "汉") + "\n" + long(2000, "汉汉\n") + "```\n",
		"bad UTF-8":                 "```\n" + long(2000, "\xff\x80") + "\n```\n",
		"block never closed":        "```\n" + body,
		"quote of a block":          "> ```\n" + strings.Repeat("> code\n", 1000) + "> ```\n",
		"mentions in a block":       "```\n" + long(500, "<!channel> <@U1>\n") + "```\n",
		"blank lines":               long(5000, "\n"),
		"wide text, no line breaks": long(3000, "汉字 "),
	}
	for name, in := range inputs {
		for _, max := range []int{128, 129, 150, 300, 1000, 2800} {
			var parts []string
			done := make(chan struct{})
			go func() {
				defer close(done)
				parts = Split(in, max)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s, max %d: Split does not stop", name, max)
			}
			for i, p := range parts {
				if len(p) > max {
					t.Errorf("%s, max %d: part %d is %d bytes", name, max, i, len(p))
				}
				if strings.TrimSpace(p) == "" {
					t.Errorf("%s, max %d: part %d is empty", name, max, i)
				}
				if name != "bad UTF-8" && !utf8.ValidString(p) {
					t.Errorf("%s, max %d: part %d has a cut character", name, max, i)
				}
				if i < len(parts)-1 && mdEndsInBlock(p) {
					t.Errorf("%s, max %d: part %d ends inside a code block", name, max, i)
				}
				if out := FromMarkdown(p); mdCheck(p, out) != "" {
					t.Errorf("%s, max %d: part %d: %s", name, max, i, mdCheck(p, out))
				}
			}
		}
		for _, max := range []int{0, 1, 2, 3, 4, 10, 63, 127} {
			done := make(chan struct{})
			go func() {
				defer close(done)
				for _, p := range Split(in, max) {
					if len(p) > max && utf8.RuneCountInString(p) > 1 && name != "bad UTF-8" {
						t.Errorf("%s, max %d: a part of %d bytes", name, max, len(p))
					}
				}
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s, max %d: Split does not stop", name, max)
			}
		}
	}
}

// Lines of text and of fences in any order: every line that is not a fence is in the pieces once and in order, and a piece
// that is not the last does not end inside a block.
func TestMrkdwnSplitRandomLines(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	for n := 0; n < 500; n++ {
		var lines []string
		for i, k := 0, 20+rng.Intn(200); i < k; i++ {
			switch rng.Intn(8) {
			case 0:
				lines = append(lines, []string{"```", "```go", "~~~", "````", "  ```py"}[rng.Intn(5)])
			case 1:
				lines = append(lines, "")
			default:
				lines = append(lines, fmt.Sprintf("text %d %s", i, strings.Repeat("é", rng.Intn(30))))
			}
		}
		in := strings.Join(lines, "\n")
		max := 128 + rng.Intn(300)
		parts := Split(in, max)
		var got, want []string
		for i, p := range parts {
			if len(p) > max {
				t.Fatalf("part %d is %d bytes, max %d", i, len(p), max)
			}
			if i < len(parts)-1 && mdEndsInBlock(p) {
				t.Fatalf("part %d ends inside a code block, max %d\n%s", i, max, p)
			}
			for _, l := range strings.Split(p, "\n") {
				if strings.HasPrefix(l, "text ") {
					got = append(got, l)
				}
			}
		}
		for _, l := range lines {
			if strings.HasPrefix(l, "text ") {
				want = append(want, l)
			}
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("lines lost or doubled, max %d\n%q", max, in)
		}
	}
}

func TestMrkdwnSplitPlain(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		max  int
		want []string
	}{
		{"empty", "", 10, nil},
		{"fits", "short text", 10, []string{"short text"}},
		{"fits with a break at the end", "abc\n", 10, []string{"abc\n"}},
		{"line break is preferred", "aaa\nbbb\nccc", 8, []string{"aaa\nbbb", "ccc"}},
		{"a line break is preferred to a later space", "ab\ncd ef gh ij", 9, []string{"ab", "cd ef gh", "ij"}},
		{"space when no line break", "one two three four", 10, []string{"one two", "three four"}},
		{"a break just after max", "abcd efgh", 4, []string{"abcd", "efgh"}},
		{"no break at all", "abcdefghij", 4, []string{"abcd", "efgh", "ij"}},
		{"a character at the cut", "ab汉cd", 3, []string{"ab", "汉", "cd"}},
		{"a longer character at the cut", "a😀b😀c", 4, []string{"a", "😀", "b", "😀", "c"}},
		{"max below one character", "汉汉汉", 1, []string{"汉", "汉", "汉"}},
		{"max of two and a character of three", "汉汉汉", 2, []string{"汉", "汉", "汉"}},
		{"max of zero", "abc", 0, []string{"a", "b", "c"}},
		{"max below zero", "abc", -5, []string{"a", "b", "c"}},
		{"a break at the start is kept in the text", "\n\nabcdef", 4, []string{"\n", "abcd", "ef"}},
	} {
		got := SplitPlain(tc.in, tc.max)
		if len(got) != len(tc.want) {
			t.Errorf("%s: %q gave %q, want %q", tc.name, tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: %q gave %q, want %q", tc.name, tc.in, got, tc.want)
				break
			}
		}
	}
	// One long line of two-byte letters: nothing is lost and no letter is cut.
	line := strings.Repeat("слово", 1000)
	parts := SplitPlain(line, 101)
	if strings.Join(parts, "") != line {
		t.Error("a cut between characters lost text")
	}
	for i, p := range parts {
		if len(p) > 101 || !utf8.ValidString(p) {
			t.Fatalf("part %d: %d bytes, valid %v", i, len(p), utf8.ValidString(p))
		}
	}
}

// The pieces put together are the text without the break characters at the cuts, one byte for each cut at most.
func TestMrkdwnSplitPlainRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	pieces := []string{"a", "bc", " ", "\n", "汉", "é", "😀", "word", "  ", "\n\n"}
	strip := strings.NewReplacer("\n", "", " ", "")
	for n := 0; n < 3000; n++ {
		var b strings.Builder
		for i, k := 0, rng.Intn(40); i < k; i++ {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		in := b.String()
		max := 1 + rng.Intn(20)
		parts := SplitPlain(in, max)
		if in == "" {
			if parts != nil {
				t.Fatalf("empty text gave %q", parts)
			}
			continue
		}
		joined := strings.Join(parts, "")
		for i, p := range parts {
			if p == "" || !utf8.ValidString(p) || (len(p) > max && utf8.RuneCountInString(p) != 1) {
				t.Fatalf("%q max %d: part %d is %q", in, max, i, p)
			}
		}
		if strip.Replace(joined) != strip.Replace(in) || len(in)-len(joined) > len(parts) || len(joined) > len(in) {
			t.Fatalf("%q max %d: %q do not make the text", in, max, parts)
		}
		if len(in) <= max && (len(parts) != 1 || parts[0] != in) {
			t.Fatalf("%q fits in %d but gave %q", in, max, parts)
		}
	}
}
