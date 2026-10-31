// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Email/dismissCategory — "this does not need a category" (spec §8,
// mailroom#176, epic #156).
//
// The needs-a-category surface is a queue of QUESTIONS, and a queue you can only
// answer is not a queue, it is a chore. Plenty of mail has no right answer: it
// belongs in no category the user keeps, it is a one-off from a sender they will
// never hear from again, or the label the model reached for simply does not
// apply. Without a way to say so, the only ways to clear such a message are to
// file it somewhere false — poisoning the very corpus this surface exists to
// grow — or to leave it queued forever, which teaches the user to stop opening
// the queue. Both are worse than the question never being asked.
//
// It closes any open suggestions AND writes the tombstone that keeps the message
// out of the queue. Both, because the queue is "nothing has categorised this",
// so an unlabelled message stays queued on the strength of that absence no
// matter how many suggestions are deleted (store.DismissCategoryQuestions).
//
// IT IS NOT A REJECTION SIGNAL. It records nothing about any label being wrong.
// Turning a dismissal into negative training data is a real idea and a separate
// one (#169): a user clearing a first-run backlog is saying "not now" at least
// as often as "not this", and the two are indistinguishable from here. Guessing
// which, and then training on the guess, is how a corpus quietly acquires labels
// nobody meant.
//
// Its own method rather than an Email/set patch. Email/set is for the message's
// own state — keywords, mailboxes, the things every JMAP client understands —
// and this is not part of it: nothing here changes what the message IS, only
// what the server is still asking about it. Overloading a settable property onto
// a read-only one would also make the answer to "can I write suggestedLabels?"
// depend on which value you write.

type dismissCategoryArgs struct {
	AccountID string `json:"accountId"`
	// IDs are the messages whose open questions to close. Batch, because the
	// first-run case is a backlog and clearing it one round trip at a time is
	// the thing that makes people give up on it.
	IDs []string `json:"ids"`
	// Undo reverses a dismissal instead of making one: the tombstones come off
	// and the messages return to the queue.
	//
	// A flag on the same method rather than its own, because it is the same
	// decision being taken back — and because the pair has to stay symmetric.
	// It does NOT restore suggestions the dismissal deleted; the message goes
	// back to being merely uncategorised, which is the honest end state and is
	// still enough for it to be asked about again.
	Undo bool `json:"undo"`
}

type dismissCategoryResult struct {
	AccountID string `json:"accountId"`
	// Dismissed lists the messages something actually changed for — or, under
	// `undo`, the ones actually restored. A client can then tell "cleared 12"
	// from "asked about 20 and 8 were already settled", and offer an undo over
	// exactly what it changed rather than over what it asked about. A message
	// already dismissed is NOT an error: the surface is shared with the ordinary
	// label path, and racing a label against a dismiss is normal traffic.
	Dismissed []string `json:"dismissed"`
	// NotFound is the ids that are not this user's messages, in the RFC 8620
	// tradition of naming what could not be acted on rather than failing the
	// whole call. Another user's id and a deleted one are indistinguishable here
	// by design — that is the isolation property, not an oversight.
	NotFound []string `json:"notFound"`
}

// dismissMaxIDs bounds one call. Generous — a first-run backlog is the point —
// but bounded, because every id costs a change_log row and this is the one
// method a client would naturally hand an unbounded selection to ("dismiss
// everything matching").
const dismissMaxIDs = 500

func (a *API) emailDismissCategory(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args dismissCategoryArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if len(args.IDs) > dismissMaxIDs {
		return nil, &MethodError{Type: errRequestTooLarge}
	}

	res := dismissCategoryResult{
		AccountID: userID.String(),
		Dismissed: []string{},
		NotFound:  []string{},
	}
	ids := make([]ulid.ULID, 0, len(args.IDs))
	for _, raw := range args.IDs {
		id, err := ulid.Parse(strings.TrimSpace(raw))
		if err != nil {
			// An unparseable id cannot name a message of ANYONE'S, so it is
			// notFound rather than an invalidArguments that would fail the whole
			// batch over one malformed entry.
			res.NotFound = append(res.NotFound, raw)
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return mustMarshal(res), nil
	}

	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		// Existence is checked FIRST, against the user's own messages, so an id
		// belonging to another user is reported as notFound instead of silently
		// dismissing nothing and reading as success. RLS makes the check
		// fail-closed: another user's message is simply not there.
		views, err := tx.GetEmails(ctx, ids)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		mine := make(map[ulid.ULID]bool, len(views))
		for _, v := range views {
			mine[v.ID] = true
		}
		present := make([]ulid.ULID, 0, len(ids))
		for _, id := range ids {
			if mine[id] {
				present = append(present, id)
				continue
			}
			res.NotFound = append(res.NotFound, id.String())
		}
		if len(present) == 0 {
			return nil
		}
		var dismissed []ulid.ULID
		if args.Undo {
			dismissed, err = tx.UndoCategoryDismissal(ctx, present)
		} else {
			dismissed, err = tx.DismissCategoryQuestions(ctx, present)
		}
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		for _, id := range dismissed {
			res.Dismissed = append(res.Dismissed, id.String())
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	return mustMarshal(res), nil
}
