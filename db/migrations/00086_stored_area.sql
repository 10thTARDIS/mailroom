-- SPDX-License-Identifier: AGPL-3.0-only
--
-- STORED RESOLVED AREA (ADR-049, increment 1: EXPAND). A message's area is
-- derived by the resolver (internal/store/areas_resolve.go, areaJoinSQL). This
-- migration adds a CACHE of that answer and the triggers that keep it correct;
-- no reader uses it yet, and it adds no meaning of its own: the resolver is
-- still the definition and the oracle.
--
--   app.messages.area_id          NULL = Needs sorting, exactly as the resolver
--   app.area_index_state          per-user "this column is complete" flag; set
--                                 by `mailroom-admin area-index --apply` only
--   app.resolve_message_area      the resolver's single-message form
--   app.recompute_message_areas   its set-based per-user form
--   triggers                      message_labels (statement), messages, areas, area_feeds,
--                                 labels (see each function)
--
-- EXPAND-ONLY (ADR-008): a nullable column, a table, functions, triggers and an
-- index. Existing rows are NOT rewritten; they stay NULL until the backfill.
--
-- No change_log row: the stored column changes nothing JMAP-visible (the label
-- or verdict change that moves it already appends its own change row), so the
-- load-bearing invariant is untouched.
--
-- THE TAB PRECEDENCE is hardcoded in app.area_tab_precedence() because SQL
-- cannot import tabs.PrecedenceNames(). A store test compares the two and goes
-- red if they differ.
--
-- All functions are SECURITY INVOKER: they run under the caller's identity and
-- RLS, so a trigger fired where the caller cannot see the user's rows updates
-- nothing (a no-op, never a wrong value).

-- +goose Up

ALTER TABLE app.messages
  ADD COLUMN area_id app.ulid;
ALTER TABLE app.messages
  ADD CONSTRAINT messages_area_id_fkey
  FOREIGN KEY (area_id) REFERENCES app.areas(id) ON DELETE SET NULL;

CREATE TABLE app.area_index_state (
  user_id  app.ulid PRIMARY KEY REFERENCES app.users(id) ON DELETE CASCADE,
  ready_at timestamptz
);
ALTER TABLE app.area_index_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.area_index_state FORCE ROW LEVEL SECURITY;
CREATE POLICY area_index_state_isolation ON app.area_index_state
  FOR ALL TO mailroom_app
  USING (user_id = app.current_user_id())
  WITH CHECK (user_id = app.current_user_id());
GRANT SELECT, INSERT, UPDATE, DELETE ON app.area_index_state TO mailroom_app;

-- An area's list in its own order, and its count (ADR-049 section 4).
CREATE INDEX messages_user_area_arrival
  ON app.messages (user_id, area_id, COALESCE(snooze_woke_at, received_at) DESC, id DESC);

-- +goose StatementBegin
CREATE FUNCTION app.area_tab_precedence() RETURNS text[]
LANGUAGE sql IMMUTABLE AS $$
  SELECT ARRAY['forums','social','promotions','updates','primary']::text[]
$$;
-- +goose StatementEnd

-- The resolver's single-message form. p_classified_tab is a parameter because
-- the BEFORE triggers run before the row holds the new verdict.
-- +goose StatementBegin
CREATE FUNCTION app.resolve_message_area(p_user_id app.ulid, p_message_id app.ulid,
                                         p_classified_tab app.category_tab)
RETURNS app.ulid
LANGUAGE sql AS $$
  SELECT COALESCE(
    (SELECT f.area_id
       FROM app.message_labels ml
       JOIN app.area_feeds f ON f.user_id = ml.user_id AND f.label_id = ml.label_id
       JOIN app.areas a ON a.user_id = f.user_id AND a.id = f.area_id
      WHERE ml.user_id = p_user_id AND ml.message_id = p_message_id
      ORDER BY a.position, a.id
      LIMIT 1),
    (SELECT tf.area_id
       FROM app.area_feeds tf
      WHERE tf.user_id = p_user_id
        AND tf.tab::text = COALESCE(
              (SELECT l.tab::text
                 FROM app.message_labels ml
                 JOIN app.labels l ON l.user_id = ml.user_id AND l.id = ml.label_id
                WHERE ml.user_id = p_user_id AND ml.message_id = p_message_id
                  AND l.tab IS NOT NULL
                ORDER BY array_position(app.area_tab_precedence(), l.tab::text)
                LIMIT 1),
              p_classified_tab::text)))
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app.resolve_message_area(p_user_id app.ulid, p_message_id app.ulid)
RETURNS app.ulid
LANGUAGE sql AS $$
  SELECT app.resolve_message_area(m.user_id, m.id, m.classified_tab)
    FROM app.messages m
   WHERE m.user_id = p_user_id AND m.id = p_message_id
$$;
-- +goose StatementEnd

-- The set-based form: one pass over a user's messages, three joins, as the
-- resolver's areaJoinSQL does (a per-message call here would re-evaluate RLS
-- per row). Writes only rows whose stored area differs. Returns how many
-- messages changed.
-- +goose StatementBegin
CREATE FUNCTION app.recompute_message_areas(p_user_id app.ulid) RETURNS bigint
LANGUAGE plpgsql AS $$
DECLARE
  v_changed bigint;
BEGIN
  WITH la AS (
    SELECT DISTINCT ON (ml.message_id) ml.message_id, f.area_id
      FROM app.message_labels ml
      JOIN app.area_feeds f ON f.user_id = ml.user_id AND f.label_id = ml.label_id
      JOIN app.areas a ON a.user_id = f.user_id AND a.id = f.area_id
     WHERE ml.user_id = p_user_id
     ORDER BY ml.message_id, a.position, a.id
  ), lt AS (
    SELECT DISTINCT ON (ml.message_id) ml.message_id, l.tab::text AS tab
      FROM app.message_labels ml
      JOIN app.labels l ON l.user_id = ml.user_id AND l.id = ml.label_id
     WHERE ml.user_id = p_user_id AND l.tab IS NOT NULL
     ORDER BY ml.message_id, array_position(app.area_tab_precedence(), l.tab::text)
  ), r AS (
    SELECT m.id, COALESCE(la.area_id, tf.area_id) AS area_id
      FROM app.messages m
      LEFT JOIN la ON la.message_id = m.id
      LEFT JOIN lt ON lt.message_id = m.id
      LEFT JOIN app.area_feeds tf
             ON tf.user_id = m.user_id
            AND tf.tab::text = COALESCE(lt.tab, m.classified_tab::text)
     WHERE m.user_id = p_user_id
  )
  UPDATE app.messages m
     SET area_id = r.area_id
    FROM r
   WHERE m.user_id = p_user_id AND m.id = r.id
     AND m.area_id IS DISTINCT FROM r.area_id;
  GET DIAGNOSTICS v_changed = ROW_COUNT;
  RETURN v_changed;
END;
$$;
-- +goose StatementEnd

-- Labels added to or removed from messages: re-resolve the touched messages in
-- ONE statement per SQL statement, driven by the transition table: each touched
-- message is resolved by two indexed LATERAL lookups (its feeding label, its
-- mapped tab), the single-message resolver's shape, so a bulk relabel pays no
-- per-row query planning. A set-based DISTINCT ON form was measured an order of
-- magnitude slower here: the planner cannot see how many rows a DISTINCT ON
-- yields and chose a nested loop over it. The pairs are not de-duplicated: a
-- message named twice resolves to the same answer twice. Writes only rows that
-- differ. The resolution is MATERIALIZED so the planner cannot re-run it per
-- joined row.
-- +goose StatementBegin
CREATE FUNCTION app.trg_message_labels_ins() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  WITH r AS MATERIALIZED (
      SELECT m2.user_id, m2.id, COALESCE(la.area_id, tf.area_id) AS area_id
        FROM (SELECT user_id, message_id FROM newt) c
        JOIN app.messages m2 ON m2.user_id = c.user_id AND m2.id = c.message_id
        LEFT JOIN LATERAL (
          SELECT f.area_id
            FROM app.message_labels ml
            JOIN app.area_feeds f ON f.user_id = ml.user_id AND f.label_id = ml.label_id
            JOIN app.areas a ON a.user_id = f.user_id AND a.id = f.area_id
           WHERE ml.user_id = m2.user_id AND ml.message_id = m2.id
           ORDER BY a.position, a.id
           LIMIT 1) la ON true
        LEFT JOIN LATERAL (
          SELECT l.tab::text AS tab
            FROM app.message_labels ml
            JOIN app.labels l ON l.user_id = ml.user_id AND l.id = ml.label_id
           WHERE ml.user_id = m2.user_id AND ml.message_id = m2.id AND l.tab IS NOT NULL
           ORDER BY array_position(app.area_tab_precedence(), l.tab::text)
           LIMIT 1) lt ON true
        LEFT JOIN app.area_feeds tf
               ON tf.user_id = m2.user_id
              AND tf.tab::text = COALESCE(lt.tab, m2.classified_tab::text)
  )
  UPDATE app.messages m
     SET area_id = r.area_id
    FROM r
   WHERE m.user_id = r.user_id AND m.id = r.id
     AND m.area_id IS DISTINCT FROM r.area_id;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app.trg_message_labels_del() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  WITH r AS MATERIALIZED (
      SELECT m2.user_id, m2.id, COALESCE(la.area_id, tf.area_id) AS area_id
        FROM (SELECT user_id, message_id FROM oldt) c
        JOIN app.messages m2 ON m2.user_id = c.user_id AND m2.id = c.message_id
        LEFT JOIN LATERAL (
          SELECT f.area_id
            FROM app.message_labels ml
            JOIN app.area_feeds f ON f.user_id = ml.user_id AND f.label_id = ml.label_id
            JOIN app.areas a ON a.user_id = f.user_id AND a.id = f.area_id
           WHERE ml.user_id = m2.user_id AND ml.message_id = m2.id
           ORDER BY a.position, a.id
           LIMIT 1) la ON true
        LEFT JOIN LATERAL (
          SELECT l.tab::text AS tab
            FROM app.message_labels ml
            JOIN app.labels l ON l.user_id = ml.user_id AND l.id = ml.label_id
           WHERE ml.user_id = m2.user_id AND ml.message_id = m2.id AND l.tab IS NOT NULL
           ORDER BY array_position(app.area_tab_precedence(), l.tab::text)
           LIMIT 1) lt ON true
        LEFT JOIN app.area_feeds tf
               ON tf.user_id = m2.user_id
              AND tf.tab::text = COALESCE(lt.tab, m2.classified_tab::text)
  )
  UPDATE app.messages m
     SET area_id = r.area_id
    FROM r
   WHERE m.user_id = r.user_id AND m.id = r.id
     AND m.area_id IS DISTINCT FROM r.area_id;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- A new message, or a changed verdict: store the area with the row itself.
-- +goose StatementBegin
CREATE FUNCTION app.trg_messages_area() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  NEW.area_id := app.resolve_message_area(NEW.user_id, NEW.id, NEW.classified_tab);
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- Configuration changes: recompute every message of each affected user.
-- +goose StatementBegin
CREATE FUNCTION app.trg_area_config_ins() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM app.recompute_message_areas(u.user_id)
     FROM (SELECT DISTINCT user_id FROM newt) u;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app.trg_area_config_del() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM app.recompute_message_areas(u.user_id)
     FROM (SELECT DISTINCT user_id FROM oldt) u;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app.trg_areas_position() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM app.recompute_message_areas(u.user_id)
     FROM (SELECT DISTINCT n.user_id
             FROM newt n JOIN oldt o ON o.id = n.id
            WHERE o.position IS DISTINCT FROM n.position) u;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app.trg_area_feeds_upd() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM app.recompute_message_areas(u.user_id)
     FROM (SELECT DISTINCT n.user_id
             FROM newt n JOIN oldt o ON o.id = n.id
            WHERE o.area_id IS DISTINCT FROM n.area_id
               OR o.tab IS DISTINCT FROM n.tab
               OR o.label_id IS DISTINCT FROM n.label_id) u;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- A label deleted: take it off its messages first, in the CALLER's context.
-- Left to the foreign key's cascade, the removal would run as the table owner,
-- which RLS (FORCED, owner without a policy) shows no rows, so the row triggers
-- above would compute from nothing and leave a stale area (ADR-049 section 2
-- assumed that case is a harmless no-op; it is not).
-- +goose StatementBegin
CREATE FUNCTION app.trg_labels_detach() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  DELETE FROM app.message_labels WHERE user_id = OLD.user_id AND label_id = OLD.id;
  RETURN OLD;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app.trg_labels_tab() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM app.recompute_message_areas(u.user_id)
     FROM (SELECT DISTINCT n.user_id
             FROM newt n JOIN oldt o ON o.id = n.id
            WHERE o.tab IS DISTINCT FROM n.tab) u;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd

GRANT EXECUTE ON FUNCTION
  app.area_tab_precedence(),
  app.resolve_message_area(app.ulid, app.ulid, app.category_tab),
  app.resolve_message_area(app.ulid, app.ulid),
  app.recompute_message_areas(app.ulid),
  app.trg_message_labels_ins(),
  app.trg_message_labels_del(),
  app.trg_messages_area(),
  app.trg_area_config_ins(),
  app.trg_area_config_del(),
  app.trg_areas_position(),
  app.trg_area_feeds_upd(),
  app.trg_labels_tab(),
  app.trg_labels_detach()
TO mailroom_app;

CREATE TRIGGER message_labels_area_ins
  AFTER INSERT ON app.message_labels
  REFERENCING NEW TABLE AS newt
  FOR EACH STATEMENT EXECUTE FUNCTION app.trg_message_labels_ins();
CREATE TRIGGER message_labels_area_del
  AFTER DELETE ON app.message_labels
  REFERENCING OLD TABLE AS oldt
  FOR EACH STATEMENT EXECUTE FUNCTION app.trg_message_labels_del();

CREATE TRIGGER messages_area_insert
  BEFORE INSERT ON app.messages
  FOR EACH ROW EXECUTE FUNCTION app.trg_messages_area();
CREATE TRIGGER messages_area_verdict
  BEFORE UPDATE OF classified_tab ON app.messages
  FOR EACH ROW WHEN (OLD.classified_tab IS DISTINCT FROM NEW.classified_tab)
  EXECUTE FUNCTION app.trg_messages_area();

CREATE TRIGGER areas_area_ins
  AFTER INSERT ON app.areas
  REFERENCING NEW TABLE AS newt
  FOR EACH STATEMENT EXECUTE FUNCTION app.trg_area_config_ins();
CREATE TRIGGER areas_area_del
  AFTER DELETE ON app.areas
  REFERENCING OLD TABLE AS oldt
  FOR EACH STATEMENT EXECUTE FUNCTION app.trg_area_config_del();
CREATE TRIGGER areas_area_pos
  AFTER UPDATE ON app.areas
  REFERENCING OLD TABLE AS oldt NEW TABLE AS newt
  FOR EACH STATEMENT EXECUTE FUNCTION app.trg_areas_position();

CREATE TRIGGER area_feeds_area_ins
  AFTER INSERT ON app.area_feeds
  REFERENCING NEW TABLE AS newt
  FOR EACH STATEMENT EXECUTE FUNCTION app.trg_area_config_ins();
CREATE TRIGGER area_feeds_area_del
  AFTER DELETE ON app.area_feeds
  REFERENCING OLD TABLE AS oldt
  FOR EACH STATEMENT EXECUTE FUNCTION app.trg_area_config_del();
CREATE TRIGGER area_feeds_area_upd
  AFTER UPDATE ON app.area_feeds
  REFERENCING OLD TABLE AS oldt NEW TABLE AS newt
  FOR EACH STATEMENT EXECUTE FUNCTION app.trg_area_feeds_upd();

CREATE TRIGGER labels_area_detach
  BEFORE DELETE ON app.labels
  FOR EACH ROW EXECUTE FUNCTION app.trg_labels_detach();
CREATE TRIGGER labels_area_tab
  AFTER UPDATE ON app.labels
  REFERENCING OLD TABLE AS oldt NEW TABLE AS newt
  FOR EACH STATEMENT EXECUTE FUNCTION app.trg_labels_tab();

-- +goose Down
DROP TRIGGER IF EXISTS labels_area_tab ON app.labels;
DROP TRIGGER IF EXISTS labels_area_detach ON app.labels;
DROP TRIGGER IF EXISTS area_feeds_area_upd ON app.area_feeds;
DROP TRIGGER IF EXISTS area_feeds_area_del ON app.area_feeds;
DROP TRIGGER IF EXISTS area_feeds_area_ins ON app.area_feeds;
DROP TRIGGER IF EXISTS areas_area_pos ON app.areas;
DROP TRIGGER IF EXISTS areas_area_del ON app.areas;
DROP TRIGGER IF EXISTS areas_area_ins ON app.areas;
DROP TRIGGER IF EXISTS messages_area_verdict ON app.messages;
DROP TRIGGER IF EXISTS messages_area_insert ON app.messages;
DROP TRIGGER IF EXISTS message_labels_area_del ON app.message_labels;
DROP TRIGGER IF EXISTS message_labels_area_ins ON app.message_labels;
DROP FUNCTION IF EXISTS app.trg_labels_tab();
DROP FUNCTION IF EXISTS app.trg_labels_detach();
DROP FUNCTION IF EXISTS app.trg_area_feeds_upd();
DROP FUNCTION IF EXISTS app.trg_areas_position();
DROP FUNCTION IF EXISTS app.trg_area_config_del();
DROP FUNCTION IF EXISTS app.trg_area_config_ins();
DROP FUNCTION IF EXISTS app.trg_messages_area();
DROP FUNCTION IF EXISTS app.trg_message_labels_del();
DROP FUNCTION IF EXISTS app.trg_message_labels_ins();
DROP FUNCTION IF EXISTS app.recompute_message_areas(app.ulid);
DROP FUNCTION IF EXISTS app.resolve_message_area(app.ulid, app.ulid);
DROP FUNCTION IF EXISTS app.resolve_message_area(app.ulid, app.ulid, app.category_tab);
DROP FUNCTION IF EXISTS app.area_tab_precedence();
DROP INDEX IF EXISTS app.messages_user_area_arrival;
DROP TABLE IF EXISTS app.area_index_state;
ALTER TABLE app.messages DROP CONSTRAINT IF EXISTS messages_area_id_fkey;
ALTER TABLE app.messages DROP COLUMN IF EXISTS area_id;
