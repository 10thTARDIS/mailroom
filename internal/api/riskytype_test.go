// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/base64"
	"encoding/binary"
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
