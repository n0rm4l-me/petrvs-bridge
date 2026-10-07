package e2e

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/approval"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

func alarmRig(t *testing.T, o testrig.Options) (*testrig.Rig, *testrig.AgentRig) {
	t.Helper()
	o.Alarm = true
	r := testrig.New(t, o)
	r.StartAll()
	return r, r.Agents["laptop"]
}

func alerts(r *testrig.Rig) int {
	n := 0
	for _, s := range r.SentTexts() {
		if strings.Contains(s, "ALARM") {
			n++
		}
	}
	return n
}

func TestADisarmedAlarmStaysSilent(t *testing.T) {
	r, a := alarmRig(t, testrig.Options{})
	a.Sensor.Touch()
	r.Never("an alert from a disarmed alarm", 400*time.Millisecond, func() bool { return alerts(r) > 0 })
}

func TestArmedAlarmReportsATouchWithATime(t *testing.T) {
	r, a := alarmRig(t, testrig.Options{})
	r.Say("/alarm on")
	r.WaitSent("Alarm armed")
	r.Eventually("armed", func() bool { return a.Alarm.Armed() })
	time.Sleep(100 * time.Millisecond) // the baseline is taken on arming
	a.Sensor.Touch()
	r.WaitSent("ALARM")
	var alert string
	for _, s := range r.SentTexts() {
		if strings.Contains(s, "ALARM") {
			alert = s
		}
	}
	year := time.Now().Format("2006-")
	if !strings.Contains(alert, "keyboard, mouse or trackpad was used") || !strings.Contains(alert, year) {
		t.Fatalf("alert = %q", alert)
	}
}

func TestRepeatedTouchesAreGroupedByTheCooldown(t *testing.T) {
	r, a := alarmRig(t, testrig.Options{})
	r.Say("/alarm on")
	r.Eventually("armed", func() bool { return a.Alarm.Armed() })
	for i := 0; i < 6; i++ {
		a.Sensor.Touch()
		time.Sleep(30 * time.Millisecond)
	}
	r.Eventually("first alert", func() bool { return alerts(r) >= 1 })
	if n := alerts(r); n != 1 {
		t.Fatalf("%d alerts inside the cooldown, want 1", n)
	}
	time.Sleep(600 * time.Millisecond) // past the cooldown
	a.Sensor.Touch()
	r.Eventually("second alert with a count", func() bool { return r.Sent("more since the last report") })
}

func TestDisarmStopsAlerts(t *testing.T) {
	r, a := alarmRig(t, testrig.Options{})
	r.Say("/alarm on")
	r.Eventually("armed", func() bool { return a.Alarm.Armed() })
	r.Say("/alarm off")
	r.WaitSent("Alarm disarmed")
	if a.Alarm.Armed() {
		t.Fatal("still armed")
	}
	a.Sensor.Touch()
	r.Never("an alert after disarming", 400*time.Millisecond, func() bool { return alerts(r) > 0 })
}

func TestAlarmStatus(t *testing.T) {
	r, _ := alarmRig(t, testrig.Options{})
	r.Say("/alarm")
	r.WaitSent("alarm disarmed")
	r.Say("/alarm on")
	r.Say("/alarm status")
	r.WaitSent("alarm armed, watching")
	r.Say("/alarm sideways")
	r.WaitSent("unknown alarm command")
}

func TestAnArmedAlarmSurvivesAnAgentRestart(t *testing.T) {
	r, a := alarmRig(t, testrig.Options{})
	r.Say("/alarm on")
	r.Eventually("armed", func() bool { return a.Alarm.Armed() })
	r.StartAgent("laptop")
	r.WaitConnected("laptop")
	if !a.Alarm.Armed() {
		t.Fatal("the alarm came back disarmed")
	}
	time.Sleep(150 * time.Millisecond)
	a.Sensor.Touch()
	r.Eventually("alert after restart", func() bool { return alerts(r) == 1 })
	r.Say("/agents")
	r.WaitSent("alarm=on")
}

func TestDisarmingCanRequireAOneTimeCode(t *testing.T) {
	secret, _, _ := approval.GenerateSecret()
	r, a := alarmRig(t, testrig.Options{AlarmSecret: secret})
	r.Say("/alarm on")
	r.Eventually("armed", func() bool { return a.Alarm.Armed() })
	r.Say("/alarm off")
	r.WaitSent("one-time code is required")
	r.Say("/alarm off 000000")
	time.Sleep(200 * time.Millisecond)
	if !a.Alarm.Armed() {
		t.Fatal("a wrong code disarmed the alarm")
	}
	code := approval.Code(secret, time.Now())
	r.Say("/alarm off " + code)
	r.WaitSent("Alarm disarmed")
	if a.Alarm.Armed() {
		t.Fatal("the right code did not disarm")
	}
	// the same code cannot disarm a second time
	r.Say("/alarm on")
	r.Eventually("armed again", func() bool { return a.Alarm.Armed() })
	r.Say("/alarm off " + code)
	time.Sleep(300 * time.Millisecond)
	if !a.Alarm.Armed() {
		t.Fatal("a used code disarmed the alarm again")
	}
	time.Sleep(100 * time.Millisecond)
	a.Sensor.Touch()
	r.Eventually("still alerting", func() bool { return alerts(r) >= 1 })
}

func TestAReplayedDisarmCommandDoesNotSilenceAReArmedAlarm(t *testing.T) {
	r, a := alarmRig(t, testrig.Options{})
	r.Say("/alarm on")
	r.Eventually("armed", func() bool { return a.Alarm.Armed() })
	r.Say("/alarm off")
	r.WaitSent("Alarm disarmed")
	captured := r.Mem.Published(r.Names.ToAgent("laptop"))
	r.Say("/alarm on")
	r.Eventually("armed again", func() bool { return a.Alarm.Armed() })
	for i := 0; i < 3; i++ {
		for _, p := range captured {
			r.Mem.Inject(r.Names.AgentSub("laptop"), p.Data, p.Attrs)
		}
	}
	time.Sleep(400 * time.Millisecond)
	if !a.Alarm.Armed() {
		t.Fatal("a replayed message disarmed the alarm")
	}
}

func TestADelayedDisarmCommandExpires(t *testing.T) {
	clock := testrig.NewClock()
	r, a := alarmRig(t, testrig.Options{Clock: clock, SilenceLimit: 24 * time.Hour, OfflineAfter: 24 * time.Hour})
	r.Say("/alarm on")
	r.Eventually("armed", func() bool { return a.Alarm.Armed() })
	r.Mem.Hold(r.Names.AgentSub("laptop"))
	r.Say("/alarm off")
	r.Eventually("relayed", func() bool { return r.Hub.Stats().Relayed >= 2 })
	clock.Advance(5 * time.Minute)
	r.Mem.Release(r.Names.AgentSub("laptop"), nil)
	r.WaitSent("alarm command expired")
	if !a.Alarm.Armed() {
		t.Fatal("an expired command disarmed the alarm")
	}
}

func TestALockedMachineKeepsItsAlarmArmed(t *testing.T) {
	r, a := alarmRig(t, testrig.Options{})
	r.Say("/alarm on")
	r.Eventually("armed", func() bool { return a.Alarm.Armed() })
	r.Say("/lockdown")
	r.Eventually("locked", func() bool { return a.Agent.Locked() })
	r.Say("/unlock")
	r.Say("/alarm off")
	r.WaitSent("the alarm stays armed")
	if !a.Alarm.Armed() {
		t.Fatal("a locked machine was disarmed from the chat")
	}
}

func TestAnAlertSurvivesAHubOutage(t *testing.T) {
	r, a := alarmRig(t, testrig.Options{})
	r.Say("/alarm on")
	r.Eventually("armed", func() bool { return a.Alarm.Armed() })
	time.Sleep(100 * time.Millisecond)
	r.StopHub()
	a.Sensor.Touch()
	time.Sleep(300 * time.Millisecond)
	r.StartHub()
	r.Eventually("the alert arrives after the hub is back", func() bool { return alerts(r) == 1 })
	time.Sleep(800 * time.Millisecond)
	if n := alerts(r); n != 1 {
		t.Fatalf("the alert was shown %d times", n)
	}
}

func TestAnAlertSurvivesAChatOutage(t *testing.T) {
	r, a := alarmRig(t, testrig.Options{})
	r.Say("/alarm on")
	r.Eventually("armed", func() bool { return a.Alarm.Armed() })
	time.Sleep(100 * time.Millisecond)
	r.TG.SetSendErr(errors.New("chat down"))
	a.Sensor.Touch()
	time.Sleep(500 * time.Millisecond)
	r.TG.SetSendErr(nil)
	r.Eventually("the alert is delivered once the chat is back", func() bool { return alerts(r) == 1 })
}

func TestGoingOfflineWhileArmedIsCalledOut(t *testing.T) {
	r, a := alarmRig(t, testrig.Options{OfflineAfter: 400 * time.Millisecond})
	r.Say("/alarm on")
	r.Eventually("armed", func() bool { return a.Alarm.Armed() })
	time.Sleep(300 * time.Millisecond) // a heartbeat carrying alarm=on
	r.StopAgent("laptop")
	r.WaitSent("Its alarm was armed")
}

func TestAlarmCommandsGoToTheActiveAgentOnly(t *testing.T) {
	r := testrig.New(t, testrig.Options{Agents: []string{"laptop", "nas"}, Alarm: true})
	r.StartAll()
	r.Say("/agent nas")
	r.Say("/alarm on")
	r.Eventually("nas armed", func() bool { return r.Agents["nas"].Alarm.Armed() })
	time.Sleep(200 * time.Millisecond)
	if r.Agents["laptop"].Alarm.Armed() {
		t.Fatal("the other agent was armed too")
	}
	time.Sleep(100 * time.Millisecond)
	r.Agents["nas"].Sensor.Touch()
	r.WaitSent("[nas]")
}

func TestAnAgentWithoutAnAlarmSaysSo(t *testing.T) {
	r, _ := one(t, testrig.Options{})
	r.Say("/alarm on")
	r.WaitSent("no alarm configured")
}

func TestAlertCannotBeForgedIntoTheChat(t *testing.T) {
	r, _ := alarmRig(t, testrig.Options{})
	for i := 0; i < 20; i++ {
		r.Mem.Inject(r.Names.HubSub(), junk(300), wire.Attrs(wire.KindMsg, "laptop"))
	}
	r.Never("a forged alert reaching the chat", 500*time.Millisecond, func() bool { return alerts(r) > 0 })
}
