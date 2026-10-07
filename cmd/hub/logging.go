package main

import (
	"io"
	"log/slog"
)

// newLogger is the hub's logger: JSON lines in the field names of Cloud Logging, at the configured level.
func newLogger(w io.Writer, lvl string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level(lvl), ReplaceAttr: cloudLoggingAttr}))
}

// cloudLoggingAttr is a slog ReplaceAttr that renames the two fields of the JSON log that Cloud Logging (GKE) reads from a
// container's standard output: "severity" and "message". slog writes "level" and "msg", so without this every line is
// INFO there and an alert on severity >= ERROR never fires. Only the hub does this: the agent runs on laptops, where the
// plain names are easier to read. The values of other fields are left as they are.
func cloudLoggingAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a // an attribute of a group that happens to be called "level" or "msg" is the caller's own
	}
	switch a.Key {
	case slog.LevelKey:
		if l, ok := a.Value.Any().(slog.Level); ok {
			return slog.String("severity", cloudSeverity(l))
		}
	case slog.MessageKey:
		return slog.String("message", a.Value.String())
	}
	return a
}

// cloudSeverity maps a slog level to the name of a Cloud Logging severity.
func cloudSeverity(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARNING"
	case l >= slog.LevelInfo:
		return "INFO"
	}
	return "DEBUG"
}
