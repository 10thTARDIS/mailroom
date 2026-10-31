// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"

	"github.com/oklog/ulid/v2"
)

// A send completing before the runner is built must be a no-op, not a nil
// call; once set, every fire reaches the nudge with the user it was given.
func TestWritebackNudgeHookFiresOnlyOnceSet(t *testing.T) {
	var h writebackNudgeHook
	user := ulid.Make()

	h.fire(user) // before set: must not panic, must not be remembered

	var got []ulid.ULID
	h.set(func(u ulid.ULID) { got = append(got, u) })
	h.fire(user)

	if len(got) != 1 || got[0] != user {
		t.Fatalf("nudged %v, want exactly [%s] — the pre-set fire must be dropped and the post-set one delivered", got, user)
	}
}
