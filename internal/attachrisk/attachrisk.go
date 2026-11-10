// SPDX-License-Identifier: AGPL-3.0-only

// Package attachrisk decides whether an attachment is a "risky type": a file an
// operating system would run, install, mount or open with macros if the person
// double-clicked it (ADR-056, operator decision Q81).
//
// # What this is and is not
//
// It is a WARNING signal. The server already neutralises every type it does not
// know (octet-stream, attachment disposition, nosniff; blob.go safeContentType),
// so classifying a file as risky changes nothing about what the download route
// serves. It exists so the client can ask "are you sure?" before the file lands
// on a disk. A third-party client that ignores it gets the same neutralised
// response it always did.
//
// # Both signals, and either one is enough
//
// The filename extension is what an operating system acts on. The bytes are what
// the file is. A name and its bytes can disagree in both directions, and each
// disagreement is an attack: `invoice.pdf.exe` (a name that hides what it is) and
// a PE executable named `.pdf` (bytes that hide behind a name). Deciding by the
// extension alone misses the second; deciding by the bytes alone misses
// `payload.js`, which has no binary signature at all. So either is enough, and a
// disagreement is reported as its own reason, [Disguised].
//
// # What it deliberately does not do
//
// It reads the leading bytes only, and it does not open archives: the scan walk
// does not enumerate archive members and this package adds no archive parsing, so a
// .zip holding an .exe is not flagged, and a macro-enabled document is recognised
// by its extension or declared type, not by finding vbaProject.bin inside a zip.
// Those limits are written in ADR-056 rather than discovered later.
package attachrisk

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode"
)

// The reason codes. A closed set: the client maps each to a sentence, and a code
// outside it is shown with the generic one rather than dropped.
const (
	Program       = "program"
	Script        = "script"
	Installer     = "installer"
	MacroDocument = "macro_document"
	Shortcut      = "shortcut"
	DiskImage     = "disk_image"
	// Disguised: the name and the bytes disagree, or a document-like extension
	// precedes a risky one. The person is told the file is not what it looks like.
	Disguised = "disguised"
)

// Reasons lists every code, for tests and for documentation of the API field.
func Reasons() []string {
	return []string{Program, Script, Installer, MacroDocument, Shortcut, DiskImage, Disguised}
}

// Extensions lists every extension the classifier knows (without the dot), so a
// caller that bounds filenames can check its bound against the longest one.
func Extensions() []string {
	out := make([]string, 0, len(extReason))
	for e := range extReason {
		out = append(out, e)
	}
	return out
}

var extReason = map[string]string{
	"exe": Program, "com": Program, "scr": Program, "dll": Program, "cpl": Program,
	"jar": Program, "app": Program, "apk": Program, "pif": Program, "msc": Program,
	"chm": Program, "gadget": Program,

	"bat": Script, "cmd": Script, "ps1": Script, "psm1": Script, "vbs": Script, "vbe": Script,
	"js": Script, "jse": Script, "wsf": Script, "wsh": Script, "hta": Script, "sh": Script,
	"reg": Script, "sct": Script, "ws": Script, "vb": Script, "ps1xml": Script,
	"command": Script, "scpt": Script, "inf": Script,

	"msi": Installer, "msp": Installer, "dmg": Installer, "pkg": Installer, "deb": Installer,
	"rpm": Installer, "appx": Installer, "msix": Installer, "appxbundle": Installer,
	"msixbundle": Installer, "application": Installer, "appref-ms": Installer,

	"docm": MacroDocument, "dotm": MacroDocument, "xlsm": MacroDocument, "xltm": MacroDocument,
	"xlam": MacroDocument, "pptm": MacroDocument, "potm": MacroDocument, "ppam": MacroDocument,
	"ppsm": MacroDocument, "sldm": MacroDocument, "xlsb": MacroDocument,

	"lnk": Shortcut, "url": Shortcut, "library-ms": Shortcut, "search-ms": Shortcut,
	"settingcontent-ms": Shortcut,

	"iso": DiskImage, "img": DiskImage, "vhd": DiskImage, "vhdx": DiskImage,
}

// documentExts are the extensions a double-extension trick borrows to look
// harmless. A risky extension preceded by one of these is Disguised.
var documentExts = map[string]bool{
	"pdf": true, "doc": true, "docx": true, "xls": true, "xlsx": true, "ppt": true, "pptx": true,
	"txt": true, "rtf": true, "csv": true, "jpg": true, "jpeg": true, "png": true, "gif": true,
	"bmp": true, "tif": true, "tiff": true, "zip": true, "htm": true, "html": true, "mp3": true,
	"mp4": true, "odt": true, "ods": true,
}

// typeReason maps declared media types that name something runnable. Declared
// types are attacker-chosen, so they can only ADD a warning.
var typeReason = map[string]string{
	"application/x-msdownload":                                   Program,
	"application/x-dosexec":                                      Program,
	"application/vnd.microsoft.portable-executable":              Program,
	"application/x-msdos-program":                                Program,
	"application/x-executable":                                   Program,
	"application/x-mach-binary":                                  Program,
	"application/java-archive":                                   Program,
	"application/vnd.android.package-archive":                    Program,
	"application/x-sh":                                           Script,
	"application/x-bat":                                          Script,
	"application/x-msi":                                          Installer,
	"application/x-apple-diskimage":                              DiskImage,
	"application/x-iso9660-image":                                DiskImage,
	"application/x-ms-shortcut":                                  Shortcut,
	"application/vnd.ms-word.document.macroenabled.12":           MacroDocument,
	"application/vnd.ms-excel.sheet.macroenabled.12":             MacroDocument,
	"application/vnd.ms-powerpoint.presentation.macroenabled.12": MacroDocument,
	"application/vnd.ms-excel.sheet.binary.macroenabled.12":      MacroDocument,
	"application/vnd.ms-excel.template.macroenabled.12":          MacroDocument,
	"application/vnd.ms-excel.addin.macroenabled.12":             MacroDocument,
	"application/vnd.ms-word.template.macroenabled.12":           MacroDocument,
	"application/vnd.ms-powerpoint.slideshow.macroenabled.12":    MacroDocument,
	"application/vnd.ms-powerpoint.template.macroenabled.12":     MacroDocument,
	"application/vnd.ms-powerpoint.addin.macroenabled.12":        MacroDocument,
}

// Result is the verdict on one attachment.
type Result struct {
	Risky bool
	// Reason is one of the code constants; empty when not risky.
	Reason string
}

// Classify decides whether a part is risky from its filename, its declared media
// type and its decoded bytes. data may be nil or short; only its first few
// kilobytes are read (plus 0x8006 bytes for an ISO header, when present).
func Classify(filename, declaredType string, data []byte) Result {
	ext, prevExt := extensions(filename)
	byName, nameRisky := extReason[ext]
	bySniff := sniff(data, ext)
	byType, typeRisky := typeReason[mediaType(declaredType)]

	switch {
	case nameRisky && prevExt != "" && documentExts[prevExt]:
		// invoice.pdf.exe: the name is built to be read as its first extension.
		return Result{true, Disguised}
	case nameRisky:
		return Result{true, byName}
	case bySniff != "":
		// The bytes are risky and the name does not say so: a PE named .pdf, or a
		// file with no extension at all. With no extension there is no lie to
		// report, so it keeps its own reason; with ANY other extension it is
		// pretending to be something else.
		if ext == "" {
			return Result{true, bySniff}
		}
		return Result{true, Disguised}
	case typeRisky:
		return Result{true, byType}
	}
	return Result{}
}

// ClassifyName is Classify with no bytes: the filename and declared type only.
func ClassifyName(filename, declaredType string) Result { return Classify(filename, declaredType, nil) }

func mediaType(declared string) string {
	t := strings.ToLower(strings.TrimSpace(declared))
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}

// extensions returns the lower-cased last extension of a filename and the one
// before it ("" when absent).
//
// ⚠ THE NAME IS CLEANED FIRST, and each step is an evasion that works on a real
// operating system: Windows ignores trailing dots and spaces ("a.exe."
// runs as a.exe), a control character (NUL included) is dropped by the saved name, and a format character (a bidi
// override, a zero-width space) can sit between the dot and the extension
// without being drawn. None of them may hide the extension from this check.
func extensions(filename string) (last, prev string) {
	var b strings.Builder
	for _, r := range filename {
		// ⚠ A control rune (NUL included) is SKIPPED, never a place to stop. The
		// name the server sends is sanitizeFilename's, which drops them, so
		// "invoice.pdf\x00.exe" is saved as "invoice.pdf.exe"; stopping at the
		// NUL classified it as .pdf. Skipping reads the name the way the saved
		// file will be read.
		if r < 0x20 || r == 0x7f {
			continue
		}
		if unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
	}
	// Upper then lower: a simple lower-casing leaves U+017F (long s) as itself,
	// and upper-casing folds it to S, so ".ſcr" is matched as ".scr".
	name := strings.ToLower(strings.ToUpper(strings.TrimRight(b.String(), ". \t")))
	// Only the final path element: a client may send a path.
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return "", ""
	}
	last = strings.TrimSpace(name[i+1:])
	rest := name[:i]
	if j := strings.LastIndexByte(rest, '.'); j >= 0 {
		prev = strings.TrimSpace(rest[j+1:])
	}
	return last, prev
}

// sniff returns a reason when the leading bytes are an executable, shortcut or
// disk image, and "" otherwise. ext is the cleaned extension; it gates only the
// shebang check, because `#!` at the top of a .txt or .py attachment is an
// ordinary text file, while the same bytes in a file with no extension are what a
// shell will run.
func sniff(data []byte, ext string) string {
	if len(data) >= 4 {
		switch {
		case isPE(data):
			return Program
		case bytes.HasPrefix(data, []byte("\x7fELF")):
			return Program
		case isMachO(data):
			return Program
		}
	}
	if len(data) >= 20 && bytes.Equal(data[:20], lnkHeader) {
		return Shortcut
	}
	if len(data) >= 0x8006 && bytes.Equal(data[0x8001:0x8006], []byte("CD001")) {
		return DiskImage
	}
	if ext == "" && bytes.HasPrefix(data, []byte("#!")) {
		return Script
	}
	return ""
}

// lnkHeader is the fixed start of a Windows shell link: HeaderSize 0x4C and the
// link CLSID 00021401-0000-0000-C000-000000000046.
var lnkHeader = []byte{
	0x4C, 0x00, 0x00, 0x00,
	0x01, 0x14, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00,
	0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46,
}

// isPE reports a DOS "MZ" header whose e_lfanew points at "PE\0\0". Checking the
// second signature keeps an ordinary file that merely starts with "MZ" from being
// called a program; a truncated PE (the pointer is past what we hold) is still
// called one when the pointer is plausible, because a half-delivered executable is
// still what the name was meant to deliver.
func isPE(data []byte) bool {
	if len(data) < 0x40 || data[0] != 'M' || data[1] != 'Z' {
		return false
	}
	off := int(binary.LittleEndian.Uint32(data[0x3c:0x40]))
	if off < 0x40 || off > 1<<20 {
		return false
	}
	if off+4 > len(data) {
		return true
	}
	return bytes.Equal(data[off:off+4], []byte("PE\x00\x00"))
}

func isMachO(data []byte) bool {
	magic := binary.BigEndian.Uint32(data[:4])
	switch magic {
	case 0xFEEDFACE, 0xFEEDFACF, 0xCEFAEDFE, 0xCFFAEDFE, 0xCAFEBABE:
		return true
	}
	return false
}
