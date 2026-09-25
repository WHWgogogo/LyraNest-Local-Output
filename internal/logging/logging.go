// Package logging provides the small structured logger used by the shell.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a text slog.Logger writing to stderr, which is the container log
// stream and stays readable in `docker logs lyranest-local-output`.
func New(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler)
}
