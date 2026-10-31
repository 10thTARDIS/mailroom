// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"sync/atomic"

	"github.com/oklog/ulid/v2"
)

// writebackNudgeHook carries the send worker's "a submission was just sent"
// signal to the sync runner's writeback nudge (#691).
//
// It is an indirection rather than a direct reference because of start-up
// order: the job dispatcher (and so the SendWorker) is started BEFORE the
// per-user fan-out builds the syncrunner.Runner, and the dispatcher's goroutine
// may complete a send while the fan-out is still being built. A plain variable
// assigned later would be a data race; this is an atomic pointer.
//
// Until set is called, fire does nothing. That is the pre-#691 behaviour for a
// send completing in the first instants of boot: its intents drain at the
// account's next pass, which the boot catch-up pass is about to run anyway.
type writebackNudgeHook struct {
	f atomic.Pointer[func(ulid.ULID)]
}

// set installs the nudge. Called once, where the runner is built.
func (h *writebackNudgeHook) set(f func(ulid.ULID)) { h.f.Store(&f) }

// fire nudges userID's writeback drain if a nudge has been installed.
func (h *writebackNudgeHook) fire(userID ulid.ULID) {
	if f := h.f.Load(); f != nil {
		(*f)(userID)
	}
}
