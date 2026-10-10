package slack

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/slack/slackid"
)

func TestTimestampsTurnIntoIdsAndBack(t *testing.T) {
	for _, ts := range []string{"1700000000.123456", "1759999999.000001", "999999999.999999"} {
		id, ok := tsToID(ts)
		if !ok {
			t.Fatalf("%s refused", ts)
		}
		back, ok := idToTS(id)
		if !ok || back != ts {
			t.Fatalf("%s -> %d -> %q", ts, id, back)
		}
	}
	if got := tsTime("1700000000.500000"); !got.Equal(time.Unix(1700000000, 500_000_000)) {
		t.Fatalf("time %v", got)
	}
}

func TestOddTimestampsAreRefused(t *testing.T) {
	for _, ts := range []string{"", "1700000000", "1700000000.1", "17000000000000.123456", "abc.def", "1700000000.123456 ", "-1700000000.123456", "1700000000.12345a"} {
		if _, ok := tsToID(ts); ok {
			t.Errorf("%q was accepted", ts)
		}
		if !tsTime(ts).IsZero() {
			t.Errorf("%q has a time", ts)
		}
	}
	for _, id := range []int64{0, -5} {
		if _, ok := idToTS(id); ok {
			t.Errorf("id %d turned into a timestamp", id)
		}
	}
}

func TestStrangersNeverGetTheNumbersOfTheAllowedChatAndUser(t *testing.T) {
	seen := map[int64]string{}
	for i := 0; i < 5000; i++ {
		s := strings.Repeat("U", 1) + strings.ToUpper(strings.Repeat("A", i%7)) + string(rune('0'+i%10)) + string(rune('A'+i%26))
		n := otherID(s + string(rune('A'+(i/26)%26)))
		if n == ChatID || n == UserID || n < 2 {
			t.Fatalf("%q got %d", s, n)
		}
		seen[n] = s
	}
	if otherID("D0ABC") != otherID("D0ABC") || otherID("D0ABC") == otherID("D0ABD") {
		t.Fatal("not stable or not distinct")
	}
}

func TestShapesOfIdsAndTokens(t *testing.T) {
	type c struct {
		s string
		f func(string) bool
	}
	good := []c{{"U0123ABCD", ValidUserID}, {"W0123ABCD", ValidUserID}, {"D0123ABCD", ValidChannelID},
		{"xoxb-1234567890-abcdef", ValidBotToken}, {"xapp-1-A0123-4567890-abcdef", ValidAppToken}}
	for _, g := range good {
		if !g.f(g.s) {
			t.Errorf("%q refused", g.s)
		}
	}
	bad := []c{{"u0123abcd", ValidUserID}, {"U01", ValidUserID}, {"C0123ABCD", ValidUserID}, {"U0123 ABCD", ValidUserID}, {"", ValidUserID},
		{"U0123ABCD\n", ValidUserID}, {"C0123ABCD", ValidChannelID}, {"G0123ABCD", ValidChannelID}, {"D0123ABCD/..", ValidChannelID},
		{"xapp-1-A0123-4567890-abcdef", ValidBotToken}, {"xoxb-1234567890-abcdef", ValidAppToken}, {"xoxb-short", ValidBotToken},
		{"xoxb-1234567890 abcdef", ValidBotToken}, {"xoxb-1234567890-abcdef\n", ValidBotToken}, {"xoxb-12345678\x00-abc", ValidBotToken},
		{strings.Repeat("xoxb-", 100), ValidBotToken}}
	for _, b := range bad {
		if b.f(b.s) {
			t.Errorf("%q accepted", b.s)
		}
	}
}

func TestEveryReactionTheHubMayUseHasAName(t *testing.T) {
	for _, e := range []string{"👀", "👎", "👍", "✅", "❤️", "❤", "⚠️"} {
		if n, ok := ReactionName(e); !ok || n == "" {
			t.Errorf("%q has no name", e)
		}
	}
	if _, ok := ReactionName("🦄"); ok {
		t.Error("an emoji outside the table was accepted")
	}
	if _, ok := ReactionName(":eyes:"); ok {
		t.Error("a Slack name was taken for an emoji")
	}
	if len(Reactions()) < 10 {
		t.Error("the list of reactions is short")
	}
}

func TestTheNamesOfTheMarksAreTheOnesSlackKnows(t *testing.T) {
	want := "100 cry eyes fire handshake heart hourglass_flowing_sand inbox_tray incoming_envelope ok ok_hand octagonal_sign pray rocket scream see_no_evil " +
		"tada thinking_face thumbsdown thumbsup warning white_check_mark x zap clap"
	got := strings.Join(slackid.ReactionNames(), " ")
	w := strings.Fields(want)
	sort.Strings(w)
	if got != strings.Join(w, " ") {
		t.Fatalf("the table of marks changed:\n got  %s\n want %s", got, strings.Join(w, " "))
	}
}
