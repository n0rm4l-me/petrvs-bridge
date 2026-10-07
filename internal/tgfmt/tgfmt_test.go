package tgfmt

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestInlineFormatting(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":                  "plain text",
		"a **bold** word":             "a <b>bold</b> word",
		"an *italic* word":            "an <i>italic</i> word",
		"an _italic_ word":            "an <i>italic</i> word",
		"~~gone~~":                    "<s>gone</s>",
		"run `go test ./...` now":     "run <code>go test ./...</code> now",
		"``a ` b``":                   "<code>a ` b</code>",
		"**bold with `code` inside**": "<b>bold with <code>code</code> inside</b>",
		"***both***":                  "<b><i>both</i></b>",
		"[the docs](https://example.com/a?b=1&c=2)": `<a href="https://example.com/a?b=1&amp;c=2">the docs</a>`,
		"![pic](https://example.com/p.png)":         `<a href="https://example.com/p.png">pic</a>`,
		"a < b && c > d":                            "a &lt; b &amp;&amp; c &gt; d",
		"escaped \\*stars\\* and \\`ticks\\`":       "escaped *stars* and `ticks`",
	} {
		if got := HTML(in); got != want {
			t.Errorf("%q\n got  %q\n want %q", in, got, want)
		}
	}
}

// Text that only looks like Markdown stays as it is.
func TestThingsThatAreNotFormattingAreLeftAlone(t *testing.T) {
	for _, in := range []string{
		"snake_case_name and my_file_name.go",
		"2*3*4 = 24",
		"a * b",
		"price: 5*",
		"under_score_ at the end",
		"*not closed",
		"**not closed",
		"`not closed",
		"~~not closed",
		"[not a link] (really)",
		"[bad](javascript:alert(1))",
		"[bad](tg://resolve?domain=x)",
		"[](https://example.com)",
		"https://example.com/a_b_c is a link",
		"C:\\path\\to\\file",
		"__init__.py",
		"a __dunder__ word",
		"100% of 5 > 3",
	} {
		got := HTML(in)
		plain := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">").Replace(got)
		if strings.Contains(got, "<b>") || strings.Contains(got, "<i>") || strings.Contains(got, "<s>") || strings.Contains(got, "<a ") || strings.Contains(got, "<code>") {
			t.Errorf("%q was formatted: %q", in, got)
		}
		if plain != in {
			t.Errorf("%q changed to %q", in, plain)
		}
	}
}

func TestLinksOnlyToWebAddresses(t *testing.T) {
	got := HTML(`[x](http://a.example/p) [y](https://b.example/"onclick=1) [z](file:///etc/passwd)`)
	if !strings.Contains(got, `<a href="http://a.example/p">x</a>`) {
		t.Errorf("%q", got)
	}
	if strings.Contains(got, "file://") && strings.Contains(got, "<a href=\"file") {
		t.Errorf("a file link was made: %q", got)
	}
	if strings.Contains(got, `<a href="https://b.example`) {
		t.Errorf("a link with a quote in its address was made: %q", got)
	}
}

func TestCodeBlocks(t *testing.T) {
	in := "before\n```go\nfmt.Println(\"<hi>\" & x)\n```\nafter"
	want := "before\n<pre><code class=\"language-go\">fmt.Println(\"&lt;hi&gt;\" &amp; x)</code></pre>\nafter"
	if got := HTML(in); got != want {
		t.Errorf("\n got  %q\n want %q", got, want)
	}
	// Nothing inside a block is Markdown, and a longer fence holds a shorter one.
	if got := HTML("````\n**not bold** ```\n````"); got != "<pre>**not bold** ```</pre>" {
		t.Errorf("%q", got)
	}
	if got := HTML("~~~\nx\n~~~"); got != "<pre>x</pre>" {
		t.Errorf("%q", got)
	}
	// A block that is never closed ends with the text; an odd language name is not put into the tag.
	if got := HTML("```\nopen"); got != "<pre>open</pre>" {
		t.Errorf("%q", got)
	}
	if got := HTML("```\"><script>\nx\n```"); strings.Contains(got, "script") && strings.Contains(got, "<script") {
		t.Errorf("%q", got)
	}
	if got := HTML("```\n\n```"); got != "" {
		t.Errorf("an empty block gave %q", got)
	}
}

func TestBlocks(t *testing.T) {
	for in, want := range map[string]string{
		"# Title":                "<b>Title</b>",
		"### A **b** title ##":   "<b>A b title</b>",
		"- one\n- two":           "• one\n• two",
		"* one\n  * nested":      "• one\n  ◦ nested",
		"1. first\n2. second":    "1. first\n2. second",
		"- [ ] todo\n- [x] done": "• ☐ todo\n• ☑ done",
		"---":                    "──────────",
		"> quoted **text**":      "<blockquote>quoted <b>text</b></blockquote>",
		"> a\n> b":               "<blockquote>a\nb</blockquote>",
		"one\n\n\n\ntwo":         "one\n\ntwo",
	} {
		if got := HTML(in); got != want {
			t.Errorf("%q\n got  %q\n want %q", in, got, want)
		}
	}
}

func TestATableBecomesAlignedFixedWidthText(t *testing.T) {
	in := "| Name | Result |\n|---|:-:|\n| `alpha` | **pass** |\n| beta | fail |"
	want := "<pre>Name  │ Result\n──────┼───────\nalpha │ pass\nbeta  │ fail</pre>"
	if got := HTML(in); got != want {
		t.Errorf("\n got  %q\n want %q", got, want)
	}
	// A line with a bar is not a table without the separator row.
	if got := HTML("a | b | c"); got != "a | b | c" {
		t.Errorf("%q", got)
	}
}

func TestWideCharactersTakeTwoColumns(t *testing.T) {
	got := HTML("| a | b |\n|---|---|\n| 汉字 | x |\n| ab | y |")
	if !strings.Contains(got, "汉字 │ x") || !strings.Contains(got, "ab   │ y") {
		t.Errorf("%q", got)
	}
}

func TestALongQuoteIsFolded(t *testing.T) {
	got := HTML("> " + strings.Repeat("word ", 200))
	if !strings.HasPrefix(got, "<blockquote expandable>") {
		t.Errorf("%.60q", got)
	}
}

// Whatever comes in, what goes out is HTML that Telegram parses: the tags it knows, closed in order, nothing raw between.
func TestTheOutputIsAlwaysBalancedHTML(t *testing.T) {
	alphabet := []string{"*", "**", "_", "__", "~~", "`", "``", "```", "\n", " ", "a", "b", "1.", "- ", "# ", "> ", "[", "]", "(", ")", "](https://x.y/z)", "<", ">", "&", "|", "---", "\\", "\"", "汉", "é", "~~~"}
	rng := rand.New(rand.NewSource(7))
	for n := 0; n < 30000; n++ {
		var b strings.Builder
		for i, k := 0, rng.Intn(30); i < k; i++ {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		in := b.String()
		if err := validHTML(HTML(in)); err != "" {
			t.Fatalf("%q\n gave %q\n%s", in, HTML(in), err)
		}
	}
}

func TestRealisticAnswersGiveValidHTML(t *testing.T) {
	in := "## Итоги\n\nСделано **три** вещи:\n\n1. Агент досылает очередь (`StopFlush`).\n2. Хук `PreToolUse` шлёт текст до действий.\n3. Таблица ниже.\n\n| Что | Статус |\n|---|---|\n| Хук | ✅ |\n| Тесты | ✅ `go test ./...` |\n\n> Важно: не трогать `kill-server`.\n\n```sh\ntmux -S \"$SOCK\" kill-server   # <- только свой сокет\n```\n\nСм. [README](https://example.com/readme) и snake_case_name."
	got := HTML(in)
	if err := validHTML(got); err != "" {
		t.Fatalf("%s\n%s", err, got)
	}
	for _, want := range []string{"<b>Итоги</b>", "<b>три</b>", "<code>StopFlush</code>", "<blockquote>", `<pre><code class="language-sh">`, `<a href="https://example.com/readme">README</a>`, "snake_case_name"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

// validHTML checks the subset of HTML that Telegram takes. It returns "" when the text is fine.
func validHTML(s string) string {
	allowed := map[string]bool{"b": true, "i": true, "s": true, "code": true, "pre": true, "a": true, "blockquote": true}
	var stack []string
	for i := 0; i < len(s); {
		switch s[i] {
		case '<':
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				return "an unclosed tag"
			}
			tag := s[i+1 : i+end]
			i += end + 1
			if strings.HasPrefix(tag, "/") {
				name := tag[1:]
				if len(stack) == 0 || stack[len(stack)-1] != name {
					return "a closing tag out of order: " + tag
				}
				stack = stack[:len(stack)-1]
				continue
			}
			name := strings.Fields(tag)[0]
			if !allowed[name] {
				return "a tag Telegram does not take: " + tag
			}
			if len(stack) > 0 && (stack[len(stack)-1] == "pre" && name != "code") {
				return "a tag inside pre: " + tag
			}
			if len(stack) > 0 && stack[len(stack)-1] == "code" && len(stack) > 1 && stack[len(stack)-2] == "pre" {
				return "a tag inside pre/code: " + tag
			}
			if name == "blockquote" {
				for _, o := range stack {
					if o == "blockquote" {
						return "a quote inside a quote"
					}
				}
			}
			stack = append(stack, name)
		case '>':
			return "a raw >"
		case '&':
			j := strings.IndexByte(s[i:], ';')
			if j < 0 || (s[i:i+j+1] != "&amp;" && s[i:i+j+1] != "&lt;" && s[i:i+j+1] != "&gt;" && s[i:i+j+1] != "&quot;") {
				return "a raw &"
			}
			i += j + 1
		default:
			i++
		}
	}
	if len(stack) != 0 {
		return "tags left open: " + strings.Join(stack, ",")
	}
	if !utf8.ValidString(s) {
		return "invalid UTF-8"
	}
	return ""
}

func TestSplitKeepsEachPieceWhole(t *testing.T) {
	var b strings.Builder
	b.WriteString("intro line\n```go\n")
	for i := 0; i < 400; i++ {
		b.WriteString("fmt.Println(\"line number \", ")
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
		if err := validHTML(HTML(p)); err != "" {
			t.Errorf("part %d: %s\n%.200s", i, err, p)
		}
		if strings.Count(p, "```")%2 != 0 {
			t.Errorf("part %d has an unbalanced fence", i)
		}
	}
	if !strings.HasPrefix(parts[1], "```go\n") {
		t.Errorf("the second part does not reopen the block: %.30q", parts[1])
	}
	if !strings.Contains(parts[len(parts)-1], "the end") {
		t.Error("the text after the block is lost")
	}
}

func TestSplitNeverCutsACharacterAndLosesNothing(t *testing.T) {
	long := strings.Repeat("слово ", 700) + "\n" + strings.Repeat("汉", 900)
	parts := Split(long, 500)
	var back strings.Builder
	for _, p := range parts {
		if len(p) > 500 {
			t.Errorf("a part of %d bytes", len(p))
		}
		if !utf8.ValidString(p) {
			t.Fatal("a character was cut")
		}
		back.WriteString(p)
	}
	if strings.ReplaceAll(strings.ReplaceAll(back.String(), "\n", ""), " ", "") != strings.ReplaceAll(strings.ReplaceAll(long, "\n", ""), " ", "") {
		t.Error("text was lost or changed by splitting")
	}
	if got := Split("short", 100); len(got) != 1 || got[0] != "short" {
		t.Errorf("%q", got)
	}
	if got := Split("   \n ", 100); len(got) != 0 {
		t.Errorf("%q: white space alone is not a message", got)
	}
}
