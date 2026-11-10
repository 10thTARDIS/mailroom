// SPDX-License-Identifier: AGPL-3.0-only

package main

// `mailroom-admin activity` — the operator's view of the Activity records
// (ADR-053, #45). READ-ONLY, and the same store read the web page uses, run once
// per active user under that user's own UserTx: there is no cross-user query,
// no SECURITY DEFINER function and no new grant behind it.
//
// Default: INSTANCE-WIDE COUNTS per kind over the window, never one total, so
// "nothing happened" can be told apart from "this source keeps nothing that
// old". No per-user detail is printed.
//
// With --user ID it prints that user's merged entries, newest first, the same
// shape the web shows: key names, methods, outcomes and object ids. Never
// message content, never a subject.
//
// Out of scope, and said so on every run: a DISABLED or deleted user is not
// listed (the per-user fan-out is ListActiveUserIDs), and instance events with no
// user (the user.hard_delete tombstones) are not readable by the application role.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	"mailroom/internal/textcheck"
)

// activityKindsInOrder is every branch, in the order the report prints them.
var activityKindsInOrder = []string{
	store.ActivityKindAITool, store.ActivityKindAIKey, store.ActivityKindDecryption, store.ActivityKindPurge,
	store.ActivityKindSignIn, store.ActivityKindSignOut, store.ActivityKindPlacement, store.ActivityKindRuleApply,
}

// activityDefaultCap bounds how many entries one user contributes to the counts.
const activityDefaultCap = 5000

func runActivity(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("activity", flag.ContinueOnError)
	since := fs.String("since", "7d", "window start: a duration (24h, 7d) or a date (2026-11-01); default 7d")
	source := fs.String("source", "", "comma-separated sources to include ("+strings.Join(store.ActivitySources, ", ")+"); default all")
	user := fs.String("user", "", "print this one user's entries (a user id) instead of instance-wide counts")
	limit := fs.Int("limit", store.ActivityDefaultLimit, "with --user: how many entries to print (1-100)")
	capN := fs.Int("cap", activityDefaultCap, "instance-wide: the most entries counted per user")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts, err := parseActivityOptions(time.Now(), *since, *source, *user, *limit, *capN)
	if err != nil {
		return err
	}
	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	return activityCore(ctx, d.store, opts, reportOut)
}

type activityOptions struct {
	Since   time.Time
	Sources []string
	User    *ulid.ULID
	Limit   int
	Cap     int
}

func parseActivityOptions(now time.Time, since, source, user string, limit, capN int) (activityOptions, error) {
	o := activityOptions{Limit: limit, Cap: capN}
	t, err := parseActivitySince(now, since)
	if err != nil {
		return o, err
	}
	o.Since = t
	if source != "" {
		for _, s := range strings.Split(source, ",") {
			s = strings.TrimSpace(s)
			ok := false
			for _, k := range store.ActivitySources {
				ok = ok || s == k
			}
			if !ok {
				return o, fmt.Errorf("activity: unknown source %q (want one of %s)", s, strings.Join(store.ActivitySources, ", "))
			}
			o.Sources = append(o.Sources, s)
		}
	}
	if user != "" {
		u, perr := ulid.Parse(user)
		if perr != nil {
			return o, fmt.Errorf("activity: --user must be a user id: %w", perr)
		}
		o.User = &u
	}
	if limit < 1 || limit > store.ActivityMaxLimit {
		return o, fmt.Errorf("activity: --limit must be between 1 and %d", store.ActivityMaxLimit)
	}
	if capN < 1 {
		return o, errors.New("activity: --cap must be at least 1")
	}
	return o, nil
}

// parseActivitySince reads "24h", "7d" or "2026-11-01" relative to now.
func parseActivitySince(now time.Time, s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n < 0 {
			return time.Time{}, fmt.Errorf("activity: --since %q is not a duration or a date", s)
		}
		return now.UTC().Add(-time.Duration(n) * 24 * time.Hour), nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil || dur < 0 {
		return time.Time{}, fmt.Errorf("activity: --since %q is not a duration or a date", s)
	}
	return now.UTC().Add(-dur), nil
}

// activityCore is runActivity after flags and dependencies are resolved, split
// out so an integration test can drive it against a fixture store.
func activityCore(ctx context.Context, st *store.Store, o activityOptions, out io.Writer) error {
	pr := func(f string, a ...any) { _, _ = fmt.Fprintf(out, f, a...) }
	if o.User != nil {
		return activityForUser(ctx, st, o, pr)
	}
	users, err := st.ListActiveUserIDs(ctx)
	if err != nil {
		return fmt.Errorf("activity: list users: %w", err)
	}
	counts := map[string]int{}
	oldest := map[string]*time.Time{}
	withActivity, capped := 0, 0
	for _, uid := range users {
		n, hitCap, err := countUserActivity(ctx, st, uid, o, counts, oldest)
		if err != nil {
			return fmt.Errorf("activity: user %s: %w", uid, err)
		}
		if n > 0 {
			withActivity++
		}
		if hitCap {
			capped++
		}
	}
	pr("=== activity: instance-wide, since %s ===\n", o.Since.UTC().Format(time.RFC3339))
	pr("sources: %s\n\n", sourcesText(o.Sources))
	pr("%-12s %10s  %s\n", "KIND", "ENTRIES", "OLDEST IN WINDOW")
	total := 0
	for _, k := range activityKindsInOrder {
		if len(o.Sources) > 0 && !contains(o.Sources, store.ActivitySourceOf(k)) {
			continue
		}
		old := "-"
		if t := oldest[k]; t != nil {
			old = t.UTC().Format("2006-01-02 15:04")
		}
		pr("%-12s %10d  %s\n", k, counts[k], old)
		total += counts[k]
	}
	pr("\nusers scanned: %d · with entries in the window: %d\n", len(users), withActivity)
	pr("entries counted: %d\n", total)
	if capped > 0 {
		pr("⚠ %d user(s) hit the --cap of %d entries: their counts are a LOWER BOUND, not a total.\n", capped, o.Cap)
	}
	if len(users) == 0 {
		pr("⚠ NO ACTIVE USERS. An instance with users always has some; zero means this process cannot see them.\n")
	}
	pr("\nNot included: disabled or deleted users, and instance events that belong to no user\n")
	pr("(user.hard_delete tombstones), which the application role cannot read.\n")
	pr("Sign-ins are only as old as the session table keeps them (a session is swept a week after it ends).\n")
	pr("REPORT ONLY: nothing was written. Use --user ID for one user's entries.\n")
	return nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func sourcesText(s []string) string {
	if len(s) == 0 {
		return "all"
	}
	return strings.Join(s, ", ")
}

// countUserActivity pages through one user's window and adds to counts. It
// reports how many entries it counted and whether it stopped at the cap.
func countUserActivity(ctx context.Context, st *store.Store, uid ulid.ULID, o activityOptions,
	counts map[string]int, oldest map[string]*time.Time) (int, bool, error) {
	// A page never exceeds what is left of the cap, so the cap is a real bound.
	f := store.ActivityFilter{Sources: o.Sources, Since: o.Since, Limit: min(store.ActivityMaxLimit, o.Cap)}
	n := 0
	for {
		// Re-bounded every page: the last page asks only for what is left of
		// the cap (a fixed limit let --cap 150 count 200).
		f.Limit = min(store.ActivityMaxLimit, o.Cap-n)
		var page store.ActivityPage
		if err := st.UserTx(ctx, uid, func(tx *store.UserTx) error {
			var e error
			page, e = tx.ListActivity(ctx, f)
			return e
		}); err != nil {
			return n, false, err
		}
		for _, e := range page.Entries {
			counts[e.Kind]++
			if t := oldest[e.Kind]; t == nil || e.At.Before(*t) {
				at := e.At
				oldest[e.Kind] = &at
			}
			n++
		}
		if page.Next == "" {
			return n, false, nil
		}
		if n >= o.Cap {
			return n, true, nil
		}
		c, err := store.ParseActivityCursor(page.Next)
		if err != nil {
			return n, false, err
		}
		f.After = &c
	}
}

// activityForUser prints one user's newest entries: no content, only the facts
// the web shows.
func activityForUser(ctx context.Context, st *store.Store, o activityOptions, pr func(string, ...any)) error {
	var page store.ActivityPage
	if err := st.UserTx(ctx, *o.User, func(tx *store.UserTx) error {
		var e error
		page, e = tx.ListActivity(ctx, store.ActivityFilter{Sources: o.Sources, Since: o.Since, Limit: o.Limit})
		return e
	}); err != nil {
		return fmt.Errorf("activity: %w", err)
	}
	pr("=== activity: user %s, since %s ===\n", o.User, o.Since.UTC().Format(time.RFC3339))
	pr("sources: %s\n\n", sourcesText(o.Sources))
	for _, e := range page.Entries {
		pr("%s  %-11s %s\n", e.At.UTC().Format("2006-01-02 15:04:05"), e.Kind, activityDetail(e))
	}
	if len(page.Entries) == 0 {
		pr("(no entries in the window)\n")
	}
	if page.Next != "" {
		pr("\nshowing the %d most recent; there are older entries in the window (raise --limit, up to %d, or narrow --since).\n",
			len(page.Entries), store.ActivityMaxLimit)
	} else {
		pr("\n%d entries: everything the records hold for this window.\n", len(page.Entries))
	}
	srcs := make([]string, 0, len(page.Covers))
	for s := range page.Covers {
		srcs = append(srcs, s)
	}
	sort.Strings(srcs)
	pr("oldest record per source:")
	for _, s := range srcs {
		if t := page.Covers[s]; t != nil {
			pr(" %s %s;", s, t.UTC().Format("2006-01-02"))
		} else {
			pr(" %s none;", s)
		}
	}
	pr("\nREPORT ONLY: nothing was written.\n")
	return nil
}

// activityDetail is the one-line fact for an entry. Names are the user's own
// key and rule names; nothing here is message content.
func activityDetail(e store.ActivityEntry) string {
	var p []string
	add := func(k, v string) {
		if v != "" {
			p = append(p, k+"="+safeText(v))
		}
	}
	add("outcome", e.Outcome)
	add("name", e.KeyName)
	add("method", e.Method)
	add("reason", e.Reason)
	add("purpose", e.Purpose)
	if e.Fingerprint != "" {
		add("key", e.Fingerprint[max(0, len(e.Fingerprint)-8):])
	}
	if e.Count > 0 {
		add("count", strconv.Itoa(e.Count))
	}
	if e.ObjectType != "" {
		state := "gone"
		if e.ObjectExists {
			state = "present"
		}
		add("object", e.ObjectType+":"+e.ObjectID+" ("+state+")")
	}
	if len(p) == 0 {
		return safeText(e.ID)
	}
	return strings.Join(p, " ")
}

// safeText makes user-supplied text safe to print to an operator's terminal. A key
// or rule name is typed by a user, and an ANSI escape, any C0/C1 control (newline
// and tab included, so one entry stays one line), a bidi override or another
// format (Cf) or default-ignorable character could rewrite the screen or hide
// entries. Each such rune becomes U+FFFD, a visible placeholder, never silence.
func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		// textcheck.IsUnsafeRune is the class a name is refused for at save time;
		// the terminal adds the invisible characters that are only worth
		// replacing, not refusing.
		if r == utf8.RuneError || textcheck.IsUnsafeRune(r) ||
			unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) || unicode.Is(unicode.Variation_Selector, r) {
			return '\uFFFD'
		}
		return r
	}, s)
}
