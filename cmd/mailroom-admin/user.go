// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"mailroom/internal/store"
)

// `mailroom-admin user disable <user>` / `user enable <user>` — switch an
// ordinary user off and on again, softly and reversibly.
//
// WHAT DISABLED MEANS. Everything that decides "may this user use the service"
// reads app.users.disabled_at and nothing else, so one column is the single
// authority and nothing is deleted:
//
//   - sign-in is refused (app.upsert_login returns no id);
//   - every session is refused on its next request (app.session_for_token) and an
//     open event stream ends at its next re-check;
//   - API keys and OAuth access and refresh tokens are refused on every request
//     (app.api_key_for_token, app.oauth_access_for_token, app.oauth_refresh_consume);
//     the keys and grants are KEPT, so enabling the user restores them;
//   - background work stops: the per-user fan-out (sync, writeback, push, the
//     search indexer) drops the user at its next reconcile, and the job dispatcher
//     leaves the user's jobs (scheduled sends, snooze wake-ups) pending;
//   - stored mail is not touched.
//
// <user> is a user id or an email address. Both commands print what they did, are
// safe to run twice, and `disable` refuses to disable the last enabled user (it
// would lock everyone out, and nothing here could undo it without database
// access). The refusal and an unknown user exit non-zero.
func runUser(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("user", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: mailroom-admin user disable|enable <user-id-or-email>")
	}
	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	return userCore(ctx, d.store, fs.Arg(0), fs.Arg(1))
}

// userCore is runUser after flags and dependencies are resolved, split out so an
// integration test can drive it against a fixture store.
func userCore(ctx context.Context, st *store.Store, sub, ref string) error {
	var disable bool
	switch sub {
	case "disable":
		disable = true
	case "enable":
	default:
		return fmt.Errorf("user: unknown subcommand %q (want disable or enable)", sub)
	}
	res, err := st.SetUserDisabled(ctx, ref, disable)
	if err != nil {
		return fmt.Errorf("user %s: %w", sub, err)
	}
	switch res.Outcome {
	case store.UserOutcomeDisabled:
		rp("disabled %s (%s)\n", res.Email, res.ID)
		rpln("  sign-in is refused, every session and stream ends, API keys and OAuth grants are")
		rpln("  refused, and background work stops. Nothing was deleted; `user enable` restores it.")
	case store.UserOutcomeAlreadyDisabled:
		rp("%s (%s) was already disabled; nothing changed\n", res.Email, res.ID)
	case store.UserOutcomeEnabled:
		rp("enabled %s (%s)\n", res.Email, res.ID)
		rpln("  sign-in, API keys and OAuth grants work again and background work resumes.")
	case store.UserOutcomeAlreadyEnabled:
		rp("%s (%s) was already enabled; nothing changed\n", res.Email, res.ID)
	case store.UserOutcomeLastEnabled:
		rp("refused: %s (%s) is the last enabled user; disabling them would lock everyone out\n", res.Email, res.ID)
		return fmt.Errorf("user disable: %s is the last enabled user; enable or add another user first", res.Email)
	case store.UserOutcomeNotFound:
		return fmt.Errorf("user %s: no such user %q (give a user id or an email address)", sub, ref)
	default:
		return fmt.Errorf("user %s: unexpected outcome %q", sub, res.Outcome)
	}
	return nil
}
