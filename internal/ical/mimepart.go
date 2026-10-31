// SPDX-License-Identifier: AGPL-3.0-only

package ical

import (
	"errors"
	"sort"
	"strings"

	"mailroom/internal/mime"
)

// maxCandidates bounds how many calendar-looking parts one message is searched
// for. Real invitations carry one or two (Google: a text/calendar alternative
// AND an invite.ics attachment of the same bytes).
const maxCandidates = 4

// Found is the outcome of looking for an invitation in a message.
type Found struct {
	// Invite is the first candidate part that read as an invitation, or nil.
	Invite *Invite
	// Err is why NO candidate did, when there was at least one — so a calendar
	// that was present but too large is reported as such rather than looking
	// exactly like a message with no calendar. Nil when Invite is set, and nil
	// when the message carries no calendar at all (Present is then false).
	Err error
	// Present is whether the message carried any calendar part.
	Present bool
}

// FindInMessage looks for an iTIP invitation in a parsed message's TOP-LEVEL
// structure. A calendar inside a forwarded (message/rfc822) part is the
// forwarded message's, not this one's, and is not looked for: answering it
// would answer an invitation addressed to somebody else.
//
// Candidates, in preference order: text/calendar carrying a method parameter
// (RFC 6047 §2.4 requires it on an iMIP part), any other text/calendar,
// application/ics, and finally any part whose filename ends in .ics.
func FindInMessage(root *mime.Structure) Found {
	type cand struct {
		st   *mime.Structure
		rank int
	}
	var cands []cand
	var walk func(st *mime.Structure, depth int)
	walk = func(st *mime.Structure, depth int) {
		if st == nil || depth > mime.MaxDepth {
			return
		}
		if mime.IsMultipart(st.ContentType) {
			for _, s := range st.SubParts {
				walk(s, depth+1)
			}
			return
		}
		rank := -1
		switch {
		case st.ContentType == "text/calendar" && st.Params["method"] != "":
			rank = 0
		case st.ContentType == "text/calendar":
			rank = 1
		case st.ContentType == "application/ics":
			rank = 2
		case strings.HasSuffix(strings.ToLower(st.Filename), ".ics"):
			rank = 3
		}
		if rank >= 0 {
			cands = append(cands, cand{st: st, rank: rank})
		}
	}
	walk(root, 0)
	if len(cands) == 0 {
		return Found{}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].rank < cands[j].rank })
	if len(cands) > maxCandidates {
		cands = cands[:maxCandidates]
	}

	out := Found{Present: true}
	for _, c := range cands {
		var (
			inv *Invite
			err error
		)
		if len(c.st.Data) > MaxBytes {
			err = ErrTooLarge
		} else {
			inv, err = ExtractInvite(c.st.Data)
		}
		if err == nil {
			return Found{Invite: inv, Present: true}
		}
		// Keep the MOST informative failure: "too large" says more than "not an
		// invitation", which is what a published calendar sitting beside the
		// real invitation would otherwise overwrite it with.
		if out.Err == nil || errors.Is(err, ErrTooLarge) {
			out.Err = err
		}
	}
	return out
}
