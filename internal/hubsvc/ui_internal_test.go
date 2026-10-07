package hubsvc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

func keyButton(label, key, prompt string) wire.Button {
	return wire.Button{Label: label, Key: key, Prompt: prompt}
}

func TestButtonsAreLaidOutThreeToARowWithCallbackDataThatSaysWhatTheyDo(t *testing.T) {
	h, _ := New(base(t))
	p := &wire.Payload{Buttons: []wire.Button{
		keyButton("1. Yes", "1", "3c83c1b3"), keyButton("2. Yes, and switch", "2", "3c83c1b3"), keyButton("3. No", "3", "3c83c1b3"), keyButton("✖ Esc", "Escape", "3c83c1b3"),
		keyButton("↑", "Up", ""), {Label: "📺 Screen", Key: wire.ButtonScreen},
	}}
	m := h.messageFor("laptop", p, "the text")
	if m.Text != "the text" || len(m.Buttons) != 2 || len(m.Buttons[0]) != 3 || len(m.Buttons[1]) != 3 {
		t.Fatalf("%+v", m)
	}
	var got []string
	for _, row := range m.Buttons {
		for _, b := range row {
			got = append(got, b.Text+"="+b.Data)
		}
	}
	want := "1. Yes=k:laptop:1:3c83c1b3 2. Yes, and switch=k:laptop:2:3c83c1b3 3. No=k:laptop:3:3c83c1b3 ✖ Esc=k:laptop:Escape:3c83c1b3 ↑=k:laptop:Up: 📺 Screen=s:laptop"
	if strings.Join(got, " ") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(got, " "), want)
	}
}

func TestAMessageWithoutButtonsStaysPlain(t *testing.T) {
	h, _ := New(base(t))
	if m := h.messageFor("laptop", &wire.Payload{}, "x"); m.Text != "x" || m.Buttons != nil {
		t.Fatalf("%+v", m)
	}
}

// Telegram allows 64 bytes of callback data. The longest the hub makes is an agent name of 32 characters, the longest
// key and a hash of 16: it fits. A button that would not fit is left out, never shortened into another button.
func TestCallbackDataFitsTheLimit(t *testing.T) {
	name := strings.Repeat("a", 32)
	for _, b := range []wire.Button{
		keyButton("x", "Escape", strings.Repeat("f", 16)), keyButton("x", "9", strings.Repeat("0", 16)), {Label: "x", Key: wire.ButtonScreen},
	} {
		d, ok := buttonData(name, b)
		if !ok || len(d) > maxCallbackData {
			t.Errorf("%+v: %q (%d bytes)", b, d, len(d))
		}
	}
	for what, agent := range map[string]string{"a name longer than any agent has": strings.Repeat("a", 33), "an empty name": "", "a name with the separator": "a:b"} {
		if d, ok := buttonData(agent, keyButton("x", "1", "abcd")); ok {
			t.Errorf("%s: %q", what, d)
		}
	}
	// the limit itself: if it were lower, the button goes
	if d, ok := buttonData(name, keyButton("x", "Escape", strings.Repeat("f", 16))); !ok || len(d) != 2+32+1+6+1+16 {
		t.Errorf("%q", d)
	}
}

// The hub does not make a button it would refuse to act on: BTab changes the permission mode and never comes from a
// button; the rest are not keys at all.
func TestButtonsThatTheHubWouldRefuseAreLeftOut(t *testing.T) {
	h, _ := New(base(t))
	bad := []wire.Button{
		keyButton("shift tab", "BTab", ""), keyButton("x", "btab", ""), keyButton("x", "Delete", ""), keyButton("x", "rm -rf /", ""), keyButton("x", "", ""),
		keyButton("x", "1;2", ""), keyButton("x", "1:2", ""), keyButton("x", "10", ""), keyButton("x", "0", ""), keyButton("x", "enter", ""),
		keyButton("x", "1", "has space"), keyButton("x", "1", "a:b"), keyButton("x", "1", "line\nbreak"), keyButton("x", "1", strings.Repeat("a", 17)),
		keyButton("", "1", "abcd"), // no label
	}
	good := keyButton("1. Yes", "1", "abcd")
	m := h.messageFor("laptop", &wire.Payload{Buttons: append(bad, good)}, "t")
	if len(m.Buttons) != 1 || len(m.Buttons[0]) != 1 || m.Buttons[0][0].Data != "k:laptop:1:abcd" {
		t.Fatalf("%+v", m.Buttons)
	}
	// nothing good left: no keyboard at all
	if m := h.messageFor("laptop", &wire.Payload{Buttons: bad}, "t"); len(m.Buttons) != 0 {
		t.Fatalf("%+v", m.Buttons)
	}
}

func TestEveryKeyAButtonMayPressIsOneTheAgentAccepts(t *testing.T) {
	for k := range buttonKeys {
		if k == "BTab" {
			t.Error("BTab may be pressed from a button")
		}
	}
	if len(buttonKeys) != 14 { // Enter Escape Up Down Tab 1..9
		t.Errorf("%d keys", len(buttonKeys))
	}
}

func TestEachMessageOfButtonsCanBeUsedOnce(t *testing.T) {
	h, _ := New(base(t))
	if !h.ui.claim(7) || h.ui.claim(7) || !h.ui.claim(8) {
		t.Fatal("a message's buttons can be used once")
	}
	if !h.ui.claim(0) || !h.ui.claim(0) {
		t.Fatal("a tap that names no message cannot be told apart")
	}
	for i := int64(100); i < 100+2*maxTappedMessages; i++ {
		h.ui.claim(i)
	}
	if len(h.ui.tapped) > maxTappedMessages || len(h.ui.ring) > maxTappedMessages {
		t.Fatalf("the record grew to %d", len(h.ui.tapped))
	}
	if !h.ui.claim(7) {
		t.Fatal("a very old message was still remembered")
	}
}

func TestAKeyFromTheChatAsksForTheModeOnlyWhenItIsShiftTab(t *testing.T) {
	for key, want := range map[string]bool{"BTab": true, "Enter": false, "1": false, "Escape": false, "Tab": false, "": false} {
		m := keyMeta(key)
		if (m[wire.MetaEcho] == "mode") != want || (!want && m != nil) {
			t.Errorf("%q: %v", key, m)
		}
	}
	// the ways of writing it reach the same key
	for _, in := range []string{"btab", "BTab", "shift+tab", "Shift-Tab"} {
		if k := canonicalKey(in); keyMeta(k)[wire.MetaEcho] != "mode" {
			t.Errorf("%q becomes %q", in, k)
		}
	}
}

func TestTheHelpNamesWhatThisPartAdds(t *testing.T) {
	for _, c := range []string{"/screen", "/sessions", "/stop", "/clear", "/compact", "/resume"} {
		if !strings.Contains(helpText, c) {
			t.Errorf("the help lacks %s", c)
		}
	}
	for _, c := range chatMenu() {
		if !strings.Contains(helpText, "/"+c.Name) {
			t.Errorf("/%s is in the menu but not in the help", c.Name)
		}
	}
	names := fmt.Sprint(chatMenu())
	for _, c := range []string{"clear", "compact", "resume"} {
		if strings.Contains(names, c) {
			t.Errorf("/%s is in the menu, where one tap would change the session", c)
		}
	}
}
