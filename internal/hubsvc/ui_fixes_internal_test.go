package hubsvc

import (
	"testing"
	"time"
)

// A tap carries no date, so the hub keeps the time it sent each message that has buttons.
func TestButtonsAreObeyedForFifteenMinutesAfterTheHubSentThem(t *testing.T) {
	h, _ := New(base(t))
	at := time.Now()
	h.ui.noteButtons([]int64{7, 8, 0}, at)
	for name, c := range map[string]struct {
		id   int64
		now  time.Time
		want bool
	}{
		"just sent":                     {7, at, true},
		"a minute later":                {7, at.Add(time.Minute), true},
		"exactly fifteen minutes":       {7, at.Add(15 * time.Minute), true},
		"a moment after fifteen":        {7, at.Add(15*time.Minute + time.Nanosecond), false},
		"a day later":                   {8, at.Add(24 * time.Hour), false},
		"a message never sent":          {9, at, false},
		"no message (a tap names none)": {0, at, false},
		"the clock went back":           {7, at.Add(-time.Hour), true},
	} {
		if got := h.ui.fresh(c.id, c.now); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func TestTheRecordOfSentButtonsIsBounded(t *testing.T) {
	h, _ := New(base(t))
	at := time.Now()
	for i := int64(1); i <= 3*maxTrackedMessages; i++ {
		h.ui.noteButtons([]int64{i}, at)
	}
	if len(h.ui.sent) > maxTrackedMessages || len(h.ui.sentRing) > maxTrackedMessages {
		t.Fatalf("the record grew to %d", len(h.ui.sent))
	}
	if h.ui.fresh(1, at) || !h.ui.fresh(3*maxTrackedMessages, at) {
		t.Fatal("the oldest should be forgotten and the newest kept")
	}
	// the same message noted twice is one entry, with the later time
	h.ui.noteButtons([]int64{5000, 5000}, at)
	h.ui.noteButtons([]int64{5000}, at.Add(time.Hour))
	if n := len(h.ui.sentRing); n > maxTrackedMessages || !h.ui.fresh(5000, at.Add(time.Hour+14*time.Minute)) {
		t.Fatalf("ring %d", n)
	}
}

func TestReleaseGivesTheButtonsBackToTheMessage(t *testing.T) {
	h, _ := New(base(t))
	if !h.ui.claim(7) || h.ui.claim(7) {
		t.Fatal("claim")
	}
	h.ui.release(7)
	if !h.ui.claim(7) {
		t.Fatal("the buttons were not given back")
	}
	h.ui.release(0)
	h.ui.release(99) // never claimed: nothing to give back
	if h.ui.claim(7) {
		t.Fatal("a message that was claimed again is claimed")
	}
	if len(h.ui.ring) != 1 {
		t.Fatalf("ring %v", h.ui.ring)
	}
}
