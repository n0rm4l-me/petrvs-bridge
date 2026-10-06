package alarm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// InputSensor fires when the time since the last keyboard, mouse or trackpad input goes down,
// which means input happened since the previous check.
type InputSensor struct {
	Idle func(ctx context.Context) (time.Duration, error)
	prev time.Duration
	have bool
}

func (s *InputSensor) Name() string { return "input" }

// Reset forgets the baseline.
func (s *InputSensor) Reset() { s.have = false }

func (s *InputSensor) Check(ctx context.Context) (Event, bool, error) {
	idle, err := s.Idle(ctx)
	if err != nil {
		return Event{}, false, err
	}
	hit := s.have && idle < s.prev
	s.prev, s.have = idle, true
	if hit {
		return Event{Sensor: "input", Detail: "keyboard, mouse or trackpad was used"}, true, nil
	}
	return Event{}, false, nil
}

// ChangeSensor fires when a watched value changes.
type ChangeSensor struct {
	SensorName string
	What       string
	Read       func(ctx context.Context) (string, error)
	// Secret hides the values in the report (for example a device list).
	Secret bool
	prev   string
	have   bool
}

func (s *ChangeSensor) Name() string { return s.SensorName }

// Reset forgets the baseline.
func (s *ChangeSensor) Reset() { s.have = false }

func (s *ChangeSensor) Check(ctx context.Context) (Event, bool, error) {
	v, err := s.Read(ctx)
	if err != nil {
		return Event{}, false, err
	}
	hit := s.have && v != s.prev
	old := s.prev
	s.prev, s.have = v, true
	if !hit {
		return Event{}, false, nil
	}
	if s.Secret {
		return Event{Sensor: s.SensorName, Detail: s.What + " changed"}, true, nil
	}
	return Event{Sensor: s.SensorName, Detail: fmt.Sprintf("%s changed: %s -> %s", s.What, old, v)}, true, nil
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}

var hidIdle = regexp.MustCompile(`"HIDIdleTime"\s*=\s*(\d+)`)

// MacIdle reads the time since the last input from the HID system.
func MacIdle(ctx context.Context) (time.Duration, error) {
	out, err := run(ctx, "ioreg", "-c", "IOHIDSystem")
	if err != nil {
		return 0, err
	}
	return parseHIDIdle(out)
}

func parseHIDIdle(out string) (time.Duration, error) {
	m := hidIdle.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("HIDIdleTime not found")
	}
	ns, err := strconv.ParseUint(m[1], 10, 63)
	if err != nil {
		return 0, err
	}
	return time.Duration(ns), nil
}

// LinuxIdle reads the idle time with xprintidle (X11).
func LinuxIdle(ctx context.Context) (time.Duration, error) {
	out, err := run(ctx, "xprintidle")
	if err != nil {
		return 0, err
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(ms) * time.Millisecond, nil
}

var clamshell = regexp.MustCompile(`"AppleClamshellState"\s*=\s*(Yes|No)`)

// MacLid reports "closed" or "open".
func MacLid(ctx context.Context) (string, error) {
	out, err := run(ctx, "ioreg", "-r", "-k", "AppleClamshellState", "-d", "1")
	if err != nil {
		return "", err
	}
	return parseLid(out)
}

func parseLid(out string) (string, error) {
	m := clamshell.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("lid state not found")
	}
	if m[1] == "Yes" {
		return "closed", nil
	}
	return "open", nil
}

var powerSource = regexp.MustCompile(`'([^']+)'`)

// MacPower reports the current power source.
func MacPower(ctx context.Context) (string, error) {
	out, err := run(ctx, "pmset", "-g", "ps")
	if err != nil {
		return "", err
	}
	return parsePower(out)
}

func parsePower(out string) (string, error) {
	m := powerSource.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("power source not found")
	}
	return m[1], nil
}

// MacUSB returns a digest of the attached USB devices, so a change is seen without naming the devices.
func MacUSB(ctx context.Context) (string, error) {
	out, err := run(ctx, "ioreg", "-p", "IOUSB", "-w0")
	if err != nil {
		return "", err
	}
	return parseUSB(out), nil
}

func parseUSB(out string) string {
	var names []string
	for _, l := range strings.Split(out, "\n") {
		if i := strings.Index(l, "+-o "); i >= 0 {
			names = append(names, strings.TrimSpace(l[i+4:]))
		}
	}
	sort.Strings(names)
	return digest(strings.Join(names, "\n"))
}

// LinuxUSB returns a digest of lsusb output.
func LinuxUSB(ctx context.Context) (string, error) {
	out, err := run(ctx, "lsusb")
	if err != nil {
		return "", err
	}
	return digest(out), nil
}

func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:8]) }

// DefaultSensors returns the sensors that make sense on this operating system.
func DefaultSensors() []Sensor {
	switch runtime.GOOS {
	case "darwin":
		return []Sensor{
			&InputSensor{Idle: MacIdle},
			&ChangeSensor{SensorName: "lid", What: "lid", Read: MacLid},
			&ChangeSensor{SensorName: "power", What: "power source", Read: MacPower},
			&ChangeSensor{SensorName: "usb", What: "USB devices", Read: MacUSB, Secret: true},
		}
	case "linux":
		return []Sensor{
			&InputSensor{Idle: LinuxIdle},
			&ChangeSensor{SensorName: "usb", What: "USB devices", Read: LinuxUSB, Secret: true},
		}
	}
	return nil
}
