// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"io"
	"log/slog"
	"os"
)

// NewLogger builds the process-wide structured logger from config-derived
// format ("json" or "text") and level ("debug", "info", "warn", "error"),
// writing to stdout. Unrecognized values fall back to json/info rather
// than failing — internal/config is responsible for rejecting them ahead
// of time.
func NewLogger(format, level string) *slog.Logger {
	return NewLoggerWriter(format, level, os.Stdout)
}

// NewLoggerWriter is NewLogger with an explicit writer, for tests.
func NewLoggerWriter(format, level string, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var handler slog.Handler
	switch format {
	case "text":
		handler = slog.NewTextHandler(w, opts)
	default:
		handler = slog.NewJSONHandler(w, opts)
	}
	return slog.New(handler)
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
