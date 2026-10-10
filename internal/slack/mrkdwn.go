// The converter from the Markdown that the assistant and the agent write to the text format that Slack calls mrkdwn, so that
// the chat shows bold, code blocks, lists and quotes instead of asterisks and backticks.
//
// It is conservative on purpose. Text that is not Markdown comes out unchanged (with &, < and > escaped), because a word with
// an underscore or a product like 2*3*4 is not formatting. Only what Slack can show is produced: *bold*, _italic_, ~strike~,
// `code`, code blocks, quotes and links. Slack has no headings and no tables: a heading becomes a bold line and a table an
// aligned block of fixed-width text.
//
// The text comes from an agent and from the files it read, so it is treated as hostile. Slack reads <...> as a mention, a
// channel, a broadcast or a link, and it reads the entities everywhere, in code too. So every &, < and > is escaped, and the
// only angle brackets that are ever written are the ones of a link that the text spelled as a Markdown link to an http, https
// or mailto address that passed the check in mdURL. Nothing in the text can ping anyone, and nothing in it can close a code
// block that it is in, because three backticks never come out of this file except as the fence of a block it opened itself.
// Whatever the input, the work is linear in its size and the output is at most about ten times as long (the most is a rule,
// three bytes that become thirty).

package slack

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	mdMaxDepth    = 8    // spans in spans: what is nested deeper is taken as text
	mdWorkBase    = 4096 // the bytes a line may make the converter read, apart from ...
	mdWorkPerByte = 24   // ... so many times its own length: past that a line is no longer searched for formatting
	mdMaxURL      = 2000 // bytes of an address that a link may have
	mdMaxTitle    = 500  // bytes after the address of a link that may hold a title
)

// The kinds of span that can be open, so that a second one of the same kind only drops its marks.
const (
	mdBold uint8 = 1 << iota
	mdItalic
	mdStrike
)

var (
	mdEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

	// mdSpecial are the bytes that start something in a line; the text between them is copied as it is.
	mdSpecial = func() (t [256]bool) {
		for _, c := range []byte("\\`*_~[!") {
			t[c] = true
		}
		return
	}()
)

// Escape makes text safe for Slack: &, < and > become &amp;, &lt; and &gt;. Nothing else is touched.
func Escape(s string) string { return mdEscaper.Replace(s) }

// FromMarkdown converts Markdown to Slack mrkdwn.
func FromMarkdown(src string) string {
	src = strings.ToValidUTF8(src, "\uFFFD")
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	return mdRender(src, false)
}

// mdRender is FromMarkdown; inQuote says the text is the inside of a quote, where Slack takes no further quote, so a nested one
// is shown as the lines of the quote it is in.
func mdRender(src string, inQuote bool) string {
	lines := strings.Split(src, "\n")
	var out []string
	blank := true // no blank line at the start, and never two in a row
	for i := 0; i < len(lines); {
		l := lines[i]
		mark := mdFence(l)
		title, isHeading := mdHeading(l)
		inner, isQuote := mdUnquote(l)
		switch {
		case mark != "":
			j := i + 1
			var body []string
			for ; j < len(lines); j++ {
				if mdClosesFence(lines[j], mark) {
					break
				}
				body = append(body, lines[j])
			}
			if blk := mdCodeBlock(strings.Join(body, "\n")); blk != "" {
				out = append(out, blk)
				blank = false
			}
			i = j + 1 // past the closing fence; an unclosed block ends with the text

		case strings.Contains(l, "|") && i+1 < len(lines) && strings.Contains(lines[i+1], "|") && mdIsTableSep(lines[i+1]):
			j := i + 2
			for j < len(lines) && strings.Contains(lines[j], "|") && strings.TrimSpace(lines[j]) != "" {
				j++
			}
			if blk := mdCodeBlock(mdTable(l, lines[i+2:j])); blk != "" {
				out = append(out, blk)
				blank = false
			}
			i = j

		case isHeading:
			// Slack has no headings; a bold line is the nearest thing. A heading with nothing in it is no line at all.
			if body := mdMark(mdFlags{}, mdBold, "*", mdLine(title, mdFlags{open: mdBold})); strings.TrimSpace(body) != "" {
				out = append(out, body)
				blank = false
			}
			i++

		case mdIsRule(l):
			out = append(out, "──────────")
			i++
			blank = false

		case isQuote && inQuote:
			for {
				rest, ok := mdUnquote(inner)
				if !ok {
					break
				}
				inner = rest
			}
			out = append(out, mdRender(inner, true))
			i++
			blank = false

		case isQuote:
			var inside []string
			for ; i < len(lines); i++ {
				rest, ok := mdUnquote(lines[i])
				if !ok {
					break
				}
				inside = append(inside, rest)
			}
			if body := strings.TrimSpace(mdRender(strings.Join(inside, "\n"), true)); body != "" {
				out = append(out, mdQuote(body))
				blank = false
			}

		case strings.TrimSpace(l) == "":
			if !blank {
				out = append(out, "")
			}
			blank = true
			i++

		default:
			out = append(out, mdListOrText(l))
			i++
			blank = false
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// mdQuote puts the quote mark in front of each line. An empty line gets the bare mark, so that the quote goes on over it.
func mdQuote(body string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if l == "" {
			lines[i] = ">"
		} else {
			lines[i] = "> " + l
		}
	}
	return strings.Join(lines, "\n")
}

// mdIsRule says whether the line is a horizontal rule: three or more of one of - * _, with spaces allowed between.
func mdIsRule(l string) bool {
	t := strings.TrimLeft(l, " ")
	if len(l)-len(t) > 3 || t == "" || strings.IndexByte("-*_", t[0]) < 0 {
		return false
	}
	n := 0
	for i := 0; i < len(t); i++ {
		switch {
		case t[i] == t[0]:
			n++
		case t[i] == ' ' || t[i] == '\t':
		default:
			return false
		}
	}
	return n >= 3
}

// mdFence returns the fence of a line that opens a code block: three or more backticks or tildes, indented by at most three
// spaces, and no backtick after them. The rest of the line, the language, is dropped; Slack has nothing to do with it.
func mdFence(l string) string {
	t := strings.TrimLeft(l, " ")
	if len(l)-len(t) > 3 || len(t) < 3 || (t[0] != '`' && t[0] != '~') {
		return ""
	}
	n := mdRun(t, 0, t[0])
	if n < 3 || strings.IndexByte(t[n:], '`') >= 0 {
		return ""
	}
	return t[:n]
}

// mdClosesFence says whether the line ends a block opened with mark: the same character, at least as many, nothing else.
func mdClosesFence(l, mark string) bool {
	t := strings.TrimLeft(l, " ")
	if len(l)-len(t) > 3 {
		return false
	}
	t = strings.TrimRight(t, " \t")
	return len(t) >= len(mark) && strings.Trim(t, mark[:1]) == ""
}

// mdHeading reads a line of # signs and a title; the closing # signs are not part of the title.
func mdHeading(l string) (title string, ok bool) {
	t := strings.TrimLeft(l, " ")
	if len(l)-len(t) > 3 {
		return "", false
	}
	n := mdRun(t, 0, '#')
	if n < 1 || n > 6 || n >= len(t) || (t[n] != ' ' && t[n] != '\t') {
		return "", false
	}
	title = strings.TrimLeft(t[n:], " \t")
	title = strings.TrimRight(title, " \t")
	title = strings.TrimRight(title, "#")
	return strings.TrimRight(title, " \t"), true
}

// mdUnquote takes the quote mark off a line: up to three spaces, a >, and one space after it.
func mdUnquote(l string) (rest string, ok bool) {
	t := strings.TrimLeft(l, " ")
	if len(l)-len(t) > 3 || t == "" || t[0] != '>' {
		return "", false
	}
	t = t[1:]
	if t != "" && (t[0] == ' ' || t[0] == '\t') {
		t = t[1:]
	}
	return t, true
}

// mdBullet reads a list item that starts with - * or +, and gives its indent and its text.
func mdBullet(l string) (indent int, item string, ok bool) {
	t := strings.TrimLeft(l, " ")
	if len(t) < 2 || strings.IndexByte("-*+", t[0]) < 0 || (t[1] != ' ' && t[1] != '\t') {
		return 0, "", false
	}
	return len(l) - len(t), strings.TrimLeft(t[1:], " \t"), true
}

// mdNumber reads a list item that starts with a number of up to three digits and a . or ), and gives its indent, number and text.
func mdNumber(l string) (indent int, num, item string, ok bool) {
	t := strings.TrimLeft(l, " ")
	d := 0
	for d < len(t) && d <= 3 && t[d] >= '0' && t[d] <= '9' {
		d++
	}
	if d < 1 || d > 3 || d+1 >= len(t) || (t[d] != '.' && t[d] != ')') || (t[d+1] != ' ' && t[d+1] != '\t') {
		return 0, "", "", false
	}
	return len(l) - len(t), t[:d], strings.TrimLeft(t[d+1:], " \t"), true
}

// mdListOrText converts a list item to a bullet or keeps its number; anything else is a line of text. Slack has no lists in
// text, so the bullet is a character, and the level shows in the indent and in the character.
func mdListOrText(l string) string {
	if indent, item, ok := mdBullet(l); ok && !mdIsRule(l) {
		marks := []string{"•", "◦", "▪"}
		level := indent / 2
		switch {
		case strings.HasPrefix(item, "[ ] "):
			item = "☐ " + item[4:]
		case strings.HasPrefix(item, "[x] "), strings.HasPrefix(item, "[X] "):
			item = "☑ " + item[4:]
		}
		return strings.Repeat("  ", min(level, 4)) + marks[min(level, len(marks)-1)] + " " + mdLine(item, mdFlags{})
	}
	if indent, num, item, ok := mdNumber(l); ok {
		return strings.Repeat("  ", min(indent/2, 4)) + num + ". " + mdLine(item, mdFlags{})
	}
	return mdLine(l, mdFlags{})
}

// mdCodeBlock writes a code block. Nothing inside is Markdown, but Slack reads the entities in code too, so they are escaped,
// and the text is made unable to close the block it is in.
func mdCodeBlock(body string) string {
	body = strings.Trim(body, "\n")
	if strings.TrimSpace(body) == "" {
		return ""
	}
	return "```\n" + mdText(body) + "\n```"
}

// mdText escapes text that is not Markdown. It also takes three backticks apart, which is what closes a code block: every
// backtick of a run of three or more becomes ˋ (U+02CB, a modifier letter that looks like one). A run of one or two cannot close
// anything and stays.
func mdText(s string) string {
	s = Escape(s)
	if !strings.Contains(s, "```") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		j := strings.IndexByte(s[i:], '`')
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+j])
		i += j
		n := mdRun(s, i, '`')
		if n >= 3 {
			b.WriteString(strings.Repeat("ˋ", n))
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// mdIsTableSep says whether the line is the row under the head of a table: cells of dashes, with colons for the alignment.
func mdIsTableSep(l string) bool {
	l = strings.Trim(l, " \t")
	l = strings.TrimPrefix(l, "|")
	l = strings.TrimSuffix(l, "|")
	if l == "" {
		return false
	}
	for _, c := range strings.Split(l, "|") {
		c = strings.Trim(c, " \t")
		c = strings.TrimPrefix(c, ":")
		c = strings.TrimSuffix(c, ":")
		if c == "" || strings.Trim(c, "-") != "" {
			return false
		}
	}
	return true
}

// mdTable draws a Markdown table as text with aligned columns. Slack has no tables; a block of fixed-width text keeps the
// columns. A row has only the cells it has, so that a row of one bar does not grow into as many columns as the widest row. And
// when padding the columns would make the table much longer than it was written (one long cell above a thousand short ones),
// the cells are not padded at all.
func mdTable(head string, rows []string) string {
	all := [][]string{mdCells(head)}
	size := len(head)
	for _, r := range rows {
		all = append(all, mdCells(r))
		size += len(r) + 1
	}
	cols := 0
	for _, r := range all {
		cols = max(cols, len(r))
	}
	width := make([]int, cols)
	widths := make([][]int, len(all))
	for n, r := range all {
		widths[n] = make([]int, len(r))
		for c, v := range r {
			widths[n][c] = mdTextWidth(v)
			width[c] = max(width[c], widths[n][c])
		}
	}
	padding := 0
	for n, r := range all {
		for c := 0; c+1 < len(r); c++ {
			padding += width[c] - widths[n][c]
		}
	}
	aligned := padding <= size
	var out []string
	for n, r := range all {
		parts := make([]string, len(r))
		for c, v := range r {
			parts[c] = v
			if aligned {
				parts[c] += strings.Repeat(" ", width[c]-widths[n][c])
			}
		}
		out = append(out, strings.TrimRight(strings.Join(parts, " │ "), " "))
		if n == 0 {
			rule := make([]string, cols)
			for c := range rule {
				rule[c] = strings.Repeat("─", width[c])
			}
			out = append(out, strings.Join(rule, "─┼─"))
		}
	}
	return strings.Join(out, "\n")
}

var mdPlain = strings.NewReplacer("**", "", "__", "", "~~", "", "`", "")

func mdCells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	var out []string
	for _, c := range strings.Split(row, "|") {
		out = append(out, mdPlain.Replace(strings.TrimSpace(c)))
	}
	return out
}

// mdTextWidth is the width in columns of a fixed-width font: wide East Asian characters and most emoji take two.
func mdTextWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && (r <= 0x115f || (r >= 0x2e80 && r <= 0xa4cf) || (r >= 0xac00 && r <= 0xd7a3) || (r >= 0xf900 && r <= 0xfaff) || (r >= 0xfe30 && r <= 0xfe6f) || (r >= 0xff00 && r <= 0xff60) || (r >= 0xffe0 && r <= 0xffe6) || (r >= 0x1f300 && r <= 0x1faff)):
			w += 2
		case unicode.Is(unicode.Mn, r) || r == 0x200d || r == 0xfe0f:
			// combining marks and joiners take no room
		default:
			w++
		}
	}
	return w
}

type mdFlags struct {
	open  uint8 // the kinds of span that are open here: inside one, a second of its kind only drops its marks
	plain bool  // inside the text of a link: Slack shows no formatting there, so no marks are written at all
	depth int   // how many spans deep this is
}

func mdIsPunct(c byte) bool { return strings.IndexByte("\\`*_{}[]()#+-.!~|>&<\"'", c) >= 0 }

func mdIsWordByte(c byte) bool {
	return c >= 0x80 || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func mdIsSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' }

func mdRun(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

// mdConv is the state of the conversion of one line. A line may make it read only so much (a few dozen times the length of the
// line), and past that the line is no longer searched for formatting and the rest of it is text. That is what keeps a hostile
// line, a million openers with no closer, from costing the square of its length.
type mdConv struct{ left int }

func mdLine(s string, f mdFlags) string {
	c := &mdConv{left: mdWorkBase + mdWorkPerByte*len(s)}
	return mdBreakTicks(c.inline(s, f))
}

// mdBreakTicks takes apart every run of three or more backticks in a converted line. The converter never writes one on purpose
// in a line of text (the fence of a code block is written by mdCodeBlock), but marks that drop out of the text can bring the
// backticks on both sides of them together (` + "`**“a**`" + `: the two ** go, and "```a" is left), and Slack would open a code block there
// that swallows the rest of the message.
func mdBreakTicks(s string) string {
	if !strings.Contains(s, "```") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '`' {
			j := strings.IndexByte(s[i:], '`')
			if j < 0 {
				b.WriteString(s[i:])
				break
			}
			b.WriteString(s[i : i+j])
			i += j
			continue
		}
		n := mdRun(s, i, '`')
		if n >= 3 {
			b.WriteString(strings.Repeat("ˋ", n))
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// limit is how far a search from `from` may read; pay charges for what was read.
func (c *mdConv) limit(s string, from int) int {
	if c.left <= 0 {
		return from
	}
	return min(len(s), from+c.left)
}

func (c *mdConv) pay(from, to int) { c.left -= to - from }

// mdCodeMap marks where, in s, an escape or a code span starts, with the index after it. A code span is a run of backticks and
// the next run of the same length; a run with no partner marks nothing. It is made once for a line, so that the searches for
// the end of a bold or a link step over code in one move and do not look for the partner of every run again.
func mdCodeMap(s string) []int32 {
	if (strings.IndexByte(s, '`') < 0 && strings.IndexByte(s, '\\') < 0) || len(s) >= 1<<30 {
		return nil
	}
	var runs map[int][]int // the start of every run of backticks, by its length
	for i := strings.IndexByte(s, '`'); i >= 0; {
		n := mdRun(s, i, '`')
		if runs == nil {
			runs = map[int][]int{}
		}
		runs[n] = append(runs[n], i)
		j := strings.IndexByte(s[i+n:], '`')
		if j < 0 {
			break
		}
		i += n + j
	}
	code := make([]int32, len(s))
	for i := 0; i < len(s); {
		switch s[i] {
		case '\\':
			if i+1 < len(s) && mdIsPunct(s[i+1]) {
				code[i] = int32(i + 2)
				i += 2
				continue
			}
		case '`':
			n := mdRun(s, i, '`')
			list := runs[n]
			if k := sort.SearchInts(list, i+n); k < len(list) {
				code[i] = int32(list[k] + n)
				i = list[k] + n
			} else {
				i += n
			}
			continue
		}
		i++
	}
	return code
}

// mdSkip returns the index after the escape or code span that starts at j, or j when there is none.
func mdSkip(code []int32, j int) int {
	if code != nil && code[j] > 0 {
		return int(code[j])
	}
	return j
}

// mdMark writes inner between the marks of a span of one kind, unless that kind is open already (then it is text of the span
// that is open) or the text is the text of a link, where Slack shows no formatting.
func mdMark(f mdFlags, kind uint8, mark, inner string) string {
	if f.plain || f.open&kind != 0 {
		return inner
	}
	// Slack takes a span only when its marks touch the text, so white space at the edges goes outside them, and a span with
	// nothing in it is no span.
	lead := len(inner) - len(strings.TrimLeft(inner, " \t"))
	trail := len(inner) - len(strings.TrimRight(inner, " \t"))
	if lead+trail >= len(inner) {
		return inner
	}
	return inner[:lead] + mark + inner[lead:len(inner)-trail] + mark + inner[len(inner)-trail:]
}

func mdKind(c byte) uint8 {
	switch c {
	case '*':
		return mdBold
	case '_':
		return mdItalic
	}
	return mdStrike
}

// mdLookalike is the character that stands for a mark where the mark itself would end the span it is in.
func mdLookalike(c byte) string {
	switch c {
	case '*':
		return "∗" // U+2217 asterisk operator
	case '_':
		return "＿" // U+FF3F fullwidth low line
	}
	return "∼" // U+223C tilde operator
}

// mdInert says whether the mark at s[i] is one that Slack reads as text: between two word characters (snake_case, 2*3*4) or
// between two spaces (a * b). Any other mark could start or end a span.
func mdInert(s string, i int) bool {
	m := s[i]
	var prev, next byte
	if i > 0 {
		prev = s[i-1]
	}
	if i+1 < len(s) {
		next = s[i+1]
	}
	word := func(c byte) bool { return c != m && mdIsWordByte(c) }
	return (word(prev) && word(next)) || (mdIsSpace(prev) && mdIsSpace(next))
}

// literal writes the character s[i] as text. A mark that is text in the Markdown is written as it is, with one exception:
// inside a span of its own kind (a * in bold) Slack could take it for the end of that span and cut the span short, so there it
// is replaced by a look-alike unless Slack reads it as text anyway. A backtick that was escaped is replaced too: a lone
// backtick is harmless, but Slack has no escape, and an escaped one would pair with another and make a code span.
func (c *mdConv) literal(b *strings.Builder, s string, i int, f mdFlags) {
	switch ch := s[i]; ch {
	case '*', '_', '~':
		if f.open&mdKind(ch) != 0 && !mdInert(s, i) {
			b.WriteString(mdLookalike(ch))
			return
		}
		b.WriteByte(ch)
	case '`':
		b.WriteString("ˋ")
	default:
		b.WriteString(Escape(s[i : i+1]))
	}
}

// mdCodeSpan writes the code between two runs of backticks. Slack has no way to put a backtick in a span, so each one in the
// text becomes ˋ and the span stays a span. A span of white space only shows nothing in Slack and is written as the white space.
func mdCodeSpan(code string, f mdFlags) string {
	if len(code) >= 2 && code[0] == ' ' && code[len(code)-1] == ' ' && strings.TrimSpace(code) != "" {
		code = code[1 : len(code)-1]
	}
	text := strings.ReplaceAll(Escape(code), "`", "ˋ")
	if f.plain || strings.TrimSpace(code) == "" {
		return text
	}
	return "`" + text + "`"
}

// inline converts the spans inside a line: code, bold, italic, strike and links.
func (c *mdConv) inline(s string, f mdFlags) string {
	if f.depth >= mdMaxDepth {
		return mdText(s)
	}
	f.depth++
	code := mdCodeMap(s)
	lastLink := strings.LastIndex(s, "](") // a link cannot start after this
	var b strings.Builder
	b.Grow(len(s) + len(s)/8)
	for i := 0; i < len(s); {
		ch := s[i]
		switch {
		case ch == '\\' && code != nil && code[i] > 0:
			c.literal(&b, s, i+1, f)
			i += 2

		case ch == '`':
			n := mdRun(s, i, '`')
			if code != nil && code[i] > 0 {
				end := int(code[i])
				b.WriteString(mdCodeSpan(s[i+n:end-n], f))
				i = end
				break
			}
			// A run with no partner is text. From three on it could close or open a code block, so it is taken apart.
			if n >= 3 {
				b.WriteString(strings.Repeat("ˋ", n))
			} else {
				b.WriteString(s[i : i+n])
			}
			i += n

		case ch == '*' && strings.HasPrefix(s[i:], "***"):
			if span, next, ok := c.delimited(s, code, i, "***"); ok {
				g := f
				g.open |= mdBold | mdItalic
				inner := mdMark(f, mdItalic, "_", c.inline(span, g))
				b.WriteString(mdMark(f, mdBold, "*", inner))
				i = next
				break
			}
			c.literal(&b, s, i, f)
			i++

		case ch == '*' && strings.HasPrefix(s[i:], "**"):
			if span, next, ok := c.delimited(s, code, i, "**"); ok {
				g := f
				g.open |= mdBold
				b.WriteString(mdMark(f, mdBold, "*", c.inline(span, g)))
				i = next
				break
			}
			c.literal(&b, s, i, f)
			c.literal(&b, s, i+1, f)
			i += 2

		case ch == '~' && strings.HasPrefix(s[i:], "~~"):
			if span, next, ok := c.delimited(s, code, i, "~~"); ok {
				g := f
				g.open |= mdStrike
				b.WriteString(mdMark(f, mdStrike, "~", c.inline(span, g)))
				i = next
				break
			}
			c.literal(&b, s, i, f)
			c.literal(&b, s, i+1, f)
			i += 2

		case ch == '*' || ch == '_':
			if span, next, ok := c.italic(s, code, i); ok {
				g := f
				g.open |= mdItalic
				b.WriteString(mdMark(f, mdItalic, "_", c.inline(span, g)))
				i = next
				break
			}
			c.literal(&b, s, i, f)
			i++

		case ch == '~':
			c.literal(&b, s, i, f)
			i++

		case ch == '[' || (ch == '!' && i+1 < len(s) && s[i+1] == '[' && !f.plain):
			start := i
			if ch == '!' {
				i++
			}
			if !f.plain && i <= lastLink {
				if out, next, ok := c.link(s, code, i, f); ok {
					b.WriteString(out)
					i = next
					break
				}
			}
			b.WriteString(s[start : i+1])
			i++

		default:
			j := i + 1
			for j < len(s) && !mdSpecial[s[j]] {
				j++
			}
			_, _ = mdEscaper.WriteString(&b, s[i:j])
			i = j
		}
	}
	return b.String()
}

// delimited finds the span between the opener m at s[i:] and its closer. The opener is followed by a non-space and the closer
// preceded by one. It reads no further than the line may be searched.
func (c *mdConv) delimited(s string, code []int32, i int, m string) (span string, next int, ok bool) {
	start := i + len(m)
	if start >= len(s) || mdIsSpace(s[start]) {
		return "", 0, false
	}
	limit := c.limit(s, start)
	for j := start; j < limit; {
		if k := mdSkip(code, j); k != j {
			j = k
			continue
		}
		if j > start && !mdIsSpace(s[j-1]) && strings.HasPrefix(s[j:limit], m) {
			c.pay(start, j+len(m))
			return s[start:j], j + len(m), true
		}
		j++
	}
	c.pay(start, limit)
	return "", 0, false
}

// italic finds *text* or _text_: the opener is not preceded by a word character and is followed by a non-space; the closer is
// preceded by a non-space and not followed by a word character. So snake_case and 2*3*4 are left alone.
func (c *mdConv) italic(s string, code []int32, i int) (span string, next int, ok bool) {
	ch := s[i]
	start := i + 1
	if start >= len(s) || mdIsSpace(s[start]) || s[start] == ch || (i > 0 && (mdIsWordByte(s[i-1]) || s[i-1] == ch)) {
		return "", 0, false
	}
	limit := c.limit(s, start)
	for j := start; j < limit; {
		if k := mdSkip(code, j); k != j {
			j = k
			continue
		}
		if s[j] != ch {
			j++
			continue
		}
		r := mdRun(s[:limit], j, ch)
		after := j + r
		if after == limit && limit < len(s) {
			break // the run goes on where the line may not be read
		}
		if r == 1 && j > start && !mdIsSpace(s[j-1]) && (after >= len(s) || !mdIsWordByte(s[after])) {
			c.pay(start, after)
			return s[start:j], after, true
		}
		j = after
	}
	c.pay(start, limit)
	return "", 0, false
}

// link reads [text](address) at s[i] and writes it as <address|text>. Only http, https and mailto addresses that pass mdURL
// are accepted; anything else is not a link and its characters are text, so the address is shown as it was written, escaped.
// The text is plain: a | in it would end it, so it is written as ¦, and Slack shows no formatting in it.
func (c *mdConv) link(s string, code []int32, i int, f mdFlags) (out string, next int, ok bool) {
	limit := c.limit(s, i)
	depth, j := 0, i
	for ; j < limit; j++ {
		if k := mdSkip(code, j); k != j {
			j = k - 1
			continue
		}
		if s[j] == '[' {
			depth++
		} else if s[j] == ']' {
			depth--
		}
		if depth == 0 {
			break
		}
	}
	if j >= limit {
		c.pay(i, limit)
		return "", 0, false
	}
	c.pay(i, j+1)
	dest, next, read, ok := mdDest(s[:c.limit(s, j+1)], j+1)
	c.pay(j+1, read)
	if !ok {
		return "", 0, false
	}
	url, ok := mdURL(dest)
	if !ok {
		return "", 0, false
	}
	text := s[i+1 : j]
	if text == dest {
		return "<" + url + ">", next, true
	}
	label := strings.TrimSpace(c.inline(text, mdFlags{plain: true, depth: f.depth}))
	if label == "" {
		return "", 0, false
	}
	label = mdShowHost(label, dest)
	return "<" + url + "|" + strings.ReplaceAll(label, "|", "¦") + ">", next, true
}

// mdDest reads the (address) or (address "title") that follows the text of a link at s[p], and returns the address and the
// index after the closing bracket. The address may hold balanced brackets and ends at a space or at the closing bracket; the
// title is dropped. It reads only as much as an address and a title can take, so that an unclosed bracket costs little, and
// read is how far it looked, for the line to be charged with.
func mdDest(s string, p int) (dest string, next, read int, ok bool) {
	if p >= len(s) || s[p] != '(' {
		return "", 0, p + 1, false
	}
	end := min(len(s), p+1+mdMaxURL+mdMaxTitle)
	q := p + 1
	for q < end && (s[q] == ' ' || s[q] == '\t') {
		q++
	}
	from, depth := q, 0
scan:
	for ; q < end; q++ {
		switch s[q] {
		case ' ', '\t':
			break scan
		case '(':
			depth++
		case ')':
			if depth == 0 {
				break scan
			}
			depth--
		}
	}
	if q >= end {
		return "", 0, end, false
	}
	dest = s[from:q]
	r := q
	for r < end && (s[r] == ' ' || s[r] == '\t') {
		r++
	}
	if r > q && r < end && (s[r] == '"' || s[r] == '\'') {
		k := strings.IndexByte(s[r+1:end], s[r])
		if k < 0 {
			return "", 0, end, false
		}
		r += k + 2
		for r < end && (s[r] == ' ' || s[r] == '\t') {
			r++
		}
	}
	if r >= end || s[r] != ')' {
		return "", 0, min(r+1, end), false
	}
	return dest, r + 1, r + 1, true
}

func mdHasScheme(u, scheme string) bool {
	if len(u) <= len(scheme) {
		return false // an address with nothing after the scheme is no address
	}
	for i := 0; i < len(scheme); i++ {
		c := u[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != scheme[i] {
			return false
		}
	}
	return true
}

// mdURL checks an address and writes it as it goes between the angle brackets of a link: the scheme in lower case and & as
// &amp;, nothing else changed. Only http://, https:// and mailto: are accepted, at most mdMaxURL bytes, and none of the
// characters that would end the address or the link (a space, <, >, |), start a code span (a backtick), or are invisible or
// control characters.
func mdURL(u string) (string, bool) {
	if len(u) > mdMaxURL {
		return "", false
	}
	scheme := ""
	for _, sc := range []string{"https://", "http://", "mailto:"} {
		if mdHasScheme(u, sc) {
			scheme = sc
			break
		}
	}
	if scheme == "" {
		return "", false
	}
	for _, r := range u {
		if r == '<' || r == '>' || r == '|' || r == '`' || r == utf8.RuneError || unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", false
		}
	}
	return strings.ReplaceAll(scheme+u[len(scheme):], "&", "&amp;"), true
}

// SplitPlain cuts plain text into pieces of at most max bytes: at a line break where it can, otherwise at a space, otherwise
// between two characters, never inside one. The break character it cuts at, one "\n" or one " ", is not in either piece, so the
// pieces joined with "" give back the text without one byte for each cut at a break; a cut between characters loses nothing.
// Text that fits is one piece, as it is, and empty text is nil. A max below the size of a character gives pieces of one
// character each, which are longer than max; a max below one is taken as one.
func SplitPlain(s string, max int) []string {
	if s == "" {
		return nil
	}
	if max < 1 {
		max = 1
	}
	if len(s) <= max {
		return []string{s}
	}
	var out []string
	for len(s) > max {
		cut, skip := mdBreak(s, max)
		out = append(out, s[:cut])
		s = s[cut+skip:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// mdBreak finds where to cut s, which is longer than max: the last line break among the first max bytes after the first, else
// the last space, else the last character boundary. skip is the length of the break character that is left out.
func mdBreak(s string, max int) (cut, skip int) {
	window := s[1 : max+1]
	if i := strings.LastIndexByte(window, '\n'); i >= 0 {
		return i + 1, 1
	}
	if i := strings.LastIndexByte(window, ' '); i >= 0 {
		return i + 1, 1
	}
	cut = max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 {
		_, cut = utf8.DecodeRuneInString(s)
	}
	return cut, 0
}
