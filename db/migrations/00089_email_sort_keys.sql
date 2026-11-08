-- SPDX-License-Identifier: AGPL-3.0-only
--
-- SORT KEYS FOR Email/query (RFC 8621 4.4.2): the BASE SUBJECT.
--
-- JMAP sorts by `subject` on the base subject of RFC 5256 2.1 (the subject with
-- reply and forward markers stripped), so "Re: Lunch" and "Lunch" sit together.
-- It is a function so the query and any later index name one expression.
-- NO INDEX covers it today: Email/query computes count(*) OVER () for the total,
-- which reads every row of the user's result anyway, so the planner has no
-- ordered-scan shortcut and sorts by subject or sender cost a pass over the
-- user's matching messages (computing the key per row) plus a sort. That is
-- linear in the user's mail, not in the page size.
--
-- The subject is sender-controlled and unbounded, so the strip works on its
-- first 1000 characters only (left(p_subject, 1000)): the work per message is
-- bounded whatever the sender sent. Two subjects equal in their first 1000
-- characters therefore tie, which is no loss for ordering.
--
-- IMMUTABLE and STRICT (so it could be indexed later). The strip is the
-- RFC's, in its regular form: the [fwd: ...] wrapper is unwrapped, leading
-- "Re:" / "Fw:" / "Fwd:" (each optionally followed by a [blob]) and bare
-- [blob]s are removed repeatedly, and trailing "(fwd)" markers and spaces go.
-- Whitespace runs collapse to one space first. app.base_subject lowercases it
-- (i;ascii-casemap); app.base_subject_raw keeps case (i;octet).
--
-- EXPAND-ONLY (ADR-008): two functions; no existing row is
-- rewritten, nothing JMAP-visible changes, so no change_log row is involved.

-- +goose Up

-- +goose StatementBegin
CREATE FUNCTION app.base_subject_raw(p_subject text) RETURNS text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
  SELECT btrim(
    regexp_replace(
      regexp_replace(
        regexp_replace(
          regexp_replace(left(p_subject, 1000), '\s+', ' ', 'g'),
          '^ ?\[fwd: ?(.*)\] ?$', '\1', 'i'),
        '^(?:\s*(?:re|fwd?)\s*(?:\[[^\]]*\])?\s*:|\s*\[[^\]]*\]\s*(?=\S))*', '', 'i'),
      '(?:\s*\(fwd\))*\s*$', '', 'i'))
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app.base_subject(p_subject text) RETURNS text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
  SELECT lower(app.base_subject_raw(p_subject))
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION app.base_subject_raw(text), app.base_subject(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.base_subject_raw(text), app.base_subject(text) TO mailroom_app, mailroom_admin;

-- +goose Down
DROP FUNCTION IF EXISTS app.base_subject(text);
DROP FUNCTION IF EXISTS app.base_subject_raw(text);
