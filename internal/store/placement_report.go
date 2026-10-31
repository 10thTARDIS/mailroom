// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
)

// THE PLACEMENT REPORT (#714, mailroom-admin placement-report): read-only
// counts of what learned placement knows, what it did, how that went, and what
// a backfill of existing mail WOULD do. Every verdict in it comes from the same
// placement.Judge / placement.Decide / placement.Gate the ingest path calls,
// over the same evidence query, so the report cannot describe a decision the
// ingest path would not make (ADR-023 §2).
//
// IT WRITES NOTHING. It reads areas directly rather than through ListAreas,
// which would materialise a fresh account's default plan (and its change
// rows), and it reads outcomes through effectiveOutcomeSQL rather than ageing
// them with resolveAgedPlacements.

// PlacementReportArea is one area's line in the report.
type PlacementReportArea struct {
	AreaID   ulid.ULID
	Name     string
	Position int
	// ConfidentAddresses / ConfidentDomains: keys whose verdict is Confident
	// for this area.
	ConfidentAddresses, ConfidentDomains int
	Counts                               PlacementAreaCounts
	Gate                                 placement.GateStatus
}

// PlacementMove is one (now in -> learned) pair of the backfill simulation.
// Values, not pointers, because it is a map key. A zero From is Needs
// sorting (a real area id is never the zero ULID).
type PlacementMove struct {
	From, To ulid.ULID
}

// PlacementReport is everything placement-report prints.
type PlacementReport struct {
	Enabled bool
	Areas   []PlacementReportArea
	// KeyReasons counts keys by verdict, per key kind (address, domain).
	KeyReasons map[placement.KeyKind]map[placement.Reason]int
	// Filings is the number of user filings the evidence is built from.
	Filings int

	// THE BACKFILL SIMULATION over existing, non-trashed, non-draft mail:
	// PlanPlacementBackfill in upper-bound mode, the same planner
	// placement-backfill runs exactly. A message whose area a
	// user/rule/learned label already decides is HELD (never moved), and the
	// rest go through placement.Decide with the message's own key.
	//
	// Moves counts desk mail (deskMessageSQL) by (now in -> learned) pair;
	// NoOp is desk mail already in its learned area; OffDesk is decided mail
	// that is not on the desk (archived, other folders), which the backfill
	// never moves.
	Moves   map[PlacementMove]int
	NoOp    int
	OffDesk int
	Held    int
	// Abstain counts scanned, unheld mail by why nothing would be placed.
	Abstain map[placement.Reason]int
	Scanned int

	// Origins splits the placement log into arrival and backfill placements
	// by effective outcome; Batches lists the backfill's batches.
	Origins PlacementOrigins
	Batches []PlacementBackfillBatch
}

// PlacementReport computes the report. Ages are evaluated by the database's
// now(), the same clock the ingest path uses.
func (tx *UserTx) PlacementReport(ctx context.Context) (PlacementReport, error) {
	rep := PlacementReport{
		KeyReasons: map[placement.KeyKind]map[placement.Reason]int{
			placement.KeyAddress: {}, placement.KeyDomain: {},
		},
		Moves:   map[PlacementMove]int{},
		Abstain: map[placement.Reason]int{},
	}
	if err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE((SELECT learned_placement_enabled FROM app.desk_preferences WHERE user_id = $1), true)`,
		tx.userID.String()).Scan(&rep.Enabled); err != nil {
		return rep, fmt.Errorf("store: placement report (switch): %w", err)
	}

	// Areas, in rail order.
	rows, err := tx.tx.Query(ctx,
		`SELECT id, name, position FROM app.areas WHERE user_id = $1 ORDER BY position, id`,
		tx.userID.String())
	if err != nil {
		return rep, fmt.Errorf("store: placement report (areas): %w", err)
	}
	index := map[ulid.ULID]int{}
	for rows.Next() {
		var idStr string
		var a PlacementReportArea
		if err := rows.Scan(&idStr, &a.Name, &a.Position); err != nil {
			rows.Close()
			return rep, fmt.Errorf("store: placement report (scan area): %w", err)
		}
		id, err := ulid.Parse(idStr)
		if err != nil {
			rows.Close()
			return rep, fmt.Errorf("store: placement report: parse area id %q: %w", idStr, err)
		}
		a.AreaID = id
		index[id] = len(rep.Areas)
		rep.Areas = append(rep.Areas, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, fmt.Errorf("store: placement report (areas): %w", err)
	}

	// Evidence and key verdicts.
	filings, err := tx.placementFilings(ctx, "", time.Time{})
	if err != nil {
		return rep, err
	}
	rep.Filings = len(filings)
	byAddr, byDomain := aggregatePlacementEvidence(filings)
	corrections, err := tx.placementCorrections(ctx, nil, time.Time{})
	if err != nil {
		return rep, err
	}
	withCorrection := func(kind placement.KeyKind, m map[string]placement.Evidence) {
		for k, e := range m {
			e.LatestCorrection = corrections[placement.Key{Kind: kind, Value: k}]
			m[k] = e
		}
	}
	withCorrection(placement.KeyAddress, byAddr)
	withCorrection(placement.KeyDomain, byDomain)
	own, err := tx.ownIdentityDomains(ctx)
	if err != nil {
		return rep, err
	}
	// A key with a correction but no remaining filings (the user re-filed
	// every message elsewhere and then took those labels off too) has no
	// evidence and judges NoEvidence, which is what ingest would say.
	tally := func(kind placement.KeyKind, m map[string]placement.Evidence) {
		for k, e := range m {
			if kind == placement.KeyDomain && placement.DomainFallbackBlocked(k, own) {
				// A freemail or own domain never acts as a fallback, whatever
				// its filings say: reported as its own verdict.
				rep.KeyReasons[kind][placement.DomainBlocked]++
				continue
			}
			v := placement.Judge(kind, e)
			rep.KeyReasons[kind][v.Reason]++
			if v.Reason != placement.Confident {
				continue
			}
			if i, ok := index[*v.Area]; ok {
				if kind == placement.KeyAddress {
					rep.Areas[i].ConfidentAddresses++
				} else {
					rep.Areas[i].ConfidentDomains++
				}
			}
		}
	}
	tally(placement.KeyAddress, byAddr)
	tally(placement.KeyDomain, byDomain)

	// Placements and the gate.
	counts, err := tx.placementAreaCounts(ctx, nil)
	if err != nil {
		return rep, err
	}
	for i := range rep.Areas {
		c := counts[rep.Areas[i].AreaID]
		c.AreaID = rep.Areas[i].AreaID
		rep.Areas[i].Counts = c
		rep.Areas[i].Gate = placement.Gate(placement.GateCounts{Kept: c.GateKept, Corrected: c.GateCorrected})
	}

	// The backfill simulation: THE backfill planner (placement_backfill.go),
	// in upper-bound mode (no sender source, so no raw headers are read).
	plan, err := tx.PlanPlacementBackfill(ctx, PlacementBackfillOptions{})
	if err != nil {
		return rep, err
	}
	rep.Scanned, rep.Held, rep.NoOp, rep.OffDesk = plan.Scanned, plan.Held, plan.NoOp, plan.OffDesk
	rep.Moves, rep.Abstain = plan.Pairs, plan.Abstain

	// Arrival and backfill placements, apart.
	if rep.Origins, err = tx.placementOrigins(ctx); err != nil {
		return rep, err
	}
	if rep.Batches, err = tx.PlacementBackfillBatches(ctx); err != nil {
		return rep, err
	}
	return rep, nil
}
