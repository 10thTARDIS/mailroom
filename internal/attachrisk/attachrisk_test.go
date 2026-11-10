// SPDX-License-Identifier: AGPL-3.0-only

package attachrisk

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// pe builds a minimal DOS stub whose e_lfanew points at a PE signature.
func pe() []byte {
	b := make([]byte, 0x84)
	b[0], b[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(b[0x3c:], 0x80)
	copy(b[0x80:], "PE\x00\x00")
	return b
}

func TestClassifyByExtension(t *testing.T) {
	cases := map[string]string{
		"setup.exe": Program, "SETUP.EXE": Program, "run.bat": Script, "a.ps1": Script,
		"x.vbs": Script, "x.js": Script, "x.hta": Script, "app.msi": Installer, "a.dmg": Installer,
		"a.deb": Installer, "m.docm": MacroDocument, "m.xlsm": MacroDocument, "m.pptm": MacroDocument,
		"go.lnk": Shortcut, "go.url": Shortcut, "disc.iso": DiskImage, "d.img": DiskImage,
		"tool.jar": Program, "a.apk": Program,
	}
	for name, want := range cases {
		if got := ClassifyName(name, "application/octet-stream"); !got.Risky || got.Reason != want {
			t.Errorf("%s: got %+v, want risky %s", name, got, want)
		}
	}
}

func TestOrdinaryFilesAreNotRisky(t *testing.T) {
	for _, name := range []string{"report.pdf", "photo.JPG", "notes.txt", "data.csv", "a.docx",
		"b.xlsx", "c.png", "archive.zip", "readme", "noext.", "x.exe.txt", "jsx.md"} {
		if got := Classify(name, "application/octet-stream", []byte("hello world")); got.Risky {
			t.Errorf("%s: unexpectedly risky: %+v", name, got)
		}
	}
	// An ordinary file that merely starts with "MZ" is not a program.
	if got := Classify("memo.txt", "text/plain", []byte("MZ is the code for Zambia")); got.Risky {
		t.Errorf("MZ text: %+v", got)
	}
	// A shebang in a named .txt/.py is a text file; only an extensionless one is a script.
	if got := Classify("run.py", "text/x-python", []byte("#!/usr/bin/env python\n")); got.Risky {
		t.Errorf("shebang in .py: %+v", got)
	}
}

func TestDoubleExtensionIsDisguised(t *testing.T) {
	for _, name := range []string{"invoice.pdf.exe", "photo.jpg.scr", "Invoice.PDF.Exe", "scan.docx.js", "x.txt.bat"} {
		if got := ClassifyName(name, ""); got.Reason != Disguised || !got.Risky {
			t.Errorf("%s: %+v, want disguised", name, got)
		}
	}
}

func TestEvasionsOfTheExtension(t *testing.T) {
	for _, name := range []string{"a.exe.", "a.exe ", "a.ex\u200be", "a.exe\u202e", "a.exe\x00.pdf", "C:\\Users\\x\\a.exe"} {
		if got := ClassifyName(name, ""); !got.Risky {
			t.Errorf("%q hid its extension: %+v", name, got)
		}
	}
}

func TestClassifyBySniff(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"setup.pdf", pe(), Disguised}, // a PE named .pdf
		{"readme", pe(), Program},      // no extension: nothing to lie about
		{"x.png", append([]byte("\x7fELF"), 0, 0), Disguised},
		{"x.pdf", []byte{0xCF, 0xFA, 0xED, 0xFE, 0, 0}, Disguised},
		{"script", []byte("#!/bin/sh\necho\n"), Script},
		{"setup.exe", pe(), Program}, // name and bytes agree
	}
	for _, c := range cases {
		if got := Classify(c.name, "application/pdf", c.data); !got.Risky || got.Reason != c.want {
			t.Errorf("%s: %+v, want %s", c.name, got, c.want)
		}
	}
	lnk := append(append([]byte{}, lnkHeader...), 0, 0, 0, 0)
	if got := Classify("pic.png", "image/png", lnk); got.Reason != Disguised {
		t.Errorf("lnk bytes named png: %+v", got)
	}
	iso := make([]byte, 0x8010)
	copy(iso[0x8001:], "CD001")
	if got := Classify("photo", "", iso); got.Reason != DiskImage {
		t.Errorf("iso bytes: %+v", got)
	}
}

func TestDeclaredTypeAddsButNeverRemoves(t *testing.T) {
	if got := Classify("thing", "application/x-msdownload; name=x", []byte("plain")); got.Reason != Program {
		t.Errorf("declared PE type: %+v", got)
	}
	// A benign declared type does not cancel a risky name.
	if got := Classify("x.exe", "application/pdf", []byte("plain")); !got.Risky {
		t.Errorf("benign type hid a risky name: %+v", got)
	}
	if got := Classify("x.pdf", "application/pdf", bytes.Repeat([]byte("a"), 100)); got.Risky {
		t.Errorf("plain pdf: %+v", got)
	}
}

func TestEveryReasonHasAnExtensionOrIsDisguised(t *testing.T) {
	seen := map[string]bool{Disguised: true}
	for _, r := range extReason {
		seen[r] = true
	}
	for _, r := range Reasons() {
		if !seen[r] {
			t.Errorf("reason %s is declared but no extension produces it", r)
		}
	}
}
