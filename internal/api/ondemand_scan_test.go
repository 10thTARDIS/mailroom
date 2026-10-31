// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/clamd"
	"mailroom/internal/mime"
)

type sigScanner struct{ v clamd.Verdict }

func (s sigScanner) Scan(context.Context, []byte) (clamd.Verdict, error) { return s.v, nil }

const onDemandSynth = "From: a@example.com\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"o\"\r\n\r\n" +
	"--o\r\nContent-Type: text/plain\r\n\r\nhello\r\n" +
	"--o\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"secret-name.pdf\"\r\n\r\n%PDF-1.4 x\r\n" +
	"--o--\r\n"

func onDemandPart(t *testing.T) (*mime.Message, *mime.Structure, string) {
	t.Helper()
	parsed, err := mime.ParseBytes([]byte(onDemandSynth))
	if err != nil || parsed == nil {
		t.Fatal(err)
	}
	for n, p := range mime.PartPaths(parsed.Structure) {
		if n.ContentType == "application/pdf" {
			return parsed, n, p
		}
	}
	t.Fatal("no pdf part")
	return nil, nil, ""
}

// An on-demand verdict is observed (the metric) and logged (one INFO line with
// message id, part path, verdict, detection name for flagged, size; never the
// filename) — #47, so it can be confirmed without sending malware.
func TestOnDemandVerdictIsLoggedAndObserved(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	var got []string
	parsed, st, path := onDemandPart(t)
	msgID := ulid.Make()
	cases := []struct {
		v        clamd.Verdict
		wantSig  bool
		wantWord string
	}{
		{clamd.Verdict{State: clamd.Flagged, Signature: "Eicar-Test-Signature"}, true, "flagged"},
		{clamd.Verdict{State: clamd.Clean}, false, "clean"},
	}
	for _, c := range cases {
		buf.Reset()
		got = nil
		a := &API{
			onDemandScan:    sigScanner{c.v},
			onDemandObserve: func(v string) { mu.Lock(); got = append(got, v); mu.Unlock() },
		}
		status, sig := a.decryptedPartVerdict(context.Background(), ulid.ULID{}, msgID, parsed, st, path)
		if status != c.wantWord {
			t.Fatalf("status = %q, want %q", status, c.wantWord)
		}
		if c.wantSig && sig != "Eicar-Test-Signature" {
			t.Errorf("signature = %q", sig)
		}
		if !c.wantSig && sig != "" {
			t.Errorf("non-flagged verdict carried a signature %q", sig)
		}
		if len(got) != 1 || got[0] != c.wantWord {
			t.Errorf("observer calls = %v, want exactly [%s]", got, c.wantWord)
		}
		line := buf.String()
		for _, want := range []string{"message_id=" + msgID.String(), "part_path=" + path, "verdict=" + c.wantWord, "size_bytes="} {
			if !strings.Contains(line, want) {
				t.Errorf("log line %q lacks %q", line, want)
			}
		}
		if strings.Contains(line, "secret-name") {
			t.Errorf("log line carries the filename: %q", line)
		}
		if c.wantSig != strings.Contains(line, "signature=Eicar-Test-Signature") {
			t.Errorf("log line signature presence wrong (want %v): %q", c.wantSig, line)
		}
	}
}

// The refusal names the verdict and, for flagged, the detection (the web client
// shows both in-app instead of navigating to a bare 403).
func TestBlobQuarantinedCarriesTheDetectionName(t *testing.T) {
	rec := httptest.NewRecorder()
	blobQuarantined(rec, nil, "flagged", "Eicar-Test-Signature", false)
	if rec.Code != 403 || rec.Header().Get("X-Mailroom-Attachment-Scan") != "flagged" ||
		rec.Header().Get("X-Mailroom-Attachment-Signature") != "Eicar-Test-Signature" {
		t.Errorf("flagged: %d scan=%q sig=%q", rec.Code, rec.Header().Get("X-Mailroom-Attachment-Scan"),
			rec.Header().Get("X-Mailroom-Attachment-Signature"))
	}
	// unknown never carries a name, and a hostile one is stripped.
	rec = httptest.NewRecorder()
	blobQuarantined(rec, nil, "unknown", "Eicar", false)
	if rec.Header().Get("X-Mailroom-Attachment-Signature") != "" {
		t.Error("unknown carried a signature header")
	}
	rec = httptest.NewRecorder()
	blobQuarantined(rec, nil, "flagged", "A b\r\nX-Evil: 1é", false)
	if got := rec.Header().Get("X-Mailroom-Attachment-Signature"); got != "AbX-Evil:1" {
		t.Errorf("sanitised signature = %q", got)
	}
}
