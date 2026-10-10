package slack

import (
	"strings"
	"unicode/utf8"
)

// SplitMrkdwn cuts text that FromMarkdown made into pieces of at most max bytes (not less than 32), one message each.
//
// The text is cut AFTER it was converted, never before: cutting Markdown and converting the pieces would let the cut decide what
// the pieces mean (a code block cut in two is Markdown in its second half, and a link or a mark the author put inside it
// comes alive). Converted text is flat and has no such state: a code block is a line of three backticks up to the next such line
// (the converter never writes three backticks anywhere else), and every other construct, a span, a link, a quote mark, lives in
// one line. So a piece ends at a line break; a line that is longer than a piece is cut at a space, never inside a link or an
// entity if that can be helped, and never inside a character; and where a cut falls inside a code block the piece is closed with
// a fence line and the next one opened with one. A piece that has nothing in it but white space and fences is not sent.
func SplitMrkdwn(s string, max int) []string {
	if max < 32 {
		max = 32
	}
	if strings.TrimSpace(s) == "" {
		return nil
	}
	if len(s) <= max {
		return []string{s}
	}
	const fence = "```"
	var (
		parts  []string
		cur    []string
		size   int  // the bytes of cur, a line break counted after each line
		real   int  // the lines in cur that are not the fence that reopens a block
		inCode bool // we are inside a code block, as the text stands at the line being read
	)
	flush := func() {
		if real > 0 {
			text := strings.Join(cur, "\n")
			if inCode {
				text += "\n" + fence
			}
			if strings.TrimSpace(strings.ReplaceAll(text, fence, "")) != "" {
				parts = append(parts, text)
			}
		}
		cur, size, real = nil, 0, 0
		if inCode {
			cur, size = []string{fence}, len(fence)+1
		}
	}
	for _, l := range strings.Split(s, "\n") {
		// the room for this line: the piece, less the fence that closes a block when the piece ends inside one
		room := func() int {
			if inCode {
				return max - size - len(fence) - 1
			}
			return max - size
		}
		for len(l)+1 > room() {
			fresh := max // the room in a piece with nothing in it
			if inCode {
				fresh = max - 2*(len(fence)+1) // less the fence that reopens the block and the one that closes it
			}
			if real > 0 && len(l)+1 <= fresh {
				flush() // the line fits in a piece of its own
				break
			}
			if real > 0 && room() < max/4 {
				flush() // too little room left to be worth using
				continue
			}
			n := room() - 1
			if n < 1 {
				flush()
				continue
			}
			cut := mdCutLine(l, n, inCode)
			cur = append(cur, l[:cut])
			real++
			size += cut + 1
			flush()
			l = l[cut:]
			if l == "" {
				break
			}
		}
		if l == "" && size == 0 {
			continue
		}
		cur = append(cur, l)
		size += len(l) + 1
		real++
		if l == fence {
			inCode = !inCode
		}
	}
	if real > 0 {
		if text := strings.Join(cur, "\n"); strings.TrimSpace(strings.ReplaceAll(text, fence, "")) != "" {
			parts = append(parts, text)
		}
	}
	return parts
}

// mdCutLine finds where to cut the line l so that the head is at most n bytes: not inside a character, not inside an entity
// (&amp;), outside code not inside an <address|label> and at a space in the second half of the head when there is one.
func mdCutLine(l string, n int, inCode bool) int {
	if n >= len(l) {
		return len(l)
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(l[cut]) {
		cut--
	}
	if cut == 0 {
		_, w := utf8.DecodeRuneInString(l)
		return w // a character is longer than the room: it goes whole
	}
	// not inside an entity: a & within the last five bytes of the head with no ; after it
	if amp := strings.LastIndexByte(l[:cut], '&'); amp >= 0 && cut-amp <= 5 && strings.IndexByte(l[amp:cut], ';') < 0 && amp > 0 {
		cut = amp
	}
	if !inCode {
		// not inside a link: a < in the head with no > after it
		if open := strings.LastIndexByte(l[:cut], '<'); open > 0 && strings.IndexByte(l[open:cut], '>') < 0 {
			cut = open
		}
		// at a space, in the second half of the head, that is not inside a link
		for k := strings.LastIndexByte(l[:cut], ' '); k >= cut/2 && k > 0; k = strings.LastIndexByte(l[:k], ' ') {
			if !mdInsideLink(l, k) {
				return k + 1
			}
		}
	}
	return cut
}

// mdInsideLink says whether the byte at k is between the < and the > of a link.
func mdInsideLink(l string, k int) bool {
	open := strings.LastIndexByte(l[:k], '<')
	return open >= 0 && strings.IndexByte(l[open:k], '>') < 0
}
