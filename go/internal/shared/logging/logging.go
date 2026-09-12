// Package logging builds the structured JSON logger the servers use.
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// New returns a JSON slog logger writing to w at the named level
// (debug, info, warn, error; default info).
func New(w io.Writer, level string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		l = slog.LevelDebug
	case "warn", "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))
}
