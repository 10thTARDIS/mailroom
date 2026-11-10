// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
)

func riskyFixture() string {
	pe := make([]byte, 0x84)
	pe[0], pe[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(pe[0x3c:], 0x80)
	copy(pe[0x80:], "PE\x00\x00")
	att := func(name, ctype string, data []byte) string {
		return "--bnd\r\nContent-Type: " + ctype + "; name=\"" + name + "\"\r\n" +
			"Content-Disposition: attachment; filename=\"" + name + "\"\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString(data) + "\r\n"
	}
	return "From: a@example.com\r\nSubject: files\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=bnd\r\n\r\n" +
		"--bnd\r\nContent-Type: text/plain\r\n\r\n#!/bin/sh\r\nthis body is only text\r\n" +
		att("invoice.pdf.exe", "application/octet-stream", []byte("hello")) +
		att("setup.pdf", "application/pdf", pe) +
		att("report.pdf", "application/pdf", []byte("%PDF-1.4 hello")) +
		att("macro.xlsm", "application/octet-stream", []byte("PK")) +
		"--bnd--\r\n"
}

// The API field: every attachment carries riskyType, decided by name AND bytes,
// and a plain-text body that starts with "#!" is not a script.
func TestRiskyTypeOnBodyParts(t *testing.T) {
	raw := riskyFixture()
	body := buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{})
	got := map[string]any{}
	for _, a := range body.attachment {
		got[a["name"].(string)] = a["riskyType"]
		if _, ok := a["riskyType"]; !ok {
			t.Errorf("%v has no riskyType key at all (null must be explicit)", a["name"])
		}
	}
	want := map[string]any{
		"invoice.pdf.exe": "disguised",
		"setup.pdf":       "disguised", // a PE named .pdf: only the sniff catches it
		"report.pdf":      nil,
		"macro.xlsm":      "macro_document",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: riskyType = %v, want %v", name, got[name], w)
		}
	}
	for _, tb := range body.textBody {
		if tb["riskyType"] != nil {
			t.Errorf("the text body part is marked risky: %v", tb["riskyType"])
		}
	}
}

// attachRisk builds a message with ONE attachment whose Content-Disposition
// parameters are exactly `params` (so a test can use RFC 2231 and RFC 2047 forms)
// and returns its riskyType and the number of attachments found.
func attachRisk(t *testing.T, dispParams string) (any, int) {
	t.Helper()
	raw := "From: a@example.com\r\nSubject: f\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=bnd\r\n\r\n" +
		"--bnd\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--bnd\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; " + dispParams + "\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte("hello")) + "\r\n" +
		"--bnd--\r\n"
	body := buildEmailBody([]byte(raw), hashOf(raw), bodyArgs{})
	if len(body.attachment) != 1 {
		return nil, len(body.attachment)
	}
	return body.attachment[0]["riskyType"], 1
}

// ⚠ THE CLASSIFIED NAME MUST BE THE SAVED NAME (security review HIGH-1, HIGH-2).
// Each case is a name that reads as a document to a classifier which stops at a
// NUL or counts only a prefix, and as a program once sanitizeFilename has made it
// the name on disk. They go through the real MIME parse.
func TestRiskyTypeJudgesTheNameTheServerSends(t *testing.T) {
	for _, tc := range []struct {
		name, params string
		want         any
	}{
		{"RFC 2231 percent-encoded NUL", `filename*=utf-8''invoice.pdf%00.exe`, "disguised"},
		{"RFC 2047 encoded NUL", `filename="=?utf-8?q?report.pdf=00.exe?="`, "disguised"},
		{"long name, extension past the cut", `filename="` + strings.Repeat("a", 96) + `.exe.pdf"`, nil},
		{"long name, risky extension kept", `filename="` + strings.Repeat("a", 150) + `.exe"`, "program"},
		{"long s in the extension", "filename*=utf-8''setup.%C5%BFcr", "program"},
		// The control: an ordinary long document is NOT risky, so the cases above
		// are red because of the NUL / the cut, not because long names always are.
		{"ordinary long pdf", `filename="` + strings.Repeat("a", 150) + `.pdf"`, nil},
		{"ordinary pdf", `filename="report.pdf"`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n := attachRisk(t, tc.params)
			if n != 1 {
				t.Fatalf("parsed %d attachments, want 1: the fixture proves nothing", n)
			}
			if tc.name == "long name, extension past the cut" {
				// 96 a's + ".exe.pdf" is cut to 96 a's + ".pdf" ONLY if the
				// extension is kept: saved as .pdf it is a document, and the
				// name judged is the name saved. Before the fix it was saved as
				// 96 a's + ".exe" while judged as .pdf.
				saved := sanitizeFilename(strings.Repeat("a", 96) + ".exe.pdf")
				if !strings.HasSuffix(saved, ".pdf") {
					t.Fatalf("saved name %q lost its extension", saved)
				}
			}
			if got != tc.want {
				t.Errorf("riskyType = %v, want %v", got, tc.want)
			}
		})
	}
}

// The NUL cases must be red for the right reason: the raw name, with its NUL
// removed, is an executable, and the file is saved under that name.
func TestSanitizedNameOfNulCasesIsTheRiskyOne(t *testing.T) {
	if got := sanitizeFilename("invoice.pdf\x00.exe"); got != "invoice.pdf.exe" {
		t.Errorf("sanitizeFilename = %q, want invoice.pdf.exe", got)
	}
}

func TestSanitizeFilenameKeepsTheExtensionWhenTruncating(t *testing.T) {
	for _, tc := range []struct {
		in, wantSuffix string
		wantLen        int
	}{
		{strings.Repeat("a", 96) + ".exe.pdf", ".pdf", 100},
		{strings.Repeat("a", 300) + ".exe", ".exe", 100},
		{strings.Repeat("é", 200) + ".docx", ".docx", 100},
		// An "extension" longer than 16 runes is not kept: the plain cut applies,
		// and the total is still bounded.
		{"a." + strings.Repeat("b", 200), "", 100},
		{"short.pdf", ".pdf", 9},
	} {
		got := sanitizeFilename(tc.in)
		if n := len([]rune(got)); n > maxFilenameRunes || n != tc.wantLen {
			t.Errorf("len(%q...) = %d, want %d", got[:min(len(got), 20)], n, tc.wantLen)
		}
		if !strings.HasSuffix(got, tc.wantSuffix) {
			t.Errorf("sanitizeFilename(%.30q...) = ...%q, want suffix %q", tc.in, got[max(0, len(got)-12):], tc.wantSuffix)
		}
	}
}
