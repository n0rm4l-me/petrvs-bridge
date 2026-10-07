package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestTheHubLogUsesTheFieldNamesOfCloudLogging(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: cloudLoggingAttr}))
	log.Debug("d")
	log.Info("i", "name", "laptop")
	log.Warn("w")
	log.Error("e", "err", "boom")
	log.Log(context.Background(), slog.LevelError+4, "worse") // a level above ERROR

	want := []struct{ severity, message string }{{"DEBUG", "d"}, {"INFO", "i"}, {"WARNING", "w"}, {"ERROR", "e"}, {"ERROR", "worse"}}
	lines := logLines(t, &buf)
	if len(lines) != len(want) {
		t.Fatalf("%d lines: %s", len(lines), buf.String())
	}
	for i, w := range want {
		m := lines[i]
		if m["severity"] != w.severity || m["message"] != w.message {
			t.Errorf("line %d: %v, want severity %s and message %s", i, m, w.severity, w.message)
		}
		if _, ok := m["level"]; ok {
			t.Errorf("line %d still has \"level\": %v", i, m)
		}
		if _, ok := m["msg"]; ok {
			t.Errorf("line %d still has \"msg\": %v", i, m)
		}
		if _, ok := m["time"]; !ok {
			t.Errorf("line %d lost its time: %v", i, m)
		}
	}
	if lines[1]["name"] != "laptop" || lines[3]["err"] != "boom" {
		t.Errorf("other fields changed: %v %v", lines[1], lines[3])
	}
}

// The hub's own logger, as main builds it.
func TestTheHubsLoggerWritesSeverityAndMessageAtTheConfiguredLevel(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(&buf, "warn")
	log.Info("quiet")
	log.Error("cannot connect to Pub/Sub", "err", "boom")
	lines := logLines(t, &buf)
	if len(lines) != 1 || lines[0]["severity"] != "ERROR" || lines[0]["message"] != "cannot connect to Pub/Sub" || lines[0]["level"] != nil || lines[0]["msg"] != nil {
		t.Fatalf("%s", buf.String())
	}
}

func TestOnlyTheTopLevelLevelAndMessageAreRenamed(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: cloudLoggingAttr}))
	log.Info("hello", slog.Group("peer", slog.String("level", "high"), slog.String("msg", "mine")))
	m := logLines(t, &buf)[0]
	peer, _ := m["peer"].(map[string]any)
	if m["severity"] != "INFO" || m["message"] != "hello" || peer["level"] != "high" || peer["msg"] != "mine" {
		t.Fatalf("%v", m)
	}
}

func TestWithoutTheFunctionTheLogUsesLevelAndMsg(t *testing.T) {
	// The reason for the function: this is what slog writes by itself, and what Cloud Logging does not understand.
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Error("e")
	m := logLines(t, &buf)[0]
	if m["level"] != "ERROR" || m["msg"] != "e" || m["severity"] != nil || m["message"] != nil {
		t.Fatalf("%v", m)
	}
}
