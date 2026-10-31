// SPDX-License-Identifier: AGPL-3.0-only

package ml

import "strings"

// FeatureLabel renders a feature key (the keys featuresFromFacts mints, plus the
// synthetic coldStartDirectKey the cold-start explanation adds) into a short,
// human-readable phrase for the priorityReasons Email property (#34). It lives
// beside features.go on purpose: the keys and their labels must stay in sync, so
// a new feature key adds its phrase HERE.
//
// It is a PURE function: labels are applied at render time (never stored), so the
// wording is free to change without touching any persisted top_features payload.
// An unrecognized key falls back to the key itself rather than an empty string,
// so a future feature is at least self-describing until a phrase is added.
func FeatureLabel(key string) string {
	switch key {
	case "corr:1":
		// NOT "a known contact" — that was the old key's wording and the old key's
		// mistake (#157). Being in the address book only ever meant they had mailed
		// you, which is true of every newsletter you have ever received. This says
		// what the feature now actually measures.
		return "You have written to this sender"
	case "reply:1":
		return "A reply in a thread you're part of"
	case "list:1":
		return "Mailing-list mail"
	case "unsub:1":
		return "Bulk mail (has unsubscribe)"
	case "noreply:1":
		return "Sent from a no-reply address"
	case "authfail:1":
		return "Failed sender authentication"
	case "flagged:1":
		return "Flagged by your mail server"
	case "attach:1":
		return "Has an attachment"
	case coldStartDirectKey:
		return "Addressed directly to you"

	// The curated subject vocabularies (#169). Phrased as what the SUBJECT reads
	// like, not as a category verdict — this is one signal among many, and "looks
	// like a receipt" is a description the user can check against the message in
	// front of them, whereas "transactional" is a claim about what it IS.
	case "subjclass:transactional":
		return "Subject reads like a receipt or confirmation"
	case "subjclass:ordernum":
		return "Subject contains an order or reference number"
	case "subjclass:promotional":
		return "Subject reads like a sales campaign"
	case "subjclass:notification":
		return "Subject reads like an alert or digest"
	}

	if v, ok := strings.CutPrefix(key, "from:"); ok {
		return "From " + v
	}
	if v, ok := strings.CutPrefix(key, "dom:"); ok {
		return "From the domain " + v
	}
	if v, ok := strings.CutPrefix(key, "alias:"); ok {
		return "Sent to " + v
	}
	if v, ok := strings.CutPrefix(key, "subtok:"); ok {
		return "Subject mentions “" + v + "”"
	}
	if _, ok := strings.CutPrefix(key, "sent:"); ok {
		return "You've written to them before"
	}
	if _, ok := strings.CutPrefix(key, "oneway:"); ok {
		return "Mails you often, never answered"
	}
	if v, ok := strings.CutPrefix(key, "tocc:"); ok {
		return toccLabel(v)
	}
	if v, ok := strings.CutPrefix(key, "hour:"); ok {
		return "Received around " + v + ":00"
	}
	return key
}

// toccLabel renders a tocc:<bucket> recipient-count key. Only the large buckets
// ever appear in an explanation (small ones feed the direct-to-you proxy, which
// is reported under coldStartDirectKey), so the phrasing leans on "many".
func toccLabel(bucket string) string {
	switch bucket {
	case "0", "1":
		return "Addressed to just you"
	default:
		return "Sent to many recipients"
	}
}
