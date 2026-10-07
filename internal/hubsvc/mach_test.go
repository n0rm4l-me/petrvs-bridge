package hubsvc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
)

var machCardNow = time.Unix(1_800_000_000, 0)

// machCardHub is a hub whose only agent was last heard d ago; state says whether it ever connected and is online.
func machCardHub(t *testing.T, heard time.Duration, online bool, info map[string]string) *Hub {
	t.Helper()
	c := base(t)
	c.Now = func() time.Time { return machCardNow }
	h, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	st := h.agents["laptop"]
	st.everConnected = true
	st.lastSeen = machCardNow.Add(-heard)
	if online {
		st.cur = &sess{}
	}
	st.info = info
	return h
}

func machFullInfo() map[string]string {
	return map[string]string{"bat": "83% charging", "up": "3d4h", "disk": "212G free of 460G", "tmux": "ok", "cc": "claude", "mode": "auto",
		"q": "0", "locked": "true", "alarm": "on"}
}

func TestTheStatusCardOfAnOnlineAgent(t *testing.T) {
	h := machCardHub(t, 12*time.Second, true, machFullInfo())
	want := "laptop — online, heard 12 s ago\n" +
		"🔋 83% charging · ⏱ up 3d4h · 💾 212G free of 460G\n" +
		"🖥 tmux ok · claude · auto mode · queue 0\n" +
		"🔒 locked · 🚨 alarm armed"
	if got := h.statusText(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTheCardOmitsWhatTheAgentDidNotReport(t *testing.T) {
	// A desktop: no battery. Not yet a screen reading. Not locked, no alarm.
	h := machCardHub(t, 3*time.Second, true, map[string]string{"up": "9h1m", "disk": "1.8T free of 2T", "tmux": "ok", "locked": "false"})
	want := "laptop — online, heard 3 s ago\n" +
		"⏱ up 9h1m · 💾 1.8T free of 2T\n" +
		"🖥 tmux ok\n" +
		"🔓 unlocked"
	if got := h.statusText(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := machCardHub(t, time.Second, true, nil).statusText(); got != "laptop — online, heard 1 s ago" {
		t.Fatalf("%q", got)
	}
}

func TestAnOfflineAgentShowsItsLastKnownFactsAsOld(t *testing.T) {
	h := machCardHub(t, 5*time.Minute+10*time.Second, false, machFullInfo())
	got := h.statusText()
	want := "laptop — offline, last heard 5 min ago\n" +
		"🕓 last known:\n" +
		"🔋 83% charging · ⏱ up 3d4h · 💾 212G free of 460G\n" +
		"🖥 tmux ok · claude · auto mode · queue 0\n" +
		"🔒 locked · 🚨 alarm armed"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// An agent that went offline before it ever sent a heartbeat has nothing old to show.
	if got := machCardHub(t, time.Hour, false, nil).statusText(); got != "laptop — offline, last heard 1 h ago" {
		t.Fatalf("%q", got)
	}
}

func TestAnAgentThatNeverConnectedSaysSo(t *testing.T) {
	h, _ := New(base(t))
	if got := h.statusText(); got != "laptop — never connected" {
		t.Fatalf("%q", got)
	}
}

func TestTheCardHasRoomForTheRestOfTheHeartbeat(t *testing.T) {
	info := machFullInfo()
	info["host"], info["os"], info["alarm_blind"] = "mac-mini", "darwin", "input"
	info["zz"], info["aa"] = "1", "2"
	got := machCardHub(t, time.Second, true, info).statusText()
	for _, want := range []string{"⚠️ alarm blind: input", "💻 mac-mini (darwin)", "aa=2 · zz=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing from:\n%s", want, got)
		}
	}
	// A low battery that is not charging has its own icon; so does a plain one that is.
	if got := machCardHub(t, time.Second, true, map[string]string{"bat": "9%"}).statusText(); !strings.Contains(got, "🪫 9%") {
		t.Errorf("%s", got)
	}
	if got := machCardHub(t, time.Second, true, map[string]string{"bat": "9% charging"}).statusText(); !strings.Contains(got, "🔋 9% charging") {
		t.Errorf("%s", got)
	}
}

func TestTheCardCannotBeBrokenByWhatAnAgentSends(t *testing.T) {
	info := map[string]string{"cc": "claude\n🔒 locked · fake line", "mode": strings.Repeat("x", 500), "bat": "\x1b[31m50%"}
	got := machCardHub(t, time.Second, true, info).statusText()
	if strings.Count(got, "\n") != 2 { // the head, the battery, the session
		t.Fatalf("an agent made lines of its own:\n%q", got)
	}
	if len([]rune(got)) > 300 {
		t.Fatalf("%d characters", len([]rune(got)))
	}
}

func TestSeveralAgentsAreSeparatedAndTheActiveOneIsMarked(t *testing.T) {
	c := two(t)
	c.DefaultAgent = "desk"
	c.Now = func() time.Time { return machCardNow }
	h, _ := New(c)
	got := h.statusText()
	if !strings.Contains(got, "● desk — never connected") || !strings.Contains(got, "\n\nlaptop — never connected") && !strings.Contains(got, "laptop — never connected\n\n") {
		t.Fatalf("%q", got)
	}
	if strings.Count(got, "●") != 1 {
		t.Fatalf("%q", got)
	}
	// Nobody chosen, and a lockdown: the hub's own lines follow the cards, set apart.
	c = two(t)
	h, _ = New(c)
	h.locked = true
	got = h.statusText()
	if !strings.HasSuffix(got, "\n\nNo agent is chosen yet: pick one with /agent NAME or the buttons.\nHub is in lockdown: nothing is relayed.") {
		t.Fatalf("%q", got)
	}
}

func TestAgoIsReadable(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "0 s", 0: "0 s", 12 * time.Second: "12 s", 59 * time.Second: "59 s", time.Minute: "1 min", 5*time.Minute + 59*time.Second: "5 min",
		time.Hour: "1 h", 3*time.Hour + 5*time.Minute: "3 h 5 min", 47 * time.Hour: "47 h", 50 * time.Hour: "2 d", 10 * 24 * time.Hour: "10 d",
	} {
		if got := ago(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

func TestTheStatusAndAgentsCommandsShowTheCardWithTheAgentButtons(t *testing.T) {
	c := two(t)
	c.Now = func() time.Time { return machCardNow }
	fake := c.Telegram.(*telegram.Fake)
	h, _ := New(c)
	st := h.agents["laptop"]
	st.everConnected, st.lastSeen, st.cur, st.info = true, machCardNow.Add(-2*time.Second), &sess{}, machFullInfo()
	h.HandleText(context.Background(), "/status")
	h.HandleText(context.Background(), "/agents")
	sent := fake.Sent()
	if len(sent) != 2 {
		t.Fatalf("%v", sent)
	}
	for _, s := range sent {
		if !strings.Contains(s.Text, "laptop — online, heard 2 s ago\n🔋 83% charging") || len(s.Buttons) != 2 {
			t.Errorf("%+v", s)
		}
	}
}

func TestContinueIsListedInTheHelpButNotInTheMenu(t *testing.T) {
	if !strings.Contains(helpText, "/continue") {
		t.Fatal("not in the help")
	}
	for _, c := range fullMenu() {
		if c.Name == "continue" {
			t.Fatal("a command that starts a program is in the menu")
		}
	}
}

func TestContinueWithAnUnknownAgentNameIsRefusedAndTheKnownOneIsNotTheActiveOne(t *testing.T) {
	c := two(t)
	fake := c.Telegram.(*telegram.Fake)
	h, _ := New(c)
	h.HandleText(context.Background(), "/continue ghost")
	if s := fake.Sent(); len(s) != 1 || s[0].Text != "Unknown agent. /agents lists them." {
		t.Fatalf("%+v", s)
	}
	// With several agents and none chosen it asks instead of starting anything.
	h.HandleText(context.Background(), "/continue")
	if s := fake.Sent(); len(s) != 2 || !strings.Contains(s[1].Text, "No agent is chosen") || h.Stats().Relayed != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestAShellInThePaneIsFlaggedButClaudeCodeUnderAShellNameIsNot(t *testing.T) {
	got := machCardHub(t, time.Second, true, map[string]string{"tmux": "ok", "cc": "zsh"}).statusText()
	if !strings.Contains(got, "🖥 tmux ok · ⚠️ shell (zsh)") {
		t.Fatalf("%s", got)
	}
	got = machCardHub(t, time.Second, true, map[string]string{"tmux": "ok", "cc": "bash", "mode": "auto"}).statusText()
	if !strings.Contains(got, "🖥 tmux ok · bash · auto mode") || strings.Contains(got, "⚠️") {
		t.Fatalf("%s", got)
	}
	got = machCardHub(t, time.Second, true, map[string]string{"tmux": "ok", "cc": "node"}).statusText()
	if !strings.Contains(got, "🖥 tmux ok · node") || strings.Contains(got, "⚠️") {
		t.Fatalf("%s", got)
	}
}
