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
		{"bidi LRE", "a‪b"}, {"bidi RLO", "a‮b"}, {"bidi PDF", "a‬b"}, {"bidi LRM mark", "a‎b"},
		{"isolate LRI", "a⁦b"}, {"isolate FSI", "a⁨b"}, {"isolate PDI", "a⁩b"}, {"ALM", "a؜b"},
		{"other Cf zero width space", "a​b"}, {"other Cf word joiner", "a⁠b"}, {"other Cf BOM", "\ufeffab"},
		{"other Cf soft hyphen", "a­b"},
		{"line separator", "a b"}, {"paragraph separator", "a b"},
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
	err := CheckName("a‮b")
	if err == nil || strings.ContainsRune(err.Error(), '‮') || !strings.Contains(err.Error(), "U+202E") {
		t.Fatalf("message %v", err)
	}
}
