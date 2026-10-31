// SPDX-License-Identifier: AGPL-3.0-only

// Package jobs is the durable job dispatcher (spec §16, mailroom#13): the
// single runtime over app.jobs and the SystemTx job functions. One dispatcher
// claims due timers (dequeue_due_jobs), runs a registered handler per job kind,
// and transitions the job to done (complete_job) or, on error, retries with a
// backoff or moves it to the surfaced dead-letter state (fail_job).
//
// "Catch-up on boot" falls out of the design: dequeue returns every job whose
// fire_at has passed, so the first pass after start drains all overdue timers.
// Retries are bounded by the job's max_attempts; exhausted jobs become 'dead'
// and are surfaced (logged + metric), never silently dropped.
//
// The dispatcher depends on a small Queue interface, not *store.Store directly,
// so its logic is unit-testable with no database; StoreQueue is the production
// adapter that runs each operation in a SystemTx.
package jobs
