// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewLoggerWriterJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLoggerWriter("json", "info", &buf)
	logger.Info("hello", "key", "value")

	out := buf.String()
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("json handler output does not look like JSON: %q", out)
	}
	if !strings.Contains(out, `"msg":"hello"`) {
		t.Errorf("output missing msg field: %q", out)
	}
}

func TestNewLoggerWriterTextFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLoggerWriter("text", "info", &buf)
	logger.Info("hello", "key", "value")

	out := buf.String()
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("text handler output looks like JSON: %q", out)
	}
	if !strings.Contains(out, "msg=hello") {
		t.Errorf("output missing msg=hello: %q", out)
	}
}

func TestNewLoggerWriterLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLoggerWriter("json", "warn", &buf)
	logger.Info("should be suppressed")
	logger.Warn("should appear")

	out := buf.String()
	if strings.Contains(out, "should be suppressed") {
		t.Errorf("info message not suppressed at warn level: %q", out)
	}
	if !strings.Contains(out, "should appear") {
		t.Errorf("warn message missing: %q", out)
	}
}

func TestNewLoggerWriterUnknownFormatFallsBackToJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLoggerWriter("yaml", "info", &buf)
	logger.Info("hello")

	out := buf.String()
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("unknown format did not fall back to JSON: %q", out)
	}
}
