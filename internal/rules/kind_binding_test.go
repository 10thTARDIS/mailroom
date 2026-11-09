// SPDX-License-Identifier: AGPL-3.0-only

package rules

import (
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

func kindRule(kind string) Rule {
	return Rule{
		ID: ulid.Make(), Name: "r-" + kind, Enabled: true, Kind: kind,
		Conditions: Condition{Field: FieldFromDomain, Op: OpEquals, Value: "example.com"},
		Actions:    []Action{{Type: ActionAddLabel, Label: "L"}},
	}
}

func evalKind(r Rule, kind string) int {
	f := facts()
	f.Kind = kind
	return len(Evaluate([]Rule{r}, f))
}

// A rule bound to a kind applies to that kind and not to another.
func TestKindBoundRuleAppliesOnlyToItsKind(t *testing.T) {
	r := kindRule("promotional")
	if evalKind(r, "promotional") != 1 {
		t.Fatal("a rule bound to promotional did not apply to promotional mail")
	}
	for _, other := range []string{"personal", "transactional", "list", "unknown"} {
		if evalKind(r, other) != 0 {
			t.Errorf("a rule bound to promotional applied to %s mail", other)
		}
	}
}

// An unbound rule is unchanged, whatever kind the message has (or whether it was
// classified at all).
func TestUnboundRuleIgnoresKind(t *testing.T) {
	r := kindRule("")
	for _, k := range []string{"", "promotional", "personal", "unknown"} {
		if evalKind(r, k) != 1 {
			t.Errorf("an unbound rule did not apply with message kind %q", k)
		}
	}
}

// FAIL CLOSED: a message nobody classified never satisfies a kind-bound rule, so a
// path that cannot say what kind a message is cannot apply the rule to all mail.
func TestKindBoundRuleDoesNotMatchAnUnclassifiedMessage(t *testing.T) {
	if evalKind(kindRule("promotional"), "") != 0 {
		t.Fatal("a kind-bound rule matched a message with no kind")
	}
}

func TestUsesKindOnlyCountsEnabledBoundRules(t *testing.T) {
	off := kindRule("list")
	off.Enabled = false
	if UsesKind([]Rule{kindRule(""), off}) {
		t.Error("UsesKind true with no enabled bound rule")
	}
	if !UsesKind([]Rule{kindRule(""), kindRule("list")}) {
		t.Error("UsesKind false with an enabled bound rule")
	}
}

func TestValidateKind(t *testing.T) {
	for _, ok := range []string{"", "personal", "transactional", "promotional", "notification", "list", "automated", "unknown"} {
		if err := ValidateKind(ok); err != nil {
			t.Errorf("ValidateKind(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"Promotional", "spam", "promo ", "all"} {
		err := ValidateKind(bad)
		if err == nil {
			t.Errorf("ValidateKind(%q) = nil, want an error", bad)
		} else if !strings.Contains(err.Error(), "unknown message kind") {
			t.Errorf("ValidateKind(%q) error = %v", bad, err)
		}
	}
	r := kindRule("spam")
	if err := Validate(r); err == nil {
		t.Error("Validate accepted a rule with an invalid kind")
	}
}
