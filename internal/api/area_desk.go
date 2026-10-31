// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// ---- Area/getDesk (#665) ----------------------------------------------------
//
// A whole Review-first desk — Area/getSummary's counts plus the first page and
// thread-collapsed total of every requested section — answered from ONE
// resolution of every message's area (store.DeskSnapshot). It replaces what a
// desk load used to send as Area/getSummary plus one `Email/query
// {inArea|unsorted, onDesk, onSurface, isSnoozed:false, inAccount?}` with
// collapseThreads per section, each of which re-ran the area resolver over the
// whole mailbox.
//
// A Mailroom Type/verb extension under urn:mailroom:desk, the same standing
// Area/getSummary has (ADR-002: extensions add methods under a capability URN
// and never change a core method's semantics — Email/query is untouched). It is
// advertised as `getDesk: true` in the capability object so a client can tell a
// server that has it from one that predates it without a failed call.
//
// The ids it returns are hydrated with ONE Email/get through a result reference
// to `/sections/*/ids` — which is why the per-section limit is capped at
// maxObjectsInGet and why the client, not this method, decides whether the sum
// of its sections fits one Email/get (see docs/openapi.yaml).

// maxDeskSections bounds one Area/getDesk. A section is an area (or Needs
// sorting), so this is far above any real desk; it exists so a request cannot
// make one statement with an unbounded number of CTEs.
const maxDeskSections = 100

// deskSectionDefaultLimit is a section's window when the request names none —
// the web client's list page (web/src/core/sync.ts PAGE_SIZE).
const deskSectionDefaultLimit = 50

// maxDeskSectionLimit caps one section's window at the session's advertised
// maxObjectsInGet (session.go): a window larger than that could not be
// hydrated by one Email/get anyway. A clamped section reports the `limit` it
// was answered with, as RFC 8620 §5.5 requires of a query that clamps.
const maxDeskSectionLimit = 500

type areaGetDeskSection struct {
	// AreaID names an area, or is null for Needs sorting.
	AreaID   *string `json:"areaId"`
	Position int     `json:"position"`
	Limit    *int    `json:"limit"`
}

type areaGetDeskArgs struct {
	AccountID     string               `json:"accountId"`
	MailAccountID *string              `json:"mailAccountId"`
	Threshold     *float64             `json:"threshold"`
	Sections      []areaGetDeskSection `json:"sections"`
}

func (a *API) areaGetDesk(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args areaGetDeskArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	var scope store.DeskScope
	if args.MailAccountID != nil {
		id, err := ulid.Parse(*args.MailAccountID)
		if err != nil {
			return nil, &MethodError{Type: errInvalidArguments, Description: "mailAccountId is not a valid id"}
		}
		scope.AccountID = &id
	}
	thresholdGiven := args.Threshold != nil
	var threshold float64
	if thresholdGiven {
		threshold = *args.Threshold
		if threshold < 0 || threshold > 1 {
			return nil, &MethodError{Type: errInvalidArguments, Description: "threshold must be between 0 and 1"}
		}
	}
	if len(args.Sections) > maxDeskSections {
		return nil, &MethodError{Type: errInvalidArguments, Description: "too many sections"}
	}

	// Parsed up front; clamped tracks which sections were answered with a
	// smaller window than they asked for.
	requested := make([]store.DeskSectionPage, len(args.Sections))
	clamped := make([]bool, len(args.Sections))
	for i, s := range args.Sections {
		if s.Position < 0 {
			return nil, &MethodError{Type: errInvalidArguments, Description: "position must be >= 0"}
		}
		limit := deskSectionDefaultLimit
		if s.Limit != nil {
			if *s.Limit < 0 {
				return nil, &MethodError{Type: errInvalidArguments, Description: "limit must be >= 0"}
			}
			limit = *s.Limit
		}
		if limit > maxDeskSectionLimit {
			limit, clamped[i] = maxDeskSectionLimit, true
		}
		requested[i] = store.DeskSectionPage{Position: s.Position, Limit: limit}
		if s.AreaID != nil {
			id, err := ulid.Parse(*s.AreaID)
			if err != nil {
				return nil, &MethodError{Type: errInvalidArguments, Description: "areaId is not an area id"}
			}
			requested[i].AreaID = &id
		}
	}

	var (
		snap          store.DeskSnapshotResult
		archivedCount map[ulid.ULID]int
		areaState     int64
		emailState    int64
		notFound      []string
		// asked maps each section the store answered back to its request index.
		asked []int
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		// Materialise first, exactly as Area/getSummary does — and the list is
		// also what tells a deleted area from a live one.
		views, err := tx.ListAreas(ctx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		exists := make(map[ulid.ULID]bool, len(views))
		for _, v := range views {
			exists[v.ID] = true
		}
		// An area that no longer exists (destroyed from another device) is
		// reported in notFound rather than failing the whole desk: Email/query
		// refused it per section, and one stale area must not blank the rest.
		var sections []store.DeskSectionPage
		for i, s := range requested {
			if s.AreaID != nil && !exists[*s.AreaID] {
				notFound = append(notFound, s.AreaID.String())
				continue
			}
			sections = append(sections, s)
			asked = append(asked, i)
		}
		if !thresholdGiven {
			prefs, err := tx.GetDeskPreferences(ctx)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			threshold = float64(prefs.LineThreshold)
		}
		scope.Threshold = float32(threshold)
		if snap, err = tx.DeskSnapshot(ctx, store.DeskSnapshotQuery{Scope: scope, Sections: sections}); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if archivedCount, err = tx.DrawerArchiveCounts(ctx); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if areaState, err = tx.ChangeState(ctx, store.ObjectArea); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if emailState, err = tx.ChangeState(ctx, store.ObjectEmail); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}

	sections := make([]map[string]any, 0, len(snap.Sections))
	for j, s := range snap.Sections {
		i := asked[j]
		var areaID any
		if s.AreaID != nil {
			areaID = s.AreaID.String()
		}
		obj := map[string]any{
			"areaId":   areaID,
			"position": requested[i].Position,
			"total":    s.Total,
			"ids":      ulidStrings(s.IDs),
		}
		if clamped[i] {
			obj["limit"] = requested[i].Limit
		}
		sections = append(sections, obj)
	}
	if notFound == nil {
		notFound = []string{}
	}

	res := map[string]any{
		"accountId": userID.String(),
		// The Area state, as Area/getSummary reports it: what the counts were
		// taken at.
		"state": encodeState(areaState),
		// The Email state, as Email/query reports it as queryState: what the
		// sections' ids were taken at. Both are from the same transaction.
		"queryState": encodeState(emailState),
		// float32, for the reason areaGetSummary gives.
		"threshold": scope.Threshold,
		"list":      summaryList(snap.Summary, archivedCount),
		"sections":  sections,
		"notFound":  notFound,
	}
	if scope.AccountID != nil {
		res["mailAccountId"] = scope.AccountID.String()
	}
	return mustMarshal(res), nil
}
