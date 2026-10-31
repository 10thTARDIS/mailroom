// SPDX-License-Identifier: AGPL-3.0-only

package kind

import "testing"

// SubjectClasses is used as a machine-learning feature (#169), which puts two
// constraints on it that the rest of this package does not carry.

// IT MUST DEPEND ON NOTHING BUT THE SUBJECT. The subject is a stored column, so
// a subject-only function returns the same answer on the ingest path and on the
// replay/training path. Any dependence on a header would make the feature
// present in production and absent in training — the train/serve gap #157 was
// fixed to close, and the reason the rest of this package's signals are NOT
// exposed as features.
//
// The signature enforces this (it takes only a string), so this test guards the
// weaker but still useful property: identical subjects give identical answers,
// whatever else might be true of the message they came from.
func TestSubjectClassesDependsOnlyOnTheSubject(t *testing.T) {
	const subject = "Your order #1234567 has shipped"
	first := SubjectClasses(subject)
	for i := 0; i < 3; i++ {
		again := SubjectClasses(subject)
		if len(again) != len(first) {
			t.Fatalf("call %d returned %d classes, first returned %d", i, len(again), len(first))
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("call %d differs at %d: %q vs %q", i, j, again[j], first[j])
			}
		}
	}
}

// The classes must actually fire on the vocabulary they claim, or the feature is
// a constant and the model learns nothing from it.
func TestSubjectClassesRecogniseTheirVocabulary(t *testing.T) {
	has := func(cs []SubjectClass, want SubjectClass) bool {
		for _, c := range cs {
			if c == want {
				return true
			}
		}
		return false
	}
	cases := []struct {
		subject string
		want    SubjectClass
	}{
		{"Your order has shipped", SubjectTransactional},
		{"Payment confirmation for your booking", SubjectTransactional},
		{"Order #1234567 update", SubjectOrderNumber},
		{"50% off everything — last chance", SubjectPromotional},
		{"Free shipping this weekend only", SubjectPromotional},
		{"Your daily digest", SubjectNotification},
		{"New sign-in to your account", SubjectNotification},
	}
	for _, c := range cases {
		if got := SubjectClasses(c.subject); !has(got, c.want) {
			t.Errorf("SubjectClasses(%q) = %v, want it to include %q", c.subject, got, c.want)
		}
	}
}

// AND THEY MUST DISCRIMINATE. A lexicon that fires on everything is worse than
// none: it adds a constant to every example, costs a weight, and teaches the
// model nothing — while looking like a feature in the report.
func TestSubjectClassesDoNotFireOnOrdinaryMail(t *testing.T) {
	for _, subject := range []string{
		"lunch tomorrow?",
		"Re: the thing we discussed",
		"notes from Tuesday",
		"",
	} {
		if got := SubjectClasses(subject); len(got) != 0 {
			t.Errorf("SubjectClasses(%q) = %v, want none — this vocabulary is supposed to be specific", subject, got)
		}
	}
}

// The promotional and transactional lexicons must not both claim the same
// message wholesale; the milestone's acceptance case is a retailer promo and a
// retailer receipt landing in different categories, and these are the only
// sender-independent signals that can tell them apart.
func TestPromotionalAndTransactionalSubjectsSeparate(t *testing.T) {
	promo := SubjectClasses("Last chance: 40% off everything")
	receipt := SubjectClasses("Your order has shipped")

	hasPromo := false
	for _, c := range promo {
		if c == SubjectPromotional {
			hasPromo = true
		}
		if c == SubjectTransactional {
			t.Errorf("a pure promo subject also read as transactional: %v", promo)
		}
	}
	if !hasPromo {
		t.Errorf("promo subject did not read as promotional: %v", promo)
	}
	for _, c := range receipt {
		if c == SubjectPromotional {
			t.Errorf("a receipt subject also read as promotional: %v", receipt)
		}
	}
}
