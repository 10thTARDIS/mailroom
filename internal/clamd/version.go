// SPDX-License-Identifier: AGPL-3.0-only

package clamd

// What the daemon knows, and when it learned it (#395, ADR-027 §5c).
//
// # Why this exists at all
//
// A `clean` verdict is not a property of a file. It is a statement about what
// the signatures knew at the moment it was made, which is why §5c re-scans on a
// cadence and why app.attachments stamps the version it was scanned against.
// This is where that version comes from.
//
// # ⚠ The failure this serves is the one that makes NO noise
//
// §5c gates every re-scan tier on "only if the database moved". That is an
// optimisation which introduces the failure it was meant to prevent: when
// freshclam quietly stops updating — a CDN rate-limit, a full disk, an expired
// certificate — the version never changes, so the entire re-scan system
// suspends itself with no signal at all. clamd keeps answering. Scans keep
// succeeding. The age shown to the user keeps ADVANCING, because a scan really
// did run; it just ran against a database frozen in the past.
//
// So the publication timestamp is not decoration. "The database has not moved"
// and "the database cannot move" are the same observation until you look at how
// old it is, and only one of them is fine.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Version is what the daemon reports about itself.
type Version struct {
	// Engine is clamd's own version, e.g. "1.5.4". ⚠ Kept because §5c requires
	// a re-scan on an engine MAJOR version change and not only on signatures: a
	// newer engine can unpack a container an older one could not, so the same
	// bytes and the same signatures can legitimately yield a different verdict.
	Engine string
	// Database is the signature database version, e.g. 28110. This is the
	// number the re-scan predicate compares against.
	Database int
	// Published is when that database was built. Zero when clamd did not report
	// it — see Age, which is the only thing that reads it.
	Published time.Time
}

// Age is how long ago the loaded database was published.
//
// ⚠ It returns ok=false rather than a zero duration when the timestamp is
// missing, because "no age" and "age zero" are opposite answers here: one means
// we cannot tell whether the scanner has rotted, the other means it is perfectly
// fresh. A caller that treats them alike reimplements the fail-open bug.
func (v Version) Age(now time.Time) (time.Duration, bool) {
	if v.Published.IsZero() {
		return 0, false
	}
	return now.Sub(v.Published), true
}

// MaxDatabaseAge is when a signature database stops being credible.
//
// ⚠ §5c: "surface DB age as account health beyond ~3 days; treat 'DB older than
// the active tier's period' as a FAULT, not a no-op". freshclam checks twice a
// day, so three days is roughly six missed checks — comfortably past a transient
// network problem and well short of a database anyone should be scanning
// against. Judgment, and stated as such rather than derived.
const MaxDatabaseAge = 72 * time.Hour

// Stale reports whether the database is old enough to be a fault.
//
// ⚠ An UNKNOWN publication time counts as stale. That is the fail-closed
// direction and it is deliberate: a daemon that will not say how old its
// database is has not told us it is fresh.
func (v Version) Stale(now time.Time) bool {
	age, ok := v.Age(now)
	return !ok || age > MaxDatabaseAge
}

// Version asks the daemon what it is running.
//
// ⚠ MEASURED, NOT DOCUMENTED. Against the deployed clamav/clamav:1.5 daemon,
// 2026-09-01, the reply is one line with three slash-separated fields:
//
//	ClamAV 1.5.4/28110/Tue Sep  1 06:26:54 2026
//	       engine  db      publication timestamp
//
// It is deliberately TOLERANT of a shorter reply. clamd answers plain
// "ClamAV 1.5.4" when no database is loaded, and that must parse — a daemon
// with no signatures at all is exactly the situation this call exists to make
// visible, so failing to parse it would hide the worst case.
func (c *Client) Version(ctx context.Context) (Version, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultPingTimeout)
	defer cancel()

	conn, err := c.dial(ctx, DefaultPingTimeout)
	if err != nil {
		return Version{}, fmt.Errorf("%w (version dial: %v)", ErrDaemonUnreachable, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(DefaultPingTimeout))
	if _, err := conn.Write([]byte("zVERSION\x00")); err != nil {
		return Version{}, fmt.Errorf("%w (version write: %v)", ErrDaemonUnreachable, err)
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		return Version{}, fmt.Errorf("%w (version read: %v)", ErrDaemonUnreachable, err)
	}
	return parseVersion(string(buf[:n]))
}

// parseVersion maps clamd's reply to Version.
func parseVersion(resp string) (Version, error) {
	resp = strings.TrimSpace(strings.TrimRight(resp, "\x00\n"))
	if resp == "" {
		return Version{}, fmt.Errorf("clamd: empty VERSION reply")
	}
	fields := strings.Split(resp, "/")

	// Field 0 is "ClamAV <engine>". A daemon that answered something else
	// entirely is not one we can reason about.
	head := strings.TrimSpace(fields[0])
	if !strings.HasPrefix(head, "ClamAV") {
		return Version{}, fmt.Errorf("clamd: unrecognised VERSION reply %q", resp)
	}
	v := Version{Engine: strings.TrimSpace(strings.TrimPrefix(head, "ClamAV"))}

	if len(fields) > 1 {
		// ⚠ A database version we cannot parse is left at zero rather than
		// erroring. Zero is younger than every real version, so the re-scan
		// predicate treats such rows as needing a scan — which is the safe
		// direction, and better than refusing to scan at all because a version
		// string changed shape.
		if db, err := strconv.Atoi(strings.TrimSpace(fields[1])); err == nil {
			v.Database = db
		}
	}
	if len(fields) > 2 {
		// clamd's own format: "Tue Sep  1 06:26:54 2026", with a DOUBLE SPACE
		// before a single-digit day. "_2" is Go's reference spelling for a
		// space-padded day, so this layout describes the input exactly.
		//
		// ⚠ IT IS NOT LOAD-BEARING, AND AN EARLIER VERSION OF THIS COMMENT SAID
		// IT WAS — that "2" here would fail on nine days out of ten. Probed
		// both layouts against both a single- and a double-digit day: all four
		// combinations parse, because time.Parse tolerates the extra space. So
		// "_2" is the honest description of the format and nothing more; do not
		// treat a change here as a bug fix in either direction.
		ts := strings.TrimSpace(fields[2])
		if t, err := time.Parse("Mon Jan _2 15:04:05 2006", ts); err == nil {
			v.Published = t.UTC()
		}
	}
	return v, nil
}
