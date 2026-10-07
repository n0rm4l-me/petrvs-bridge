// Package tgfmt turns the Markdown that the assistant and the agent write into the HTML that Telegram accepts, so that the chat
// shows bold, code blocks, lists and quotes instead of asterisks and backticks.
//
// It is conservative on purpose. Text that is not Markdown comes out unchanged (with &, < and > escaped), because a word with
// an underscore or a product like 2*3*4 is not formatting. Only what Telegram can show is produced: b, i, s, code, pre, a and
// blockquote. A table has no counterpart there and becomes an aligned block of fixed-width text. Whatever the input, the
// output is balanced HTML; and the sender falls back to the plain text if Telegram refuses a message all the same.
package tgfmt

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	fenceRe   = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})[ \t]*([^\\s`]*)[^`]*$")
	headingRe = regexp.MustCompile(`^ {0,3}#{1,6}[ \t]+(.*?)[ \t]*#*[ \t]*$`)
	quoteRe   = regexp.MustCompile(`^ {0,3}>[ \t]?(.*)$`)
	bulletRe  = regexp.MustCompile(`^( *)[-*+][ \t]+(.*)$`)
	numberRe  = regexp.MustCompile(`^( *)(\d{1,3})[.)][ \t]+(.*)$`)
	tableSep  = regexp.MustCompile(`^[ \t]*\|?[ \t]*:?-+:?[ \t]*(\|[ \t]*:?-+:?[ \t]*)*\|?[ \t]*$`)
	langRe    = regexp.MustCompile(`^[A-Za-z0-9_+#.-]{1,20}$`)
	linkRe    = regexp.MustCompile(`^https?://[^\s<>"]+$`)
)

// expandableAfter is the length of a quote from which it is shown folded, with a tap to open it.
const expandableAfter = 700

// HTML converts Markdown to Telegram HTML.
func HTML(src string) string { return render(src, false) }

// render is HTML; inQuote says the text is the inside of a quote, where Telegram takes no further quote, so a nested one
// is shown as the lines of the quote it is in.
func render(src string, inQuote bool) string {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var out []string
	blank := true // no blank line at the start, and never two in a row
	for i := 0; i < len(lines); {
		l := lines[i]
		switch {
		case fenceRe.MatchString(l):
			m := fenceRe.FindStringSubmatch(l)
			mark, lang := m[1], m[2]
			j := i + 1
			var body []string
			for ; j < len(lines); j++ {
				if closesFence(lines[j], mark) {
					break
				}
				body = append(body, lines[j])
			}
			out = append(out, pre(strings.Join(body, "\n"), lang))
			i = j + 1 // past the closing fence; an unclosed block ends with the text
			blank = false

		case strings.Contains(l, "|") && i+1 < len(lines) && strings.Contains(lines[i+1], "|") && tableSep.MatchString(lines[i+1]) && strings.Contains(lines[i+1], "-"):
			j := i + 2
			for j < len(lines) && strings.Contains(lines[j], "|") && strings.TrimSpace(lines[j]) != "" {
				j++
			}
			out = append(out, pre(renderTable(lines[i], lines[i+2:j]), ""))
			i = j
			blank = false

		case headingRe.MatchString(l):
			out = append(out, "<b>"+inline(headingRe.FindStringSubmatch(l)[1], flags{bold: true})+"</b>")
			i++
			blank = false

		case isRule(l):
			out = append(out, "──────────")
			i++
			blank = false

		case quoteRe.MatchString(l) && inQuote:
			out = append(out, render(quoteRe.FindStringSubmatch(l)[1], true))
			i++
			blank = false

		case quoteRe.MatchString(l):
			var inner []string
			for ; i < len(lines) && quoteRe.MatchString(lines[i]); i++ {
				inner = append(inner, quoteRe.FindStringSubmatch(lines[i])[1])
			}
			text := strings.Join(inner, "\n")
			tag := "<blockquote>"
			if utf8.RuneCountInString(text) > expandableAfter {
				tag = "<blockquote expandable>"
			}
			if body := strings.TrimSpace(render(text, true)); body != "" {
				out = append(out, tag+body+"</blockquote>")
			}
			blank = false

		case strings.TrimSpace(l) == "":
			if !blank {
				out = append(out, "")
			}
			blank = true
			i++

		default:
			out = append(out, listOrText(l))
			i++
			blank = false
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// isRule says whether the line is a horizontal rule: three or more of one of - * _, with spaces allowed between.
func isRule(l string) bool {
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

// closesFence says whether the line ends a block opened with mark: the same character, at least as many, nothing else.
func closesFence(l, mark string) bool {
	t := strings.TrimLeft(l, " ")
	if len(l)-len(t) > 3 {
		return false
	}
	t = strings.TrimRight(t, " \t")
	return len(t) >= len(mark) && strings.Trim(t, mark[:1]) == ""
}

func pre(body, lang string) string {
	body = strings.Trim(body, "\n")
	if strings.TrimSpace(body) == "" {
		return ""
	}
	if langRe.MatchString(lang) {
		return `<pre><code class="language-` + lang + `">` + esc(body) + `</code></pre>`
	}
	return "<pre>" + esc(body) + "</pre>"
}

// listOrText converts a list item to a bullet or keeps its number; anything else is a line of text.
func listOrText(l string) string {
	if m := bulletRe.FindStringSubmatch(l); m != nil && !isRule(l) {
		marks := []string{"•", "◦", "▪"}
		level := len(m[1]) / 2
		item := m[2]
		switch {
		case strings.HasPrefix(item, "[ ] "):
			item = "☐ " + item[4:]
		case strings.HasPrefix(item, "[x] "), strings.HasPrefix(item, "[X] "):
			item = "☑ " + item[4:]
		}
		return strings.Repeat("  ", min(level, 4)) + marks[min(level, len(marks)-1)] + " " + inline(item, flags{})
	}
	if m := numberRe.FindStringSubmatch(l); m != nil {
		return strings.Repeat("  ", min(len(m[1])/2, 4)) + m[2] + ". " + inline(m[3], flags{})
	}
	return inline(l, flags{})
}

// renderTable draws a Markdown table as text with aligned columns. Telegram has no tables; a block of fixed-width text
// keeps the columns.
func renderTable(head string, rows []string) string {
	all := [][]string{cells(head)}
	for _, r := range rows {
		all = append(all, cells(r))
	}
	cols := 0
	for _, r := range all {
		cols = max(cols, len(r))
	}
	width := make([]int, cols)
	for _, r := range all {
		for c, v := range r {
			width[c] = max(width[c], textWidth(v))
		}
	}
	var out []string
	for n, r := range all {
		var parts []string
		for c := 0; c < cols; c++ {
			v := ""
			if c < len(r) {
				v = r[c]
			}
			parts = append(parts, v+strings.Repeat(" ", width[c]-textWidth(v)))
		}
		out = append(out, strings.TrimRight(strings.Join(parts, " │ "), " "))
		if n == 0 {
			var rule []string
			for c := 0; c < cols; c++ {
				rule = append(rule, strings.Repeat("─", width[c]))
			}
			out = append(out, strings.Join(rule, "─┼─"))
		}
	}
	return strings.Join(out, "\n")
}

var plainRe = strings.NewReplacer("**", "", "__", "", "~~", "", "`", "")

func cells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	var out []string
	for _, c := range strings.Split(row, "|") {
		out = append(out, plainRe.Replace(strings.TrimSpace(c)))
	}
	return out
}

// textWidth is the width in columns of a fixed-width font: wide East Asian characters and most emoji take two.
func textWidth(s string) int {
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

type flags struct {
	bold   bool // inside bold already: a ** only drops its marks
	noLink bool // inside a link's text
}

func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func escAttr(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

func isPunct(c byte) bool { return strings.IndexByte("\\`*_{}[]()#+-.!~|>&<\"'", c) >= 0 }

func isWordByte(c byte) bool {
	return c >= 0x80 || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' }

// inline converts the spans inside a line: code, bold, italic, strike and links.
func inline(s string, f flags) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && isPunct(s[i+1]):
			b.WriteString(esc(s[i+1 : i+2]))
			i += 2

		case c == '`':
			n := runLen(s, i, '`')
			j := closingRun(s, i+n, '`', n)
			if j < 0 {
				b.WriteString(s[i : i+n])
				i += n
				break
			}
			code := s[i+n : j]
			if len(code) >= 2 && code[0] == ' ' && code[len(code)-1] == ' ' && strings.TrimSpace(code) != "" {
				code = code[1 : len(code)-1]
			}
			if code == "" {
				b.WriteString(s[i : j+n])
			} else {
				b.WriteString("<code>" + esc(code) + "</code>")
			}
			i = j + n

		case c == '*' && strings.HasPrefix(s[i:], "***"):
			if span, next, ok := delimited(s, i, "***", false); ok {
				inner := "<i>" + inline(span, flags{bold: true, noLink: f.noLink}) + "</i>"
				if !f.bold {
					inner = "<b>" + inner + "</b>"
				}
				b.WriteString(inner)
				i = next
				break
			}
			b.WriteString("*")
			i++

		case c == '*' && strings.HasPrefix(s[i:], "**"):
			if span, next, ok := delimited(s, i, "**", false); ok {
				if f.bold {
					b.WriteString(inline(span, f))
				} else {
					b.WriteString("<b>" + inline(span, flags{bold: true, noLink: f.noLink}) + "</b>")
				}
				i = next
				break
			}
			b.WriteString("**")
			i += 2

		case c == '~' && strings.HasPrefix(s[i:], "~~"):
			if span, next, ok := delimited(s, i, "~~", false); ok {
				b.WriteString("<s>" + inline(span, f) + "</s>")
				i = next
				break
			}
			b.WriteString("~~")
			i += 2

		case c == '*' || c == '_':
			if span, next, ok := italic(s, i); ok {
				b.WriteString("<i>" + inline(span, f) + "</i>")
				i = next
				break
			}
			b.WriteByte(c)
			i++

		case c == '[' || (c == '!' && i+1 < len(s) && s[i+1] == '[' && !f.noLink):
			start := i
			if c == '!' {
				i++
			}
			if text, url, next, ok := link(s, i); ok && !f.noLink {
				b.WriteString(`<a href="` + escAttr(url) + `">` + inline(text, flags{bold: f.bold, noLink: true}) + "</a>")
				i = next
				break
			}
			i = start
			b.WriteString(esc(s[i : i+1]))
			i++

		default:
			_, size := utf8.DecodeRuneInString(s[i:])
			b.WriteString(esc(s[i : i+size]))
			i += size
		}
	}
	return b.String()
}

func runLen(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

// closingRun finds the next run of exactly n of c from `from`.
func closingRun(s string, from int, c byte, n int) int {
	for i := from; i < len(s); {
		if s[i] != c {
			i++
			continue
		}
		r := runLen(s, i, c)
		if r == n {
			return i
		}
		i += r
	}
	return -1
}

// skipCode returns the index after the code span or escape that starts at i, or i when there is none.
func skipCode(s string, i int) int {
	switch s[i] {
	case '\\':
		if i+1 < len(s) && isPunct(s[i+1]) {
			return i + 2
		}
	case '`':
		n := runLen(s, i, '`')
		if j := closingRun(s, i+n, '`', n); j >= 0 {
			return j + n
		}
		return i + n
	}
	return i
}

// delimited finds the span between the opener m at s[i:] and its closer. The opener is followed by a non-space and the
// closer preceded by one; with intraword set (for __) neither may touch a letter or digit outside the span.
func delimited(s string, i int, m string, wordRule bool) (span string, next int, ok bool) {
	start := i + len(m)
	if start >= len(s) || isSpace(s[start]) {
		return "", 0, false
	}
	if wordRule && i > 0 && isWordByte(s[i-1]) {
		return "", 0, false
	}
	for j := start; j < len(s); {
		if k := skipCode(s, j); k != j {
			j = k
			continue
		}
		if strings.HasPrefix(s[j:], m) && j > start && !isSpace(s[j-1]) {
			after := j + len(m)
			if !wordRule || after >= len(s) || !isWordByte(s[after]) {
				return s[start:j], after, true
			}
		}
		j++
	}
	return "", 0, false
}

// italic finds *text* or _text_: the opener is not preceded by a word character and is followed by a non-space; the
// closer is preceded by a non-space and not followed by a word character. So snake_case and 2*3*4 are left alone.
func italic(s string, i int) (span string, next int, ok bool) {
	c := s[i]
	start := i + 1
	if start >= len(s) || isSpace(s[start]) || s[start] == c || (i > 0 && (isWordByte(s[i-1]) || s[i-1] == c)) {
		return "", 0, false
	}
	for j := start; j < len(s); {
		if k := skipCode(s, j); k != j {
			j = k
			continue
		}
		if s[j] == c {
			r := runLen(s, j, c)
			after := j + r
			if r == 1 && j > start && !isSpace(s[j-1]) && (after >= len(s) || !isWordByte(s[after])) {
				return s[start:j], after, true
			}
			j = after
			continue
		}
		j++
	}
	return "", 0, false
}

// link reads [text](url) at s[i] and accepts only http and https addresses.
func link(s string, i int) (text, url string, next int, ok bool) {
	depth, j := 0, i
	for ; j < len(s); j++ {
		if k := skipCode(s, j); k != j {
			j = k - 1
			continue
		}
		if s[j] == '[' {
			depth++
		} else if s[j] == ']' {
			depth--
			if depth == 0 {
				break
			}
		}
	}
	if j >= len(s) || j+1 >= len(s) || s[j+1] != '(' {
		return "", "", 0, false
	}
	end := strings.IndexByte(s[j+2:], ')')
	if end < 0 {
		return "", "", 0, false
	}
	dest := strings.Fields(s[j+2 : j+2+end])
	if len(dest) == 0 || !linkRe.MatchString(dest[0]) {
		return "", "", 0, false
	}
	return s[i+1 : j], dest[0], j + 2 + end + 1, strings.TrimSpace(s[i+1:j]) != ""
}

// Split cuts Markdown into pieces of at most max bytes, at line breaks, so that each piece is Markdown of its own: a code
// block that a cut falls in is closed at the end of the piece and opened again at the start of the next. A line longer than
// max is cut inside, never in the middle of a character.
func Split(src string, max int) []string {
	const reserve = 80 // room for the closing and the reopening of a code block
	limit := max - reserve
	if limit < 64 {
		limit = max
	}
	if len(src) <= max {
		if strings.TrimSpace(src) == "" {
			return nil
		}
		return []string{src}
	}
	var parts []string
	var cur []string
	size := 0
	open := "" // the line that opened the code block we are in, "" outside one
	mark := ""
	flush := func() {
		text := strings.Join(cur, "\n")
		if open != "" {
			text += "\n" + mark
		}
		if strings.TrimSpace(strings.Trim(text, "`~\n")) != "" {
			parts = append(parts, text)
		}
		cur, size = nil, 0
		if open != "" {
			cur, size = []string{open}, len(open)+1
		}
	}
	for _, l := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		for len(l) > limit { // one line more than a piece holds
			room := limit - size - 1
			if room < limit/4 {
				flush()
				room = limit - size - 1
			}
			cut := room
			for cut > 0 && !utf8.RuneStart(l[cut]) {
				cut--
			}
			cur = append(cur, l[:cut])
			flush()
			l = l[cut:]
		}
		if size+len(l)+1 > limit && len(cur) > 0 {
			flush()
		}
		cur = append(cur, l)
		size += len(l) + 1
		switch {
		case open == "" && fenceRe.MatchString(l):
			open, mark = l, fenceRe.FindStringSubmatch(l)[1]
		case open != "" && closesFence(l, mark):
			open, mark = "", ""
		}
	}
	if len(cur) > 0 {
		text := strings.Join(cur, "\n")
		if strings.TrimSpace(text) != "" && (open == "" || len(cur) > 1) {
			parts = append(parts, text)
		}
	}
	return parts
}
