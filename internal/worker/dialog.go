package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// dialogRule is the line that tops a dialog on the screen of Claude Code; the agent looks for it to find where a prompt starts.
var dialogRule = strings.Repeat("─", 40)

const (
	maxDetail = 4000 // how much of one field of a request the dialog shows before the screen package cuts it
	maxLine   = 400  // a line of an Edit or Write request
)

// renderDialog draws a request for permission the way Claude Code draws it in a terminal, so that the screen package finds
// the prompt, makes its hash and the buttons, and so that a button of an earlier request cannot answer a later one: the number
// of the request is in the text, and so in the hash.
func renderDialog(tool string, seq int, input map[string]any, description, risk string) string {
	var b strings.Builder
	b.WriteString(dialogRule + "\n")
	fmt.Fprintf(&b, " %s request #%d\n\n", tool, seq)
	for _, l := range detailLines(tool, input) {
		b.WriteString("   " + l + "\n")
	}
	if description != "" {
		b.WriteString("   " + oneLine(description, maxLine) + "\n")
	}
	if risk != "" {
		b.WriteString("\n   ⚠ " + risk + "\n")
	}
	b.WriteString("\n Do you want to proceed?\n")
	b.WriteString(" ❯ 1. Yes\n")
	b.WriteString("   2. No, and tell the assistant what to do differently (esc)\n")
	return b.String()
}

// detailLines say what the request would do: the command, the file and what is written to it, or the input as JSON.
func detailLines(tool string, input map[string]any) []string {
	switch tool {
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			return numberedSafe(strings.Split(truncate(cmd, maxDetail), "\n"))
		}
	case "Write":
		path, _ := input["file_path"].(string)
		content, _ := input["content"].(string)
		return []string{"write " + oneLine(path, maxLine), fmt.Sprintf("(%d characters, %d lines)", len(content), strings.Count(content, "\n")+1)}
	case "Edit", "MultiEdit":
		path, _ := input["file_path"].(string)
		out := []string{"edit " + oneLine(path, maxLine)}
		if s, ok := input["old_string"].(string); ok {
			out = append(out, "- "+oneLine(s, maxLine))
		}
		if s, ok := input["new_string"].(string); ok {
			out = append(out, "+ "+oneLine(s, maxLine))
		}
		return out
	}
	return numberedSafe(strings.Split(truncate(compactJSON(input), maxDetail), "\n"))
}

// numberedSafe keeps a line of a command that starts like a numbered choice ("1. x") from being read as one of the choices.
func numberedSafe(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		t := strings.TrimLeft(l, " \t")
		if len(t) >= 2 && t[0] >= '0' && t[0] <= '9' {
			j := 0
			for j < len(t) && t[j] >= '0' && t[j] <= '9' {
				j++
			}
			if j < len(t) && t[j] == '.' {
				l = "·" + l
			}
		}
		out[i] = l
	}
	return out
}

func compactJSON(v map[string]any) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		raw, err := json.Marshal(v[k])
		if err != nil {
			raw = []byte("?")
		}
		parts = append(parts, k+": "+string(raw))
	}
	return strings.Join(parts, "\n")
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
	return truncate(s, max)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 { // do not cut a character in two
		cut--
	}
	return s[:cut] + "..."
}

// idleScreen is what the worker shows when nothing waits for an answer. It never holds text of the assistant: a numbered list in
// an answer would look like a prompt to the agent, which refuses to type while a prompt is on the screen.
func idleScreen(name, state string, since time.Duration, request string, queued int, cost float64, session string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (worker without a terminal)\n", name)
	switch state {
	case "working":
		fmt.Fprintf(&b, "working for %s on: %s\n", since.Round(time.Second), oneLine(strings.ReplaceAll(request, ".", "·"), 80))
	default:
		b.WriteString("idle, waiting for a message\n")
	}
	if queued > 0 {
		fmt.Fprintf(&b, "%d message(s) waiting\n", queued)
	}
	fmt.Fprintf(&b, "spent: $%.4f", cost)
	if session != "" {
		short := session
		if len(short) > 8 {
			short = short[:8]
		}
		fmt.Fprintf(&b, "  session: %s", short)
	}
	b.WriteString("\n")
	return b.String()
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
