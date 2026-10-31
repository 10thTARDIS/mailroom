// SPDX-License-Identifier: AGPL-3.0-only

package exporter

import (
	"context"
	"fmt"
	"time"

	"mailroom/internal/store"
)

// The configuration sidecars: everything the user WROTE that is not a message
// (#54, spec §10).
//
// Spec §10 names labels, scores and contacts. These go beyond that sentence
// deliberately, and the export plan records the widening: identities, rules,
// signatures and templates, saved searches, category tabs and remote-image
// allowances are all things a user AUTHORED. Losing them is losing work, not
// losing derived state, and re-creating them by hand is the tedium a portability
// promise is supposed to spare someone.
//
// ⚠ NOT ONE OF THESE FILES CONTAINS AN INTERNAL ID, and the references between
// them are resolved to durable values here rather than left as ULIDs:
//
//   - a signature bound to an identity names that identity's ADDRESS
//   - a send record names the message's CONTENT HASH, which is a file in the
//     exported maildir — so the archive is self-referential in a way another
//     system can actually follow
//   - a label's parent is its parent's NAME (sidecar.go)
//
// A ULID in an archive looks like data and is a dangling pointer into a database
// the reader does not have.

type sidecarAccount struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	CreatedAt   string `json:"created_at"`
	// Note is carried IN the file because the absence of a field is not
	// self-explaining: a reader wondering where their login identifier went
	// should find the answer here, not in a changelog.
	Note string `json:"note"`
}

type sidecarIdentity struct {
	Email         string `json:"email"`
	Kind          string `json:"kind"`
	DisplayName   string `json:"display_name,omitempty"`
	SignatureHTML string `json:"signature_html,omitempty"`
	Enabled       bool   `json:"enabled"`
}

type sidecarRule struct {
	Name            string `json:"name"`
	Priority        int    `json:"priority"`
	Enabled         bool   `json:"enabled"`
	SystemSuggested bool   `json:"system_suggested,omitempty"`
	// Conditions are the rule's own structure, verbatim: they match on message
	// FACTS (from, subject, a header value) and hold no ids.
	Conditions any `json:"conditions"`
	// Actions are NOT verbatim — see sidecarAction.
	Actions []sidecarAction `json:"actions"`
}

// sidecarAction is a rule action with its label reference RESOLVED TO A NAME.
//
// ⚠ THIS EXISTS BECAUSE THE LIVE RUN FOUND 12 DANGLING POINTERS AND THE TEST DID
// NOT. `rules.Action.Label` holds a label's ULID — a real rule reads
// `{"type":"addLabel","label":"01KYJ2CC0H…"}` — so exporting actions verbatim put
// an internal id into rules.json for every labelling rule the user has. The
// archive's own rule is that nothing in it may point into a database the reader
// does not have, and a rule whose action names a ULID cannot be reconstructed
// anywhere: the label it means is unfindable.
//
// ⚠ THE TEST MISSED IT BECAUSE THE FIXTURE WAS TOO SIMPLE — it seeded a rule with
// `actions = '[]'`, which carries no reference and therefore cannot leak one. The
// fixture now uses a real addLabel action. A negative control is only as good as
// the population it runs against, and an empty one proves nothing.
type sidecarAction struct {
	Type string `json:"type"`
	// Label is the label's NAME. Empty when the action does not label, or when
	// the id names a label that no longer exists — blank rather than a ULID,
	// because a dangling id in an archive looks like data.
	Label    string `json:"label,omitempty"`
	Mailbox  string `json:"mailbox,omitempty"`
	Evidence int    `json:"evidence,omitempty"`
}

type sidecarSnippet struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Identity is the ADDRESS this signature is bound to, or empty for "any".
	Identity  string `json:"identity,omitempty"`
	BodyHTML  string `json:"body_html,omitempty"`
	BodyText  string `json:"body_text,omitempty"`
	IsDefault bool   `json:"is_default,omitempty"`
}

type sidecarSavedSearch struct {
	Name      string `json:"name"`
	Query     string `json:"query"`
	InSidebar bool   `json:"in_sidebar"`
	Position  int    `json:"position"`
}

type sidecarTab struct {
	Tab      string `json:"tab"`
	Enabled  bool   `json:"enabled"`
	Position int    `json:"position"`
}

type sidecarRemoteAllow struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// sidecarArea is one Area (#442 increment 2), with its own label and its
// additional label feeds resolved to NAMES — like sidecarAction.Label above,
// a ULID would be a dangling pointer into a database the importing side does
// not have, so this is exactly the same resolution rules.Actions already get.
type sidecarArea struct {
	Name string `json:"name"`
	// Label is the area's OWN label's name ("areas are labels": placing a
	// message in the area is applying this label).
	Label    string   `json:"label"`
	Position int      `json:"position"`
	Tabs     []string `json:"tabs,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	// AutoArchive (#443, migration 00048) is whether this drawer sweeps at
	// all; AutoArchiveDays is only the period (null = the login default, not
	// "off" — "off" is AutoArchive: false).
	AutoArchive     bool `json:"auto_archive"`
	AutoArchiveDays *int `json:"auto_archive_days,omitempty"`
	HiddenFromRail  bool `json:"hidden_from_rail,omitempty"`
}

// sidecarDeskPreferences is the desk-wide settings singleton (#442).
type sidecarDeskPreferences struct {
	LineThreshold     float32 `json:"line_threshold"`
	DrawerDefaultDays int     `json:"drawer_default_days"`
	TrayNagDays       int     `json:"tray_nag_days"`
	// DrawerSweepEnabled is the drawer sweep's login-level master switch
	// (migration 00048, #443), default false — exported so an operator
	// reading a config export can see whether the sweep was armed without
	// opening the database.
	DrawerSweepEnabled bool `json:"drawer_sweep_enabled"`
	// LearnedPlacementEnabled is learned placement's off switch (#714,
	// migration 00060), default true: a user who switched it off must not
	// find it back on after a migration.
	LearnedPlacementEnabled bool `json:"learned_placement_enabled"`
	// QuietHours (#59, migration 00061): the user's own setting, exported for
	// learned_placement_enabled's reason — a migrated user must not find
	// their nights noisy again.
	QuietHours sidecarQuietHours `json:"quiet_hours"`
	// FollowUp (#58, migration 00064): the follow-up nudge switches and the
	// inbound delay — the user's own choice, so an opted-in user stays opted in.
	FollowUp sidecarFollowUp `json:"follow_up"`
}

type sidecarFollowUp struct {
	Outbound    bool `json:"outbound"`
	Inbound     bool `json:"inbound"`
	InboundDays int  `json:"inbound_days"`
}

// sidecarOutOfOffice is one account's out-of-office reply (#50).
type sidecarOutOfOffice struct {
	Account      string     `json:"account"`
	Enabled      bool       `json:"enabled"`
	From         *time.Time `json:"from,omitempty"`
	To           *time.Time `json:"to,omitempty"`
	Subject      string     `json:"subject"`
	Body         string     `json:"body"`
	ReplyDays    int        `json:"reply_days"`
	FromAddress  string     `json:"from_address"`
	ContactsOnly bool       `json:"contacts_only"`
}

type sidecarQuietHours struct {
	Enabled  bool   `json:"enabled"`
	Start    int    `json:"start_minute"`
	End      int    `json:"end_minute"`
	Days     int    `json:"days_mask"`
	Timezone string `json:"timezone"`
	AllowVIP bool   `json:"allow_vip"`
}

// sidecarClientPreferences is one client family's display preferences
// (#442). No id: the family name IS the identity an import re-creates by.
type sidecarClientPreferences struct {
	Family       string `json:"family"`
	Theme        string `json:"theme"`
	Density      string `json:"density"`
	MessageOpens string `json:"message_opens"`
	PaneSplitPct int    `json:"pane_split_pct"`
}

// exportClientFamilies is the fixed order client_preferences.json lists the
// three families in.
var exportClientFamilies = []store.ClientFamily{store.ClientWeb, store.ClientPhone, store.ClientDesktop}

type sidecarContactKey struct {
	Kind        string `json:"kind"`
	Fingerprint string `json:"fingerprint"`
	Source      string `json:"source"`
	FetchedAt   string `json:"fetched_at"`
}

type sidecarContactAddress struct {
	Address     string `json:"address"`
	ReachedMeAt string `json:"reached_me_at,omitempty"`
	FirstSeen   string `json:"first_seen"`
	LastSeen    string `json:"last_seen"`
	SeenCount   int    `json:"seen_count"`
}

type sidecarContact struct {
	DisplayName string                  `json:"display_name,omitempty"`
	Addresses   []sidecarContactAddress `json:"addresses,omitempty"`
	Keys        []sidecarContactKey     `json:"keys,omitempty"`
	// The user's markers (#59). Exported because they are the only state on
	// a contact the USER authored: a block silently lost in a migration would
	// let a sender back into the inbox.
	VIP              bool   `json:"vip,omitempty"`
	Blocked          bool   `json:"blocked,omitempty"`
	BlockDestination string `json:"block_destination,omitempty"`
}

type sidecarSubmission struct {
	// MessageHash names a file in the exported maildir.
	MessageHash  string   `json:"message_content_hash"`
	From         string   `json:"from"`
	EnvelopeTo   []string `json:"envelope_to"`
	State        string   `json:"state"`
	SMTPResponse string   `json:"smtp_response,omitempty"`
	SendAt       string   `json:"send_at"`
	SentAt       string   `json:"sent_at,omitempty"`
}

const rfc3339 = "2006-01-02T15:04:05Z07:00"

// writeConfig writes every configuration sidecar and returns a per-file count.
//
// ⚠ IT REPORTS PER FILE, NOT ONE TOTAL. "12 configuration records exported"
// cannot distinguish "this user has no rules" from "the rules query returned
// nothing because it is broken" — and the second is exactly the failure an
// export must not have. The command prints every line, including the zeros.
func writeConfig(ctx context.Context, st *store.Store, opt Options) (map[string]int, string, error) {
	counts := map[string]int{}

	var (
		account     store.ExportAccount
		idents      []store.Identity
		ruleList    []sidecarRule
		snippets    []store.ComposeSnippet
		searches    []store.SavedSearch
		tabViews    []store.CategoryTabView
		allows      []store.RemoteAllow
		contacts    []store.Contact
		keys        map[string][]store.ExportContactKey
		subs        []store.ExportSubmission
		identAddr   map[string]string
		labelName   map[string]string
		areaViews   []store.AreaView
		deskPrefs   store.DeskPreferences
		clientPrefs []store.ClientPreferences
		oof         []store.OutOfOffice
		acctName    map[string]string
	)

	if err := st.UserTx(ctx, opt.UserID, func(tx *store.UserTx) error {
		var e error
		if account, e = tx.ExportAccountRecord(ctx); e != nil {
			return e
		}
		if idents, e = tx.ListIdentities(ctx); e != nil {
			return e
		}
		identAddr = make(map[string]string, len(idents))
		for _, i := range idents {
			identAddr[i.ID.String()] = i.Email()
		}
		rs, e := tx.ListRules(ctx, false)
		if e != nil {
			return e
		}
		// Label ids appear inside rule ACTIONS and inside Area feeds, so the
		// label set has to be in hand before either can be resolved to names.
		labelName = map[string]string{}
		labels, e := tx.ListLabels(ctx)
		if e != nil {
			return e
		}
		for _, l := range labels {
			labelName[l.ID.String()] = l.Name
		}

		ruleList = make([]sidecarRule, 0, len(rs))
		for _, r := range rs {
			acts := make([]sidecarAction, 0, len(r.Actions))
			for _, a := range r.Actions {
				acts = append(acts, sidecarAction{
					Type:     a.Type,
					Label:    labelName[a.Label], // "" for a non-label action or a dead id
					Mailbox:  a.Mailbox,
					Evidence: a.Evidence,
				})
			}
			ruleList = append(ruleList, sidecarRule{
				Name: r.Name, Priority: r.Priority, Enabled: r.Enabled,
				SystemSuggested: r.SystemSuggested,
				Conditions:      r.Conditions, Actions: acts,
			})
		}
		if snippets, e = tx.ListComposeSnippets(ctx); e != nil {
			return e
		}
		if searches, e = tx.ListSavedSearches(ctx); e != nil {
			return e
		}
		if tabViews, e = tx.ListCategoryTabs(ctx); e != nil {
			return e
		}
		if allows, e = tx.ListRemoteAllows(ctx); e != nil {
			return e
		}
		if contacts, e = tx.ListContacts(ctx); e != nil {
			return e
		}
		raw, e := tx.ExportContactKeys(ctx)
		if e != nil {
			return e
		}
		keys = make(map[string][]store.ExportContactKey, len(raw))
		for id, k := range raw {
			keys[id.String()] = k
		}
		if areaViews, e = tx.ListAreas(ctx); e != nil {
			return e
		}
		if deskPrefs, e = tx.GetDeskPreferences(ctx); e != nil {
			return e
		}
		clientPrefs = make([]store.ClientPreferences, 0, len(exportClientFamilies))
		for _, fam := range exportClientFamilies {
			p, e := tx.GetClientPreferences(ctx, fam)
			if e != nil {
				return e
			}
			clientPrefs = append(clientPrefs, p)
		}
		if oof, e = tx.ListOutOfOffice(ctx); e != nil {
			return e
		}
		accts, e := tx.ListAccounts(ctx)
		if e != nil {
			return e
		}
		acctName = make(map[string]string, len(accts))
		for _, a := range accts {
			acctName[a.ID.String()] = a.Name
		}
		subs, e = tx.ExportSubmissions(ctx)
		return e
	}); err != nil {
		return nil, "", fmt.Errorf("exporter: read configuration: %w", err)
	}

	acct := sidecarAccount{
		Email: account.Email, DisplayName: account.DisplayName,
		CreatedAt: account.CreatedAt.UTC().Format(rfc3339),
		Note: "The sign-in identifier from the mail system this was exported from is " +
			"deliberately not included: it identifies you to that system's login provider " +
			"and means nothing anywhere else.",
	}
	if err := writeJSON(opt.OutDir, "account.json", acct); err != nil {
		return nil, "", err
	}
	counts["account.json"] = 1

	outIdents := make([]sidecarIdentity, 0, len(idents))
	for _, i := range idents {
		outIdents = append(outIdents, sidecarIdentity{
			Email: i.Email(), Kind: string(i.Kind), DisplayName: i.DisplayName,
			SignatureHTML: i.SignatureHTML, Enabled: i.Enabled,
		})
	}
	if err := writeJSON(opt.OutDir, "identities.json", outIdents); err != nil {
		return nil, "", err
	}
	counts["identities.json"] = len(outIdents)

	if err := writeJSON(opt.OutDir, "rules.json", ruleList); err != nil {
		return nil, "", err
	}
	counts["rules.json"] = len(ruleList)

	outSnips := make([]sidecarSnippet, 0, len(snippets))
	for _, s := range snippets {
		e := sidecarSnippet{
			Kind: string(s.Kind), Name: s.Name, BodyHTML: s.BodyHTML,
			BodyText: s.BodyText, IsDefault: s.IsDefault,
		}
		if s.IdentityID != nil {
			// Resolved to the address; a ULID would be a dangling pointer.
			e.Identity = identAddr[s.IdentityID.String()]
		}
		outSnips = append(outSnips, e)
	}
	if err := writeJSON(opt.OutDir, "snippets.json", outSnips); err != nil {
		return nil, "", err
	}
	counts["snippets.json"] = len(outSnips)

	outSearches := make([]sidecarSavedSearch, 0, len(searches))
	for _, s := range searches {
		outSearches = append(outSearches, sidecarSavedSearch{
			Name: s.Name, Query: s.Query, InSidebar: s.InSidebar, Position: s.Position,
		})
	}
	if err := writeJSON(opt.OutDir, "saved_searches.json", outSearches); err != nil {
		return nil, "", err
	}
	counts["saved_searches.json"] = len(outSearches)

	outTabs := make([]sidecarTab, 0, len(tabViews))
	for _, t := range tabViews {
		outTabs = append(outTabs, sidecarTab{
			Tab: string(t.Tab), Enabled: t.Enabled, Position: t.Position,
		})
	}
	if err := writeJSON(opt.OutDir, "tabs.json", outTabs); err != nil {
		return nil, "", err
	}
	counts["tabs.json"] = len(outTabs)

	// areas.json (#442 increment 2): name, tabs, and label NAMES rather than
	// ids — an area's own label and its additional label feeds are resolved
	// through labelName exactly as a rule action's label is above.
	outAreas := make([]sidecarArea, 0, len(areaViews))
	for _, v := range areaViews {
		e := sidecarArea{
			Name: v.Name, Label: labelName[v.LabelID.String()], Position: v.Position,
			AutoArchive: v.AutoArchive, AutoArchiveDays: v.AutoArchiveDays, HiddenFromRail: v.HiddenFromRail,
		}
		for _, f := range v.Feeds {
			switch {
			case f.Tab != "":
				e.Tabs = append(e.Tabs, string(f.Tab))
			case f.LabelID != nil && *f.LabelID != v.LabelID:
				// EXCLUDES the own label — never repeated, matching the wire
				// Area object (area.go's areaObject).
				if name, ok := labelName[f.LabelID.String()]; ok {
					e.Labels = append(e.Labels, name)
				}
			}
		}
		outAreas = append(outAreas, e)
	}
	if err := writeJSON(opt.OutDir, "areas.json", outAreas); err != nil {
		return nil, "", err
	}
	counts["areas.json"] = len(outAreas)

	// desk_preferences.json: a singleton, like account.json — one object, not
	// an array.
	if err := writeJSON(opt.OutDir, "desk_preferences.json", sidecarDeskPreferences{
		LineThreshold: deskPrefs.LineThreshold, DrawerDefaultDays: deskPrefs.DrawerDefaultDays,
		TrayNagDays: deskPrefs.TrayNagDays, DrawerSweepEnabled: deskPrefs.DrawerSweepEnabled,
		LearnedPlacementEnabled: deskPrefs.LearnedPlacementEnabled,
		QuietHours: sidecarQuietHours{
			Enabled: deskPrefs.QuietHours.Enabled, Start: deskPrefs.QuietHours.Start, End: deskPrefs.QuietHours.End,
			Days: deskPrefs.QuietHours.Days, Timezone: deskPrefs.QuietHours.Timezone, AllowVIP: deskPrefs.QuietHours.AllowVIP,
		},
		FollowUp: sidecarFollowUp{
			Outbound: deskPrefs.FollowUp.Outbound, Inbound: deskPrefs.FollowUp.Inbound,
			InboundDays: deskPrefs.FollowUp.InboundDays,
		},
	}); err != nil {
		return nil, "", err
	}
	counts["desk_preferences.json"] = 1

	outClientPrefs := make([]sidecarClientPreferences, 0, len(clientPrefs))
	for _, p := range clientPrefs {
		outClientPrefs = append(outClientPrefs, sidecarClientPreferences{
			Family: string(p.Family), Theme: p.Theme, Density: p.Density,
			MessageOpens: p.MessageOpens, PaneSplitPct: p.PaneSplitPct,
		})
	}
	if err := writeJSON(opt.OutDir, "client_preferences.json", outClientPrefs); err != nil {
		return nil, "", err
	}
	counts["client_preferences.json"] = len(outClientPrefs)

	// out_of_office.json (#50): the user's reply settings per account, by
	// account NAME. Whether it was installed on this deployment's provider
	// (installed_sha256, server_state) does not travel: an importing side has
	// to turn it on against its own server, which is the moment the refusal
	// rules run again.
	outOOF := make([]sidecarOutOfOffice, 0, len(oof))
	for _, o := range oof {
		outOOF = append(outOOF, sidecarOutOfOffice{
			Account: acctName[o.AccountID.String()], Enabled: o.Enabled, From: o.From, To: o.To,
			Subject: o.Subject, Body: o.Body, ReplyDays: o.ReplyDays, FromAddress: o.FromAddress,
			ContactsOnly: o.ContactsOnly,
		})
	}
	if err := writeJSON(opt.OutDir, "out_of_office.json", outOOF); err != nil {
		return nil, "", err
	}
	counts["out_of_office.json"] = len(outOOF)

	outAllows := make([]sidecarRemoteAllow, 0, len(allows))
	for _, a := range allows {
		outAllows = append(outAllows, sidecarRemoteAllow{Kind: string(a.Kind), Value: a.Value})
	}
	if err := writeJSON(opt.OutDir, "remote_allows.json", outAllows); err != nil {
		return nil, "", err
	}
	counts["remote_allows.json"] = len(outAllows)

	outContacts := make([]sidecarContact, 0, len(contacts))
	for _, c := range contacts {
		e := sidecarContact{DisplayName: c.DisplayName, VIP: c.VIP, Blocked: c.Blocked}
		if c.Blocked {
			e.BlockDestination = c.BlockDestination
		}
		for _, a := range c.Addresses {
			addr := sidecarContactAddress{
				Address:   a.Address,
				FirstSeen: a.FirstSeen.UTC().Format(rfc3339),
				LastSeen:  a.LastSeen.UTC().Format(rfc3339),
				SeenCount: a.SeenCount,
			}
			if a.ReachedMeAt != nil {
				addr.ReachedMeAt = *a.ReachedMeAt
			}
			e.Addresses = append(e.Addresses, addr)
		}
		for _, k := range keys[c.ID.String()] {
			e.Keys = append(e.Keys, sidecarContactKey{
				Kind: k.Kind, Fingerprint: k.Fingerprint, Source: k.Source,
				FetchedAt: k.FetchedAt.UTC().Format(rfc3339),
			})
		}
		outContacts = append(outContacts, e)
	}
	if err := writeJSON(opt.OutDir, "contacts.json", outContacts); err != nil {
		return nil, "", err
	}
	counts["contacts.json"] = len(outContacts)

	outSubs := make([]sidecarSubmission, 0, len(subs))
	for _, s := range subs {
		e := sidecarSubmission{
			MessageHash: s.MessageHash, From: s.FromAddress, EnvelopeTo: s.EnvelopeTo,
			State: s.State, SMTPResponse: s.SMTPResponse,
			SendAt: s.SendAt.UTC().Format(rfc3339),
		}
		if s.SentAt != nil {
			e.SentAt = s.SentAt.UTC().Format(rfc3339)
		}
		outSubs = append(outSubs, e)
	}
	if err := writeJSON(opt.OutDir, "submissions.json", outSubs); err != nil {
		return nil, "", err
	}
	counts["submissions.json"] = len(outSubs)

	return counts, account.Email, nil
}
