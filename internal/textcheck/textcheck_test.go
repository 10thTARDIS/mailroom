// SPDX-License-Identifier: AGPL-3.0-only

package textcheck

import (
	"strings"
	"testing"
)

func TestCheckNameRefusesEachClass(t *testing.T) {
	for _, c := range []struct{ name, in string }{
		{"C0 NUL", "a\x00b"}, {"C0 newline", "a\nb"}, {"C0 tab", "a\tb"}, {"C0 ESC", "\x1b[31mred"}, {"DEL", "a\x7fb"},
		{"C1 NEL", "a\u0085b"}, {"C1 CSI", "a\u009bb"},
		{"bidi LRE", "a\u202ab"}, {"bidi RLO", "a\u202eb"}, {"bidi PDF", "a\u202cb"}, {"bidi LRM mark", "a\u200eb"},
		{"isolate LRI", "a\u2066b"}, {"isolate FSI", "a\u2068b"}, {"isolate PDI", "a\u2069b"}, {"ALM", "a\u061cb"},
		{"other Cf zero width space", "a\u200bb"}, {"other Cf word joiner", "a\u2060b"}, {"other Cf BOM", "\ufeffab"},
		{"other Cf soft hyphen", "a\u00adb"},
		{"line separator", "a\u2028b"}, {"paragraph separator", "a\u2029b"},
		{"invalid UTF-8", "a\xffb"},
	} {
		if err := CheckName(c.in); err == nil {
			t.Errorf("%s: %q was accepted", c.name, c.in)
		}
	}
}

// POSITIVE CONTROL: without it the refusals above would pass against a validator
// that refuses everything.
func TestCheckNameAcceptsInternationalNames(t *testing.T) {
	for _, in := range []string{
		"Quarterly invoices", "Zoë's rules", "قواعد البريد", "שלום", "請求書フィルター", "한국어 규칙", "Правила",
		"mail 📬 sorter", "👍🏽 thumbs", "❤️ heart", "é combining acute", "นามสกุล ไทย", "देवनागरी नाम",
		"a-b_c.d (1)", "x y", "� replacement char",
	} {
		if err := CheckName(in); err != nil {
			t.Errorf("%q refused: %v", in, err)
		}
	}
}

func TestCheckNameMessageDoesNotEchoTheCharacter(t *testing.T) {
	err := CheckName("a\u202eb")
	if err == nil || strings.ContainsRune(err.Error(), '\u202e') || !strings.Contains(err.Error(), "U+202E") {
		t.Fatalf("message %v", err)
	}
}
