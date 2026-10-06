package alarm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

type fake struct {
	name   string
	mu     sync.Mutex
	hit    bool
	err    error
	resets int
}

func (f *fake) Name() string { return f.name }
func (f *fake) Reset()       { f.mu.Lock(); f.resets++; f.mu.Unlock() }
func (f *fake) Check(context.Context) (Event, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Event{}, false, f.err
	}
	return Event{Sensor: f.name, Detail: f.name + " touched"}, f.hit, nil
}
func (f *fake) set(hit bool, err error) { f.mu.Lock(); f.hit, f.err = hit, err; f.mu.Unlock() }

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type seen struct {
	e          Event
	suppressed int
}

func rig(t *testing.T, sensors ...Sensor) (*Alarm, *[]seen, *clock, string) {
	t.Helper()
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	state := filepath.Join(t.TempDir(), "alarm.state")
	var got []seen
	a, err := New(Config{Sensors: sensors, Cooldown: time.Minute, StateFile: state, Now: c.now,
		Notify: func(_ context.Context, e Event, n int) { got = append(got, seen{e, n}) }})
	if err != nil {
		t.Fatal(err)
	}
	return a, &got, c, state
}

func TestDisarmedAlarmIsSilent(t *testing.T) {
	s := &fake{name: "input", hit: true}
	a, got, _, _ := rig(t, s)
	a.Tick(context.Background())
	if len(*got) != 0 {
		t.Fatal("a disarmed alarm reported something")
	}
}

func TestArmedAlarmReportsAtOnce(t *testing.T) {
	s := &fake{name: "input"}
	a, got, _, _ := rig(t, s)
	a.Arm()
	a.Tick(context.Background())
	s.set(true, nil)
	a.Tick(context.Background())
	if len(*got) != 1 || (*got)[0].e.Sensor != "input" || (*got)[0].suppressed != 0 {
		t.Fatalf("%+v", *got)
	}
}

func TestCooldownHoldsBackAndCounts(t *testing.T) {
	s := &fake{name: "input", hit: true}
	a, got, c, _ := rig(t, s)
	a.Arm()
	for i := 0; i < 5; i++ {
		a.Tick(context.Background())
	}
	if len(*got) != 1 {
		t.Fatalf("%d reports inside the cooldown", len(*got))
	}
	c.t = c.t.Add(61 * time.Second)
	a.Tick(context.Background())
	if len(*got) != 2 || (*got)[1].suppressed != 4 {
		t.Fatalf("%+v", *got)
	}
	a.Tick(context.Background())
	if len(*got) != 2 {
		t.Fatal("the count was not reset")
	}
}

func TestCooldownIsPerSensor(t *testing.T) {
	in, lid := &fake{name: "input", hit: true}, &fake{name: "lid", hit: true}
	a, got, _, _ := rig(t, in, lid)
	a.Arm()
	a.Tick(context.Background())
	a.Tick(context.Background())
	if len(*got) != 2 {
		t.Fatalf("expected one report per sensor, got %d", len(*got))
	}
}

func TestArmingTakesTheBaselineOnlyWhenItWasOff(t *testing.T) {
	s := &fake{name: "input"}
	a, _, _, _ := rig(t, s)
	a.Arm()
	a.Arm() // a repeated "on" must not hide a touch that happened just before it
	if s.resets != 1 {
		t.Fatalf("resets = %d, want 1: arming an armed alarm retook the baseline", s.resets)
	}
	a.Disarm()
	a.Arm()
	if s.resets != 2 {
		t.Fatalf("resets = %d, want 2 after disarm and arm", s.resets)
	}
}

func TestDisarmStopsReports(t *testing.T) {
	s := &fake{name: "input", hit: true}
	a, got, c, state := rig(t, s)
	a.Arm()
	a.Tick(context.Background())
	a.Disarm()
	if a.Armed() {
		t.Fatal("still armed")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("the state file remains")
	}
	c.t = c.t.Add(time.Hour)
	a.Tick(context.Background())
	if len(*got) != 1 {
		t.Fatal("a disarmed alarm reported")
	}
	if err := a.Disarm(); err != nil {
		t.Fatalf("disarming twice: %v", err)
	}
}

func TestArmedStateSurvivesARestart(t *testing.T) {
	s := &fake{name: "input"}
	a, _, _, state := rig(t, s)
	a.Arm()
	b, err := New(Config{Sensors: []Sensor{s}, StateFile: state})
	if err != nil {
		t.Fatal(err)
	}
	if !b.Armed() {
		t.Fatal("the armed state was lost")
	}
	a.Disarm()
	c, _ := New(Config{Sensors: []Sensor{s}, StateFile: state})
	if c.Armed() {
		t.Fatal("a disarmed alarm came back armed")
	}
	if st, err := os.Stat(state); err == nil {
		t.Fatalf("state file present with mode %v", st.Mode())
	}
}

func TestStateFileIsPrivate(t *testing.T) {
	s := &fake{name: "input"}
	a, _, _, state := rig(t, s)
	a.Arm()
	st, _ := os.Stat(state)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}

func TestBrokenSensorIsReportedAsBlindNotAsAnEvent(t *testing.T) {
	s := &fake{name: "input", err: errors.New("ioreg missing")}
	a, got, _, _ := rig(t, s)
	a.Arm()
	a.Tick(context.Background())
	if len(*got) != 0 {
		t.Fatal("a failing sensor produced an event")
	}
	if !reflect.DeepEqual(a.Blind(), []string{"input"}) {
		t.Fatalf("blind = %v", a.Blind())
	}
	s.set(false, nil)
	a.Tick(context.Background())
	if len(a.Blind()) != 0 {
		t.Fatal("the sensor stayed blind after recovering")
	}
}

func TestNewNeedsASensor(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("an alarm without sensors was created")
	}
}

func TestStatusMentionsStateAndBlindness(t *testing.T) {
	s := &fake{name: "input", err: errors.New("x")}
	a, _, _, _ := rig(t, s)
	if got := a.Status(); got != "alarm disarmed, watching: [input]" {
		t.Fatal(got)
	}
	a.Arm()
	a.Tick(context.Background())
	if got := a.Status(); got != "alarm armed, watching: [input] (cannot read: [input])" {
		t.Fatal(got)
	}
}

func TestRunStopsWithTheContext(t *testing.T) {
	s := &fake{name: "input", hit: true}
	a, got, _, _ := rig(t, s)
	a.cfg.Poll = 5 * time.Millisecond
	a.Arm()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
	if len(*got) == 0 {
		t.Fatal("Run never polled")
	}
}

// ---- sensors ----

func TestInputSensorFiresOnlyWhenIdleTimeGoesDown(t *testing.T) {
	seq := []time.Duration{10 * time.Second, 11 * time.Second, 12 * time.Second, 300 * time.Millisecond, 1300 * time.Millisecond, 50 * time.Millisecond}
	i := 0
	s := &InputSensor{Idle: func(context.Context) (time.Duration, error) { d := seq[i]; i++; return d, nil }}
	var hits []int
	for n := range seq {
		if _, hit, _ := s.Check(context.Background()); hit {
			hits = append(hits, n)
		}
	}
	if !reflect.DeepEqual(hits, []int{3, 5}) {
		t.Fatalf("hits at %v, want [3 5]", hits)
	}
}

func TestInputSensorFirstReadingIsOnlyABaseline(t *testing.T) {
	s := &InputSensor{Idle: func(context.Context) (time.Duration, error) { return 0, nil }}
	if _, hit, _ := s.Check(context.Background()); hit {
		t.Fatal("the first reading fired")
	}
	s.Reset()
	if _, hit, _ := s.Check(context.Background()); hit {
		t.Fatal("the first reading after a reset fired")
	}
}

func TestInputSensorPropagatesErrors(t *testing.T) {
	s := &InputSensor{Idle: func(context.Context) (time.Duration, error) { return 0, errors.New("nope") }}
	if _, _, err := s.Check(context.Background()); err == nil {
		t.Fatal("error swallowed")
	}
}

func TestChangeSensor(t *testing.T) {
	vals := []string{"open", "open", "closed", "closed", "open"}
	i := 0
	s := &ChangeSensor{SensorName: "lid", What: "lid", Read: func(context.Context) (string, error) { v := vals[i]; i++; return v, nil }}
	var details []string
	for range vals {
		if e, hit, _ := s.Check(context.Background()); hit {
			details = append(details, e.Detail)
		}
	}
	want := []string{"lid changed: open -> closed", "lid changed: closed -> open"}
	if !reflect.DeepEqual(details, want) {
		t.Fatalf("%v", details)
	}
}

func TestSecretChangeSensorDoesNotRevealValues(t *testing.T) {
	vals := []string{"aaaa", "bbbb"}
	i := 0
	s := &ChangeSensor{SensorName: "usb", What: "USB devices", Secret: true, Read: func(context.Context) (string, error) { v := vals[i]; i++; return v, nil }}
	s.Check(context.Background())
	e, hit, _ := s.Check(context.Background())
	if !hit || e.Detail != "USB devices changed" {
		t.Fatalf("%+v", e)
	}
}

func TestParsersOnRealOutputShapes(t *testing.T) {
	d, err := parseHIDIdle(`    | |   "HIDIdleTime" = 4293750625` + "\n")
	if err != nil || d != 4293750625*time.Nanosecond {
		t.Fatalf("%v %v", d, err)
	}
	if _, err := parseHIDIdle("nothing here"); err == nil {
		t.Fatal("garbage parsed")
	}
	if _, err := parseHIDIdle(`"HIDIdleTime" = 99999999999999999999999`); err == nil {
		t.Fatal("an overflowing value parsed")
	}
	for in, want := range map[string]string{`"AppleClamshellState" = Yes`: "closed", `"AppleClamshellState" = No`: "open"} {
		if got, err := parseLid(in); err != nil || got != want {
			t.Fatalf("%q: %q %v", in, got, err)
		}
	}
	if _, err := parseLid("x"); err == nil {
		t.Fatal("garbage parsed")
	}
	if got, err := parsePower("Now drawing from 'AC Power'\n -InternalBattery-0 (id=1)\t100%; charged"); err != nil || got != "AC Power" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := parsePower("x"); err == nil {
		t.Fatal("garbage parsed")
	}
	a := parseUSB("+-o Root  <class IORegistryEntry>\n  +-o Keyboard@1\n  +-o Mouse@2\n")
	b := parseUSB("+-o Root  <class IORegistryEntry>\n  +-o Mouse@2\n  +-o Keyboard@1\n")
	c := parseUSB("+-o Root  <class IORegistryEntry>\n  +-o Mouse@2\n")
	if a != b || a == c {
		t.Fatal("USB digest must ignore order and notice a removed device")
	}
}

func TestDefaultSensorsExistOnThisPlatform(t *testing.T) {
	// The real readers are exercised on the machine running the tests; a missing tool makes a sensor blind, not a test failure.
	for _, s := range DefaultSensors() {
		if s.Name() == "" {
			t.Fatal("unnamed sensor")
		}
	}
}
