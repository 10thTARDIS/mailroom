// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
	"mailroom/internal/store"
)

// Rule method handlers for the `urn:mailroom:rules` extension (spec §8, T0 rules
// engine, mailroom#32). A Rule is a canonical object in Postgres — a
// deterministic {conditions → actions} filter the ingest path evaluates on new
// mail. This extension exposes the rule metadata itself (name, priority,
// conditions, actions, enabled, systemSuggested); the effect of a rule shows up
// on the Email objects it labels/flags/diverts, through the core surface.
//
// Extensions add methods but NEVER change core method semantics (spec §13), so
// Rule/get|query|changes|set live entirely alongside the RFC surface, modelled
// exactly on Label/* (label.go): same shapes, same set-error handling, same
// shared /changes handler.
//
// Registration: the four methods are wired into the dispatch map in service.go
// and gated on capRules in capability.go; the capability is advertised in the
// session via capabilityObjects().

// ruleObject renders a stored rule as a JMAP Rule object, honoring a requested
// property subset (id is always present). conditions/actions serialize straight
// from the stored jsonb model.
func ruleObject(r rules.Rule, corrections int, props map[string]bool) map[string]any {
	full := map[string]any{
		"id":              r.ID.String(),
		"name":            r.Name,
		"priority":        r.Priority,
		"conditions":      r.Conditions,
		"actions":         r.Actions,
		"enabled":         r.Enabled,
		"systemSuggested": r.SystemSuggested,
		// How many messages the user has taken this rule's label back off (#170).
		//
		// A rule that keeps being corrected is a rule that is wrong, and until now
		// nothing recorded that: removing a label deleted the row and its
		// attribution together, so the rule kept firing and the disagreement went
		// nowhere. This is the number that lets a client say "this has misfiled 2
		// messages — narrow it?" instead of leaving the user to work out which of
		// seventy-three rules did it.
		//
		// Not opt-in like the Email extension properties: a rule's whole purpose is
		// to act unsupervised, so how often it has been overruled belongs with the
		// rest of its state rather than behind a capability a client might not ask
		// for.
		"corrections": corrections,
	}
	if props == nil {
		return full
	}
	out := map[string]any{"id": r.ID.String()}
	for k, v := range full {
		if props[k] {
			out[k] = v
		}
	}
	return out
}

// ---- Rule/get -------------------------------------------------------------

type ruleGetArgs struct {
	AccountID  string    `json:"accountId"`
	IDs        *[]string `json:"ids"`
	Properties []string  `json:"properties"`
}

func (a *API) ruleGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args ruleGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	var props map[string]bool
	if args.Properties != nil {
		props = make(map[string]bool, len(args.Properties))
		for _, p := range args.Properties {
			props[p] = true
		}
	}

	var (
		state       int64
		byID        = make(map[ulid.ULID]rules.Rule)
		corrections = map[ulid.ULID]int{}
		requested   []requestedID
		notFound    = []string{}
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var views []rules.Rule
		var err error
		if args.IDs == nil {
			// ids:null means "all rules" (RFC 8620 §5.1).
			views, err = tx.ListRules(ctx, false)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			for _, v := range views {
				requested = append(requested, requestedID{raw: v.ID.String(), id: v.ID, ok: true})
			}
		} else {
			var valid []ulid.ULID
			valid, notFound, requested = parseIDs(*args.IDs)
			views, err = tx.GetRules(ctx, valid)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
		}
		for _, v := range views {
			byID[v.ID] = v
		}
		// One grouped count for the whole response rather than a query per rule:
		// a rules list is read as a list, and seventy-three round trips to render
		// one column would be the obvious thing and the wrong one.
		corrections, err = tx.CountRuleCorrections(ctx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		state, err = tx.ChangeState(ctx, store.ObjectRule)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}

	list := make([]map[string]any, 0, len(requested))
	for _, req := range requested {
		if !req.ok {
			continue
		}
		rv, ok := byID[req.id]
		if !ok {
			notFound = append(notFound, req.raw)
			continue
		}
		list = append(list, ruleObject(rv, corrections[rv.ID], props))
	}
	return mustMarshal(getResult{
		AccountID: userID.String(),
		State:     encodeState(state),
		List:      list,
		NotFound:  notFound,
	}), nil
}

// ---- Rule/query -----------------------------------------------------------

// ruleQuery returns every rule id in the stable priority order the store
// defines. Filtering/sorting is not offered (the set is small); the response is
// a well-formed Foo/query the client windows itself.
func (a *API) ruleQuery(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var probe struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(rawArgs, &probe); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, probe.AccountID); mErr != nil {
		return nil, mErr
	}

	var (
		ids   []ulid.ULID
		state int64
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		views, err := tx.ListRules(ctx, false)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		ids = make([]ulid.ULID, len(views))
		for i, v := range views {
			ids[i] = v.ID
		}
		state, err = tx.ChangeState(ctx, store.ObjectRule)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(queryResult{
		AccountID:           userID.String(),
		QueryState:          encodeState(state),
		CanCalculateChanges: false,
		Position:            0,
		Total:               len(ids),
		IDs:                 ulidStrings(ids),
	}), nil
}

// ---- Rule/changes ---------------------------------------------------------

func (a *API) ruleChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectRule)
}

// ---- Rule/set -------------------------------------------------------------

// ruleCreate is the parsed create form. name/conditions/actions are required;
// priority defaults to 100 (matching the schema), enabled to true.
type ruleCreate struct {
	Name            *string          `json:"name"`
	Priority        *int             `json:"priority"`
	Conditions      *json.RawMessage `json:"conditions"`
	Actions         *json.RawMessage `json:"actions"`
	Enabled         *bool            `json:"enabled"`
	SystemSuggested *bool            `json:"systemSuggested"`
}

func (a *API) ruleSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args emailSetArgs // shared {accountId, ifInState, create, update, destroy} shape
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	res := setResult{
		AccountID:    userID.String(),
		Created:      map[string]any{},
		Updated:      map[string]any{},
		Destroyed:    []string{},
		NotCreated:   map[string]SetError{},
		NotUpdated:   map[string]SetError{},
		NotDestroyed: map[string]SetError{},
	}

	var stateErr *MethodError
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		oldState, err := tx.ChangeState(ctx, store.ObjectRule)
		if err != nil {
			return err
		}
		res.OldState = encodeState(oldState)
		if args.IfInState != nil && *args.IfInState != encodeState(oldState) {
			stateErr = &MethodError{Type: errStateMismatch}
			return errMethodRollback
		}

		created := map[string]ulid.ULID{}

		for cid, raw := range args.Create {
			obj, id, serr := createRule(ctx, tx, raw)
			if serr != nil {
				res.NotCreated[cid] = *serr
				continue
			}
			created[cid] = id
			res.Created[cid] = obj
		}

		for id, patch := range args.Update {
			rid, ok := resolveSetID(id, created)
			if !ok {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			if serr := updateRule(ctx, tx, rid, patch); serr != nil {
				res.NotUpdated[id] = *serr
				continue
			}
			res.Updated[id] = nil
		}

		for _, id := range args.Destroy {
			rid, ok := resolveSetID(id, created)
			if !ok {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			found, derr := tx.DeleteRule(ctx, rid)
			if derr != nil {
				return derr
			}
			if !found {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			res.Destroyed = append(res.Destroyed, id)
		}

		newState, err := tx.ChangeState(ctx, store.ObjectRule)
		if err != nil {
			return err
		}
		res.NewState = encodeState(newState)
		return nil
	})
	if stateErr != nil {
		return nil, stateErr
	}
	if err != nil && err != errMethodRollback {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}
	return mustMarshal(res), nil
}

// createRule parses a Rule/set create patch and inserts the rule, returning the
// created JMAP object (with its server-assigned id) on success. A bad regex or
// an unknown/unsupported action is rejected by the store as ErrInvalidRule and
// surfaced as a clean invalidProperties SetError.
func createRule(ctx context.Context, tx *store.UserTx, raw json.RawMessage) (map[string]any, ulid.ULID, *SetError) {
	var c ruleCreate
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps, Description: err.Error()}
	}
	if c.Name == nil || *c.Name == "" {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
			Description: "rule name is required", Properties: []string{"name"}}
	}
	if c.Conditions == nil {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
			Description: "rule conditions are required", Properties: []string{"conditions"}}
	}
	if c.Actions == nil {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
			Description: "rule actions are required", Properties: []string{"actions"}}
	}
	conds, err := rules.ParseConditions(*c.Conditions)
	if err != nil {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
			Description: err.Error(), Properties: []string{"conditions"}}
	}
	acts, err := rules.ParseActions(*c.Actions)
	if err != nil {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
			Description: err.Error(), Properties: []string{"actions"}}
	}

	row := store.RuleRow{Name: *c.Name, Priority: 100, Enabled: true, Conditions: conds, Actions: acts}
	if c.Priority != nil {
		row.Priority = *c.Priority
	}
	if c.Enabled != nil {
		row.Enabled = *c.Enabled
	}
	if c.SystemSuggested != nil {
		row.SystemSuggested = *c.SystemSuggested
	}

	id, err := tx.CreateRule(ctx, row)
	if err != nil {
		return nil, ulid.ULID{}, ruleSetError(err)
	}
	views, err := tx.GetRules(ctx, []ulid.ULID{id})
	if err != nil || len(views) == 0 {
		return map[string]any{"id": id.String()}, id, nil
	}
	// A rule created a moment ago has no corrections by construction.
	return ruleObject(views[0], 0, nil), id, nil
}

// updateRule parses a Rule/set update patch and applies it. It accepts the
// top-level Rule properties; the server-set id is rejected as immutable.
func updateRule(ctx context.Context, tx *store.UserTx, id ulid.ULID, patch json.RawMessage) *SetError {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(patch, &raw); err != nil {
		return &SetError{Type: setInvalidPatch, Description: err.Error()}
	}
	var p store.RulePatch
	for key, val := range raw {
		switch key {
		case "name":
			var s string
			if err := json.Unmarshal(val, &s); err != nil || s == "" {
				return &SetError{Type: setInvalidProps,
					Description: "name must be a non-empty string", Properties: []string{"name"}}
			}
			p.Name = &s
		case "priority":
			var n int
			if err := json.Unmarshal(val, &n); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "priority must be an integer", Properties: []string{"priority"}}
			}
			p.Priority = &n
		case "conditions":
			conds, err := rules.ParseConditions(val)
			if err != nil {
				return &SetError{Type: setInvalidProps,
					Description: err.Error(), Properties: []string{"conditions"}}
			}
			p.Conditions = &conds
		case "actions":
			acts, err := rules.ParseActions(val)
			if err != nil {
				return &SetError{Type: setInvalidProps,
					Description: err.Error(), Properties: []string{"actions"}}
			}
			p.Actions = &acts
		case "enabled":
			var b bool
			if err := json.Unmarshal(val, &b); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "enabled must be a boolean", Properties: []string{"enabled"}}
			}
			p.Enabled = &b
		case "systemSuggested":
			var b bool
			if err := json.Unmarshal(val, &b); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "systemSuggested must be a boolean", Properties: []string{"systemSuggested"}}
			}
			p.SystemSuggested = &b
		case "id":
			return &SetError{Type: setInvalidProps,
				Description: "id is immutable", Properties: []string{"id"}}
		case "corrections":
			// Named rather than left to the default branch so the error says what
			// is actually wrong: corrections are the record of what the user took
			// back from this rule, and a client cannot edit that any more than it
			// can edit its own history. To forget them, fix the rule.
			return &SetError{Type: setInvalidProps,
				Description: "corrections is server-set", Properties: []string{"corrections"}}
		default:
			return &SetError{Type: setInvalidProps,
				Description: "unsupported Rule property in patch: " + key, Properties: []string{key}}
		}
	}
	// Changing what a rule MATCHES retires the evidence against it (#170). The
	// corrections describe the rule as it was; the messages they name were wrong
	// for the old conditions, and carrying them over would keep accusing a rule
	// the user has just fixed — the surface would say "misfiled 4" forever and the
	// suggestion to narrow would never go away.
	//
	// Deliberately narrower than "any update": renaming, reprioritising or
	// disabling a rule leaves it matching exactly the same mail, so the record
	// still describes it. Disabling in particular is a user working AROUND the
	// problem rather than fixing it, and forgetting there was one would be wrong.
	//
	// AND NARROWER STILL — the conditions must actually DIFFER, not merely be
	// present in the patch. Our own editor sends the whole rule on every save, so
	// "did the client mention conditions" would mean renaming a rule silently
	// wiped the record of what it had misfiled. Read before write; compare.
	clear := false
	if p.Conditions != nil {
		before, err := tx.GetRules(ctx, []ulid.ULID{id})
		if err != nil {
			return &SetError{Type: setForbidden, Description: err.Error()}
		}
		if len(before) == 1 && conditionJSON(before[0].Conditions) != conditionJSON(*p.Conditions) {
			clear = true
		}
	}

	found, err := tx.UpdateRule(ctx, id, p)
	if err != nil {
		return ruleSetError(err)
	}
	if !found {
		return &SetError{Type: setNotFound}
	}
	if clear {
		if _, err := tx.ClearRuleCorrections(ctx, id); err != nil {
			return &SetError{Type: setForbidden, Description: err.Error()}
		}
	}
	return nil
}

// ruleSetError maps a store rule error to the per-object SetError a Rule/set
// reports: a name collision and a validation failure are both invalidProperties
// (with the offending property named); anything else is forbidden.
func ruleSetError(err error) *SetError {
	switch {
	case errors.Is(err, store.ErrRuleNameTaken):
		return &SetError{Type: setInvalidProps,
			Description: "a rule with that name already exists", Properties: []string{"name"}}
	case errors.Is(err, store.ErrInvalidRule):
		return &SetError{Type: setInvalidProps,
			Description: err.Error(), Properties: []string{"conditions", "actions"}}
	default:
		return &SetError{Type: setForbidden, Description: err.Error()}
	}
}
