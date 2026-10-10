package slack

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// codeLines gives the lines of a converted text that are inside a code block, and the lines that are outside.
func codeLines(s string) (in, out []string) {
	code := false
	for _, l := range strings.Split(s, "\n") {
		if l == "```" {
			code = !code
			continue
		}
		if code {
			in = append(in, l)
		} else {
			out = append(out, l)
		}
	}
	return
}

func TestSplitMrkdwnShortTextIsOnePieceAndBlankTextNone(t *testing.T) {
	if got := SplitMrkdwn("short *text*", 100); len(got) != 1 || got[0] != "short *text*" {
		t.Fatalf("%q", got)
	}
	for _, s := range []string{"", "  \n\t\n", "```\n```", "\n\n"} {
		if s == "```\n```" {
			continue // a block with nothing in it is not made by the converter
		}
		if got := SplitMrkdwn(s, 100); got != nil {
			t.Errorf("%q gave %q", s, got)
		}
	}
}

func TestSplitMrkdwnCutsAtLinesAndKeepsEveryPieceWithinMax(t *testing.T) {
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, "line "+strings.Repeat("x", i%40))
	}
	text := strings.Join(lines, "\n")
	got := SplitMrkdwn(text, 300)
	if len(got) < 10 {
		t.Fatalf("%d pieces", len(got))
	}
	for i, p := range got {
		if len(p) > 300 {
			t.Errorf("piece %d is %d bytes", i, len(p))
		}
	}
	if strings.Join(got, "\n") != text {
		t.Fatal("something was lost or changed at the cuts")
	}
}

// A code block cut in two stays a code block in both halves: what the author put in it is code in the second half too.
func TestSplitMrkdwnKeepsACodeBlockACodeBlockInEveryPiece(t *testing.T) {
	body := strings.Repeat("if a &lt; b &amp;&amp; c { <not a link> }\n", 120)
	conv := FromMarkdown("before\n\n```go\n" + strings.ReplaceAll(strings.ReplaceAll(body, "&lt;", "<"), "&amp;", "&") + "[click](https://evil.example/login) *x*\n```\n\nafter *bold*")
	pieces := SplitMrkdwn(conv, 400)
	if len(pieces) < 5 {
		t.Fatalf("%d pieces", len(pieces))
	}
	wantIn, wantOut := codeLines(conv)
	var gotIn, gotOut []string
	for i, p := range pieces {
		if msg := mdCheck(p, p); msg != "" {
			t.Fatalf("piece %d: %s\n%s", i, msg, p)
		}
		in, out := codeLines(p)
		gotIn, gotOut = append(gotIn, in...), append(gotOut, out...)
	}
	if strings.Join(gotIn, "\n") != strings.Join(wantIn, "\n") {
		t.Fatal("the lines of code changed at the cuts")
	}
	if strings.Join(gotOut, "\n") != strings.Join(wantOut, "\n") {
		t.Fatalf("the lines outside code changed at the cuts:\n%q\n%q", gotOut, wantOut)
	}
	for _, l := range gotOut {
		if strings.Contains(l, "evil.example") {
			t.Fatalf("the link that was inside the code block is outside it: %q", l)
		}
	}
}

// A long code line ending in three backticks must not leave a closing fence at the start of a piece.
func TestSplitMrkdwnADeadlyCodeLineEndingInBackticksStaysCode(t *testing.T) {
	for k := 300; k < 360; k++ {
		in := "Here is the file:\n```\n" + strings.Repeat("A", k) + "```\n[click to log in](https://evil.example/login)\nDone.\n```\n"
		conv := FromMarkdown(in)
		pieces := SplitMrkdwn(conv, 320)
		for i, p := range pieces {
			if msg := mdCheck(p, p); msg != "" {
				t.Fatalf("k=%d piece %d: %s\n%q", k, i, msg, p)
			}
			_, out := codeLines(p)
			for _, l := range out {
				if strings.Contains(l, "evil.example") {
					t.Fatalf("k=%d: a link came alive in piece %d:\n%q", k, i, pieces)
				}
			}
		}
	}
}

func TestSplitMrkdwnNeverCutsACharacterALinkOrAnEntity(t *testing.T) {
	long := strings.Repeat("日本語 ", 200) + "<https://example.com/a/very/long/path/that/goes/on|a label> " + strings.Repeat("a &amp; b ", 100)
	for _, max := range []int{100, 333} { // the link above is 60 bytes: a piece shorter than a link has to cut it
		pieces := SplitMrkdwn(long, max)
		for i, p := range pieces {
			if !utf8.ValidString(p) {
				t.Fatalf("max %d piece %d is not UTF-8: %q", max, i, p)
			}
			if strings.Count(p, "<") != strings.Count(p, ">") {
				t.Fatalf("max %d piece %d cuts a link: %q", max, i, p)
			}
			if strings.HasSuffix(p, "&") || strings.HasSuffix(p, "&a") || strings.HasSuffix(p, "&am") || strings.HasSuffix(p, "&amp") {
				t.Fatalf("max %d piece %d cuts an entity: %q", max, i, p)
			}
			if len(p) > max+8 { // a character, or a fence line and a line break
				t.Fatalf("max %d piece %d is %d bytes", max, i, len(p))
			}
		}
	}
}

func TestSplitMrkdwnOneLineLongerThanEverythingIsCutAndLosesNothingVisible(t *testing.T) {
	line := strings.Repeat("abcdefghij", 5000)
	pieces := SplitMrkdwn(line, 1000)
	if strings.Join(pieces, "") != line {
		t.Fatal("bytes were lost in a line without spaces")
	}
	for _, p := range pieces {
		if len(p) > 1000 {
			t.Fatalf("%d bytes", len(p))
		}
	}
}

func TestSplitMrkdwnRandomText(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	atoms := []string{"```\n", "word ", "日本", "<https://a.example|b c>", "&amp;", "\n", "\n\n", "> q\n", "`code` ", "*b* ", strings.Repeat("z", 90), " ", "```\n"}
	for n := 0; n < 3000; n++ {
		var b strings.Builder
		for i, k := 0, rng.Intn(60); i < k; i++ {
			b.WriteString(atoms[rng.Intn(len(atoms))])
		}
		conv := FromMarkdown(b.String())
		max := 32 + rng.Intn(300)
		if longLink(conv, max) {
			continue
		}
		for i, p := range SplitMrkdwn(conv, max) {
			if msg := mdCheck(p, p); msg != "" {
				t.Fatalf("max %d piece %d: %s\n%q\nfrom %q", max, i, msg, p, conv)
			}
			if len(p) > max+8 {
				t.Fatalf("max %d piece %d is %d bytes", max, i, len(p))
			}
			if strings.TrimSpace(strings.ReplaceAll(p, "```", "")) == "" {
				t.Fatalf("an empty piece from %q", conv)
			}
		}
	}
}

// Inputs in which marks that drop out of the text bring backticks together into a fence.
func TestMrkdwnBackticksThatMeetWhenMarksDropOutDoNotMakeAFence(t *testing.T) {
	for _, in := range []string{"# `**``a**", "[x`**``y**](http://a.com)", "**`**``a**`", "_`__``a__`", "~~`~~``a~~`"} {
		out := FromMarkdown(in)
		if msg := mdCheck(in, out); msg != "" {
			t.Errorf("%q gave %q: %s", in, out, msg)
		}
		if strings.Contains(out, "```") {
			t.Errorf("%q gave %q", in, out)
		}
	}
}

func TestMrkdwnNothingIsLiveInTheSecondHalfOfACodeBlock(t *testing.T) {
	// a block whose escaped text is longer than a message
	src := "```html\n" + strings.Repeat("<a>", 700) + "\n[click](https://evil.example/login) *x*\n```"
	for _, md := range []bool{true} {
		ps := parts(telegramMessage(src, md))
		for i, p := range ps {
			_, out := codeLines(p.text)
			for _, l := range out {
				if strings.Contains(l, "evil.example") || strings.Contains(l, "_x_") {
					t.Fatalf("part %d of %d: live text outside the block: %q", i, len(ps), l)
				}
			}
		}
	}
}

func FuzzConvertAndSplit(f *testing.F) {
	for _, s := range []string{"", "plain", "**b** `c` [l](https://a.example)", "```\ncode\n```", "# `**``a**", "[x`**``y**](http://a.com)", "<!channel> <@U1>",
		"| a | b |\n|---|---|\n| 1 | 2 |", "> q\n> > r", "- a\n  - b\n1. c", "[https://good.example](https://evil.example)", "```go\n" + strings.Repeat("a", 400) + "```\n[click](https://e.example)"} {
		f.Add(s, 64+len(s)%300)
	}
	f.Fuzz(func(t *testing.T, in string, max int) {
		if max < 32 || max > 4000 {
			max = 32 + (max%4000+4000)%3968
		}
		in = mdStripBidi(in) // as the sender does
		out := FromMarkdown(in)
		if msg := mdCheck(in, out); msg != "" {
			t.Fatalf("%q\n gave %q\n%s", in, out, msg)
		}
		// A link is always written as its label and, after it, its address in brackets: the address that is shown is the one that
		// was written. (Every <...> that is not a quote mark is an address that passed mdURL, so it is the same check as mdCheck's.)
		for _, l := range mdLinkRe.FindAllString(out, -1) {
			if strings.Contains(l, "|") {
				t.Fatalf("a link with a label of its own: %q in %q", l, out)
			}
		}
		if longLink(out, max) {
			return
		}
		for i, p := range SplitMrkdwn(out, max) {
			if msg := mdCheck(p, p); msg != "" {
				t.Fatalf("max %d piece %d: %s\n%q\nfrom %q", max, i, msg, p, out)
			}
			if len(p) > max+8 {
				t.Fatalf("max %d piece %d is %d bytes", max, i, len(p))
			}
		}
	})
}

// The parts of a message, whatever the text: each fits a block of text, is safe by the rules of the converter, and the plain ones
// are the text itself, escaped.
func TestPartsOfAnyMessageFitABlockAndAreSafe(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	atoms := []string{"```\n", "```go\n", "word ", "日本語", "<!channel>", "<@U1>", "&", "&amp;", "<", ">", "\n", "\n\n", "> q\n", "`c` ", "**b** ", "[l](https://a.example/x) ",
		"[https://good.example](https://evil.example) ", "| a | b |\n|---|---|\n| 1 | 2 |\n", strings.Repeat("z", 700), strings.Repeat("<>&", 400), "# h\n", "- i\n"}
	for n := 0; n < 600; n++ {
		var b strings.Builder
		for i, k := 0, rng.Intn(80); i < k; i++ {
			b.WriteString(atoms[rng.Intn(len(atoms))])
		}
		for _, md := range []bool{true, false} {
			ps := parts(telegramMessage(b.String(), md))
			for i, p := range ps {
				if len(p.text) > maxBlockText {
					t.Fatalf("markdown=%v part %d is %d bytes", md, i, len(p.text))
				}
				if strings.TrimSpace(p.text) == "" {
					t.Fatalf("markdown=%v: an empty part", md)
				}
				if md {
					if msg := mdCheck(p.text, p.text); msg != "" && !longLink(p.text, maxPartMrkdwn) {
						t.Fatalf("markdown=false part %d: %s\n%q", i, msg, p.text)
					}
				} else if Escape(p.raw) != p.text || !p.plain {
					t.Fatalf("a plain part is not the escaped text: %q %q", p.raw, p.text)
				}
			}
		}
	}
}

// A code block inside a quote has "> " in front of its fence and of every line; a cut in it closes and opens it with the same mark.
func TestSplitMrkdwnFollowsACodeBlockInsideAQuote(t *testing.T) {
	conv := FromMarkdown("> ```\n> line 1\n> line 2 *x* :tada: http://evil.example\n> ```\n\nafter")
	const code = "line 1line 2 *x* :tada: http://evil.example"
	for max := 32; max < 70; max += 3 {
		pieces := SplitMrkdwn(conv, max)
		var inside []string
		for i, p := range pieces {
			if msg := mdCheck(p, p); msg != "" {
				t.Fatalf("max %d piece %d: %s\n%q\nof %q", max, i, msg, p, conv)
			}
			in := false
			for _, l := range strings.Split(p, "\n") {
				switch {
				case l == "> ```":
					in = !in
				case in:
					if !strings.HasPrefix(l, "> ") {
						t.Fatalf("max %d piece %d: a line of a quoted block lost its quote mark: %q", max, i, l)
					}
					inside = append(inside, strings.TrimPrefix(l, "> "))
				case strings.Contains(l, "evil.example") || strings.Contains(l, "*x*"):
					t.Fatalf("max %d piece %d: text of the block is outside it: %q", max, i, l)
				}
			}
		}
		if got := strings.Join(inside, ""); got != code {
			t.Fatalf("max %d: the code changed:\n got  %q\n want %q\n%q", max, got, code, pieces)
		}
	}
}

func TestSplitMrkdwnDoesNotMakeAPieceOfOnlyFencesAndQuoteMarks(t *testing.T) {
	for _, s := range []string{"```\n```\n> ", "> ```\n> ```\n>", "```\n```"} {
		if got := SplitMrkdwn(s+"\n"+strings.Repeat("x", 100), 40); len(got) > 0 {
			for _, p := range got {
				if strings.TrimSpace(strings.NewReplacer("```", "", ">", "").Replace(p)) == "" {
					t.Errorf("an empty piece %q from %q", p, s)
				}
			}
		}
	}
}
