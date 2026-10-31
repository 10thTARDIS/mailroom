// SPDX-License-Identifier: AGPL-3.0-only

package outofoffice

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"mailroom/internal/sieve"
)

// Writer is the ManageSieve surface the out-of-office sequences need
// (*sieve.WriteSession implements it; tests substitute a recorder).
type Writer interface {
	Capabilities() sieve.Capabilities
	ListScripts(ctx context.Context) ([]sieve.Script, error)
	GetScript(ctx context.Context, name string) (string, error)
	CheckScript(ctx context.Context, body string) error
	PutScript(ctx context.Context, name, body string) error
	SetActive(ctx context.Context, name string) error
	DeleteScript(ctx context.Context, name string) error
	Logout(ctx context.Context) error
	Close() error
}

var _ Writer = (*sieve.WriteSession)(nil)

// ForeignActiveError is the refusal: a script Mailroom did not write is the
// active one, and the provider runs only one. Activating ours would switch it
// off, so Mailroom does not.
type ForeignActiveError struct{ Script string }

func (e *ForeignActiveError) Error() string {
	return fmt.Sprintf("Out of office was not turned on: the filter script %q is active on your mail server, "+
		"and the server runs only one script at a time. Turning on the reply would switch %q off. "+
		"Deactivate it in your provider's webmail first, or leave out of office off.",
		sieve.Printable(e.Script), sieve.Printable(e.Script))
}

// activeOther returns the name of the active script if it is not ours.
func activeOther(scripts []sieve.Script) string {
	for _, sc := range scripts {
		if sc.Active && sc.Name != ScriptName {
			return sc.Name
		}
	}
	return ""
}

func find(scripts []sieve.Script, name string) (sieve.Script, bool) {
	for _, sc := range scripts {
		if sc.Name == name {
			return sc, true
		}
	}
	return sieve.Script{}, false
}

// Install is the enable sequence (#50):
//
//  1. LISTSCRIPTS: if any script other than ours is active, refuse.
//  2. CHECKSCRIPT: the server compiles the script without storing it.
//  3. PUTSCRIPT mailroom-vacation: stored, not yet active.
//  4. LISTSCRIPTS again: if something else became active meanwhile, take our
//     just-stored script back out (if it was not there before) and refuse.
//  5. SETACTIVE mailroom-vacation.
//
// Inactive scripts that are not ours are never read, written or deleted.
func Install(ctx context.Context, w Writer, script string) error {
	before, err := w.ListScripts(ctx)
	if err != nil {
		return err
	}
	if other := activeOther(before); other != "" {
		return &ForeignActiveError{Script: other}
	}
	_, existed := find(before, ScriptName)
	if err := w.CheckScript(ctx, script); err != nil {
		return fmt.Errorf("the mail server rejected the reply script: %w", err)
	}
	if err := w.PutScript(ctx, ScriptName, script); err != nil {
		return fmt.Errorf("the mail server would not store the reply script: %w", err)
	}
	after, err := w.ListScripts(ctx)
	if err != nil {
		return err
	}
	if other := activeOther(after); other != "" {
		if !existed {
			// Ours is inactive (another script is active), so it can go.
			_ = w.DeleteScript(ctx, ScriptName)
		}
		return &ForeignActiveError{Script: other}
	}
	return w.SetActive(ctx, ScriptName)
}

// RemoveOutcome says what Remove found and did.
type RemoveOutcome int

const (
	// RemovedNothing: our script was not on the server.
	RemovedNothing RemoveOutcome = iota
	// RemovedActive: ours was active; nothing is active now, and ours is gone.
	RemovedActive
	// RemovedInactive: ours was inactive with no script active; it is gone.
	RemovedInactive
	// RemovedLeftOther: another script had been activated since; it was left
	// active, and only our inactive script was deleted.
	RemovedLeftOther
)

// Remove is the disable sequence. There was no active script before Mailroom
// installed its own (Install refuses otherwise), so turning ours off restores
// exactly that: SETACTIVE "" and then DELETESCRIPT. If the user has activated
// another script since, it is left alone and only our inactive script goes.
// It returns the other script's name for RemovedLeftOther.
func Remove(ctx context.Context, w Writer) (RemoveOutcome, string, error) {
	scripts, err := w.ListScripts(ctx)
	if err != nil {
		return RemovedNothing, "", err
	}
	ours, ok := find(scripts, ScriptName)
	if !ok {
		return RemovedNothing, activeOther(scripts), nil
	}
	if ours.Active {
		if err := w.SetActive(ctx, ""); err != nil {
			return RemovedNothing, "", err
		}
		if err := w.DeleteScript(ctx, ScriptName); err != nil {
			return RemovedActive, "", fmt.Errorf("the reply was switched off but its script could not be deleted: %w", err)
		}
		return RemovedActive, "", nil
	}
	if err := w.DeleteScript(ctx, ScriptName); err != nil {
		return RemovedNothing, "", err
	}
	if other := activeOther(scripts); other != "" {
		return RemovedLeftOther, other, nil
	}
	return RemovedInactive, "", nil
}

// ServerState is what Inspect found, as stored and shown.
type ServerState string

const (
	StateOff       ServerState = "off"       // nothing of ours installed (as expected when disabled)
	StateActive    ServerState = "active"    // ours, active, exactly what we installed
	StateDisplaced ServerState = "displaced" // ours installed, but another script is active
	StateMissing   ServerState = "missing"   // we installed it, and it is gone or inactive
	StateModified  ServerState = "modified"  // ours active, but its body is not what we installed
	StateError     ServerState = "error"     // the server could not be asked
)

// Inspection is the server's view of our script.
type Inspection struct {
	State ServerState
	// Other is the active script's name when it is not ours.
	Other string
}

// Inspect compares the server with what Mailroom installed (wantSHA nil:
// nothing installed). It reads only our own script's body.
func Inspect(ctx context.Context, w Writer, wantSHA []byte) (Inspection, error) {
	scripts, err := w.ListScripts(ctx)
	if err != nil {
		return Inspection{}, err
	}
	other := activeOther(scripts)
	ours, ok := find(scripts, ScriptName)
	switch {
	case wantSHA == nil && !ok:
		return Inspection{State: StateOff, Other: other}, nil
	case wantSHA == nil:
		// A script of ours exists that the database does not know about (a
		// crash between PUTSCRIPT and the database write): drift.
		if ours.Active {
			return Inspection{State: StateModified, Other: other}, nil
		}
		return Inspection{State: StateMissing, Other: other}, nil
	case other != "":
		return Inspection{State: StateDisplaced, Other: other}, nil
	case !ok || !ours.Active:
		return Inspection{State: StateMissing}, nil
	}
	body, err := w.GetScript(ctx, ScriptName)
	if err != nil {
		var se *sieve.ServerError
		if errors.As(err, &se) {
			return Inspection{State: StateMissing}, nil
		}
		return Inspection{}, err
	}
	sum := sha256.Sum256([]byte(body))
	if string(sum[:]) != string(wantSHA) {
		return Inspection{State: StateModified}, nil
	}
	return Inspection{State: StateActive}, nil
}
