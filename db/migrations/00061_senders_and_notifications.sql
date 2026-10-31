-- SPDX-License-Identifier: AGPL-3.0-only
--
-- SENDERS AND NOTIFICATIONS (#59): block sender, VIP, triage-aware push per
-- device, quiet hours, and per-account push. EXPAND-ONLY (ADR-008): new
-- columns with constant defaults that keep today's behaviour, two partial
-- indexes, and nothing backfilled or rewritten. Every ADD COLUMN below is
-- catalog-only (a constant default, no table rewrite, no UPDATE), so
-- docs/migrations.md's FORCE-RLS backfill rule does not arise.
--
-- 1. app.contacts.block_destination / blocked_at. app.contacts has carried
--    `vip` and `blocked` booleans since the baseline (spec §10a) with nothing
--    reading them. Blocking now means something: new mail from a blocked
--    address goes to Junk (the default) or Trash, enforced at ingest before
--    rules and learned placement. block_destination is where; blocked_at is
--    when (shown in Settings, and NULL exactly when blocked is false — the
--    CHECK makes a half-blocked row unrepresentable). The column default
--    'junk' is the operator's default (#59); it is meaningful only while
--    blocked is true.
--
-- 2. app.push_subscriptions.notify_mode: THE PER-DEVICE PUSH MODE.
--      all                every new message (today's behaviour, the default)
--      above_line         only mail above the desk's line (or marked
--                         important by a rule, see internal/notify)
--      vip                only mail from a VIP
--      above_line_or_vip  either
--    It REPLACES important_only, which defaulted to true and was never read
--    (#626 §E). important_only is deliberately NOT migrated into the mode:
--    every existing row holds true only because that was the column default,
--    not because anyone chose it, and the decision is that push
--    stays "every message" until the user picks (#59). The column stays
--    (expand-only; dropping it is a later contract step) and nothing reads it.
--
-- 3. app.accounts.notify_push: per-account push on/off (#59), default on.
--
-- 4. app.desk_preferences quiet hours: the per-user settings singleton (it
--    already holds learned_placement_enabled, which is not a desk setting
--    either). Off by default. start/end are minutes after local midnight in
--    quiet_hours_timezone (an IANA zone name, validated by the API, never
--    by SQL); days is a bitmask of the day a quiet period STARTS on, bit 0 =
--    Sunday (Go's time.Weekday). allow_vip defaults ON (#59).
--
-- 5. Two partial indexes leading with user_id: the VIP and blocked contacts
--    are a handful of rows among thousands of auto-derived ones, and both are
--    read hot — is:vip and the push counter build their VIP address set from
--    contacts_user_vip; Settings lists contacts_user_blocked.
--
-- The ingest block lookup itself needs no new index: it is one probe of
-- contact_addresses' UNIQUE (user_id, address) plus the contacts primary key.

-- +goose Up

ALTER TABLE app.contacts ADD COLUMN block_destination text NOT NULL DEFAULT 'junk'
  CHECK (block_destination IN ('junk', 'trash'));
ALTER TABLE app.contacts ADD COLUMN blocked_at timestamptz;
ALTER TABLE app.contacts ADD CONSTRAINT contacts_blocked_stamped
  CHECK (blocked = (blocked_at IS NOT NULL));

ALTER TABLE app.push_subscriptions ADD COLUMN notify_mode text NOT NULL DEFAULT 'all'
  CHECK (notify_mode IN ('all', 'above_line', 'vip', 'above_line_or_vip'));

ALTER TABLE app.accounts ADD COLUMN notify_push boolean NOT NULL DEFAULT true;

ALTER TABLE app.desk_preferences ADD COLUMN quiet_hours_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE app.desk_preferences ADD COLUMN quiet_hours_start smallint NOT NULL DEFAULT 1320
  CHECK (quiet_hours_start >= 0 AND quiet_hours_start < 1440);
ALTER TABLE app.desk_preferences ADD COLUMN quiet_hours_end smallint NOT NULL DEFAULT 420
  CHECK (quiet_hours_end >= 0 AND quiet_hours_end < 1440);
ALTER TABLE app.desk_preferences ADD COLUMN quiet_hours_days smallint NOT NULL DEFAULT 127
  CHECK (quiet_hours_days >= 0 AND quiet_hours_days <= 127);
ALTER TABLE app.desk_preferences ADD COLUMN quiet_hours_timezone text NOT NULL DEFAULT 'UTC';
ALTER TABLE app.desk_preferences ADD COLUMN quiet_hours_allow_vip boolean NOT NULL DEFAULT true;

CREATE INDEX contacts_user_vip ON app.contacts (user_id) WHERE vip;
CREATE INDEX contacts_user_blocked ON app.contacts (user_id) WHERE blocked;

-- +goose Down
DROP INDEX IF EXISTS app.contacts_user_blocked;
DROP INDEX IF EXISTS app.contacts_user_vip;
ALTER TABLE app.desk_preferences DROP COLUMN IF EXISTS quiet_hours_allow_vip;
ALTER TABLE app.desk_preferences DROP COLUMN IF EXISTS quiet_hours_timezone;
ALTER TABLE app.desk_preferences DROP COLUMN IF EXISTS quiet_hours_days;
ALTER TABLE app.desk_preferences DROP COLUMN IF EXISTS quiet_hours_end;
ALTER TABLE app.desk_preferences DROP COLUMN IF EXISTS quiet_hours_start;
ALTER TABLE app.desk_preferences DROP COLUMN IF EXISTS quiet_hours_enabled;
ALTER TABLE app.accounts DROP COLUMN IF EXISTS notify_push;
ALTER TABLE app.push_subscriptions DROP COLUMN IF EXISTS notify_mode;
ALTER TABLE app.contacts DROP CONSTRAINT IF EXISTS contacts_blocked_stamped;
ALTER TABLE app.contacts DROP COLUMN IF EXISTS blocked_at;
ALTER TABLE app.contacts DROP COLUMN IF EXISTS block_destination;
