-- SPDX-License-Identifier: AGPL-3.0-only
--
-- SORT KEYS FOR Email/query (RFC 8621 4.4.2): the BASE SUBJECT.
--
-- JMAP sorts by `subject` on the base subject of RFC 5256 2.1 (the subject with
-- reply and forward markers stripped), so "Re: Lunch" and "Lunch" sit together.
-- It is a function because it is also an INDEXED expression
-- (messages_user_sort_subject): an expression index is used only when the query
-- states the expression, so the query and the index must name one function.
--
-- IMMUTABLE and STRICT, as an index expression requires. The strip is the
-- RFC's, in its regular form: the [fwd: ...] wrapper is unwrapped, leading
-- "Re:" / "Fw:" / "Fwd:" (each optionally followed by a [blob]) and bare
-- [blob]s are removed repeatedly, and trailing "(fwd)" markers and spaces go.
-- Whitespace runs collapse to one space first. app.base_subject lowercases it
-- (i;ascii-casemap); app.base_subject_raw keeps case (i;octet).
--
-- EXPAND-ONLY (ADR-008): two functions and an index; no existing row is
-- rewritten, nothing JMAP-visible changes, so no change_log row is involved.

-- +goose Up

-- +goose StatementBegin
CREATE FUNCTION app.base_subject_raw(p_subject text) RETURNS text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
  SELECT btrim(
    regexp_replace(
      regexp_replace(
        regexp_replace(
          regexp_replace(p_subject, '\s+', ' ', 'g'),
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

-- +goose Down
DROP FUNCTION IF EXISTS app.base_subject(text);
DROP FUNCTION IF EXISTS app.base_subject_raw(text);
