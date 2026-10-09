-- Format cleanup postflight (migration 000027), read-only.
--
-- Run as `postgres` against the migrated database, right after `migrate`
-- and before the app takes writes, with the preflight's last line from the
-- same database:
--
--   psql -X -v ON_ERROR_STOP=1 -U postgres -d babytracker \
--     -v before="$(grep '^FORMAT_CLEANUP_BEFORE ' preflight.out)" -f format-cleanup-postflight.sql
--
-- Checks every invariant of spec S-4 and prints PASS or FAIL for each, then a
-- per-day session listing of the last 14 days (Europe/Zurich): times, kinds,
-- part counts and gaps only, never notes, names, food text, ids or volumes.
-- Exits non-zero when any invariant fails.
--
-- With `-v listing_only=1` (no before line; any value, even 0, counts:
-- psql tests only that the variable is set) it prints only the session
-- listing, e.g. to compare the last days with a phone after the app has taken
-- writes (runbook S-6, C-2), when the invariants no longer apply.
\set QUIET 1
\set ON_ERROR_STOP 1
\pset footer off
\pset null '-'
SET client_min_messages = warning;

\if :{?listing_only}
\elif :{?before}
\else
\echo 'format-cleanup-postflight: pass the preflight''s FORMAT_CLEANUP_BEFORE line as -v before="..."'
DO $$ BEGIN RAISE EXCEPTION 'format_cleanup_postflight: before numbers missing'; END $$;
\endif

BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;

\if :{?listing_only}
\echo '== Format cleanup session listing (read-only) =='
\else
\echo '== Format cleanup postflight (read-only) =='

-- Before numbers; a missing one reads as (-1) (or "missing") and fails its check.
SELECT (SELECT count(*) FROM jsonb_object_keys(b)) AS keys,
       coalesce(b->>'families', '(-1)') AS families, coalesce(b->>'memberships', '(-1)') AS memberships,
       coalesce(b->>'babies', '(-1)') AS babies, coalesce(b->>'feed_entries', '(-1)') AS feed_entries,
       coalesce(b->>'sleep_entries', '(-1)') AS sleep_entries, coalesce(b->>'diaper_entries', '(-1)') AS diaper_entries,
       coalesce(b->>'supplement_entries', '(-1)') AS supplement_entries, coalesce(b->>'growth_profiles', '(-1)') AS growth_profiles,
       coalesce(b->>'measurement_entries', '(-1)') AS measurement_entries, coalesce(b->>'active_timers', '(-1)') AS active_timers,
       coalesce(b->>'feed_components', '(-1)') AS feed_components, coalesce(b->>'timer_components', '(-1)') AS timer_components,
       coalesce(b->>'mutation_receipts', '(-1)') AS mutation_receipts, coalesce(b->>'journal_rows', '(-1)') AS journal_rows,
       coalesce(b->>'next_sequence_sum', '(-1)') AS next_sequence_sum, coalesce(b->>'forced_rls_tables', '(-1)') AS forced_rls_tables,
       coalesce(b->>'expected_appended', '(-1)') AS expected_appended, coalesce(b->>'expected_joined', '(-1)') AS expected_joined,
       coalesce(b->>'appended_digest', 'missing') AS appended_digest, coalesce(b->>'joined_digest', 'missing') AS joined_digest,
       coalesce(b->>'expected_relinked', '(-1)') AS expected_relinked, coalesce(b->>'relinked_digest', 'missing') AS relinked_digest
FROM (SELECT coalesce(jsonb_object_agg(split_part(pair, '=', 1), split_part(pair, '=', 2))
                        FILTER (WHERE pair ~ '^[a-z_]+=[0-9a-f]+$'), '{}'::jsonb) AS b
        FROM regexp_split_to_table(btrim(:'before'), '\s+') AS pair) parsed
\gset before_

-- ---------------------------------------------------------------------------
-- After numbers.
-- ---------------------------------------------------------------------------
SELECT (SELECT count(*) FROM public.families) AS families,
       (SELECT count(*) FROM public.memberships) AS memberships,
       (SELECT count(*) FROM public.babies) AS babies,
       (SELECT count(*) FROM public.feed_entries) AS feed_entries,
       (SELECT count(*) FROM public.sleep_entries) AS sleep_entries,
       (SELECT count(*) FROM public.diaper_entries) AS diaper_entries,
       (SELECT count(*) FROM public.supplement_entries) AS supplement_entries,
       (SELECT count(*) FROM public.growth_profiles) AS growth_profiles,
       (SELECT count(*) FROM public.measurement_entries) AS measurement_entries,
       (SELECT count(*) FROM public.active_timers) AS active_timers,
       (SELECT count(*) FROM public.feed_components) AS feed_components,
       (SELECT count(*) FROM public.timer_components) AS timer_components,
       (SELECT count(*) FROM public.mutation_receipts) AS mutation_receipts,
       (SELECT count(*) FROM public.mutation_journal) AS journal_rows,
       (SELECT coalesce(sum(next_sequence), 0) FROM public.families) AS next_sequence_sum,
       (SELECT count(*) FROM public.feed_entries WHERE component_version <> 4) AS old_feeds,
       (SELECT count(*) FROM public.sleep_entries WHERE payload_version <> 2) AS old_sleeps,
       (SELECT count(*) FROM public.active_timers WHERE component_version <> 5) AS old_timers,
       (SELECT count(*) FROM public.feed_entries f WHERE f.tombstoned_at IS NULL
          AND NOT EXISTS (SELECT FROM public.feed_components c WHERE c.family_id = f.family_id AND c.feed_id = f.id)) AS feeds_without_parts,
       (SELECT count(*) FROM public.mutation_journal
         WHERE operation NOT IN ('create', 'update', 'delete', 'timer_start', 'timer_finish_bottle', 'timer_stop')) AS legacy_operations
\gset after_

-- Stored entities, journal and receipts, classified by the keys they hold:
-- legacy keys (a scalar feed or completed_sides timer), a missing session or
-- bottle-phase key, or a solids part without its foods list.
SELECT count(*) FILTER (WHERE source = 'journal' AND legacy) AS journal_legacy,
       count(*) FILTER (WHERE source = 'journal' AND missing_key) AS journal_missing_key,
       count(*) FILTER (WHERE source = 'journal' AND solids_without_foods) AS journal_solids,
       count(*) FILTER (WHERE source = 'receipts' AND legacy) AS receipts_legacy,
       count(*) FILTER (WHERE source = 'receipts' AND missing_key) AS receipts_missing_key,
       count(*) FILTER (WHERE source = 'receipts' AND solids_without_foods) AS receipts_solids
FROM (
  SELECT 'journal' source, j.canonical_entity entity FROM public.mutation_journal j
  UNION ALL
  SELECT 'receipts', r.outcome->'canonical_entity' FROM public.mutation_receipts r WHERE jsonb_typeof(r.outcome->'canonical_entity') = 'object'
  UNION ALL
  SELECT 'receipts', related.entity FROM public.mutation_receipts r,
    jsonb_array_elements(CASE WHEN jsonb_typeof(r.outcome->'related_entities') = 'array' THEN r.outcome->'related_entities' ELSE '[]'::jsonb END) related(entity)
) stored
CROSS JOIN LATERAL (SELECT stored.entity->>'entity_kind' AS kind,
  CASE WHEN jsonb_typeof(stored.entity->'payload') = 'object' THEN stored.entity->'payload' END AS payload) e
CROSS JOIN LATERAL (SELECT
  coalesce(CASE e.kind
    WHEN 'feed' THEN e.payload ?| ARRAY['sides','volume_ml','contents','foods','amount'] OR NOT e.payload ? 'components'
    WHEN 'timer' THEN e.payload ?| ARRAY['completed_sides','pending_volume_ml','pending_contents'] OR NOT e.payload ? 'completed_components'
    ELSE false END, false) AS legacy,
  coalesce(CASE e.kind
    WHEN 'feed' THEN NOT e.payload ? 'feeding_session_id'
    WHEN 'sleep' THEN NOT e.payload ? 'sleep_session_id'
    WHEN 'timer' THEN NOT (e.payload ? 'session_id' AND e.payload ? 'current_bottle_started_at')
    ELSE false END, false) AS missing_key,
  EXISTS (SELECT FROM jsonb_array_elements(CASE
      WHEN jsonb_typeof(e.payload->'components') = 'array' THEN e.payload->'components'
      WHEN jsonb_typeof(e.payload->'completed_components') = 'array' THEN e.payload->'completed_components'
      ELSE '[]'::jsonb END) part
    WHERE part->>'kind' = 'solids' AND jsonb_typeof(part->'foods') IS DISTINCT FROM 'array') AS solids_without_foods) classified
\gset stored_

-- The heads 000027 appended: its transaction stamped them, the records and
-- schema_compatibility.recorded_at with the same now(). An appended head
-- joined its record when it carries a session link the record's previous
-- journal row did not, and relinked it (moved it with its root, ruling
-- B-R13) when both carry a link and the links differ.
SELECT count(*) AS appended,
       md5(coalesce(string_agg(head.entity_kind || ':' || head.family_id || ':' || head.entity_id, ','
         ORDER BY head.entity_kind || ':' || head.family_id || ':' || head.entity_id), '')) AS appended_digest,
       count(*) FILTER (WHERE head.joined) AS joined,
       md5(coalesce(string_agg(head.entity_kind || ':' || head.family_id || ':' || head.entity_id, ','
         ORDER BY head.entity_kind || ':' || head.family_id || ':' || head.entity_id) FILTER (WHERE head.joined), '')) AS joined_digest,
       count(*) FILTER (WHERE head.relinked) AS relinked,
       md5(coalesce(string_agg(head.entity_kind || ':' || head.family_id || ':' || head.entity_id, ','
         ORDER BY head.entity_kind || ':' || head.family_id || ':' || head.entity_id) FILTER (WHERE head.relinked), '')) AS relinked_digest,
       count(*) FILTER (WHERE head.operation <> 'update') AS not_updates
FROM (
  SELECT linked.*, linked.link IS NOT NULL AND linked.previous_link IS NULL AS joined,
         linked.link IS NOT NULL AND linked.previous_link IS NOT NULL AND linked.link <> linked.previous_link AS relinked
    FROM (
    SELECT j.family_id, j.entity_kind, j.entity_id, j.operation,
           CASE WHEN j.entity_kind IN ('feed', 'sleep') THEN
             j.canonical_entity->'payload'->>CASE j.entity_kind WHEN 'feed' THEN 'feeding_session_id' ELSE 'sleep_session_id' END END AS link,
           CASE WHEN j.entity_kind IN ('feed', 'sleep') THEN
             (SELECT p.canonical_entity->'payload'->>CASE j.entity_kind WHEN 'feed' THEN 'feeding_session_id' ELSE 'sleep_session_id' END
                FROM public.mutation_journal p
               WHERE p.family_id = j.family_id AND p.entity_kind = j.entity_kind AND p.entity_id = j.entity_id AND p.sequence < j.sequence
               ORDER BY p.sequence DESC LIMIT 1) END AS previous_link
      FROM public.mutation_journal j
     WHERE j.accepted_at = (SELECT recorded_at FROM public.schema_compatibility WHERE singleton AND current_version = 27)
    ) linked
) head
\gset appended_

-- Per family, the appended heads are exactly the newest sequences.
SELECT count(*) AS families_with_gaps FROM (
  SELECT j.family_id, count(*) AS heads, min(j.sequence) AS first_head, max(j.sequence) AS last_head
    FROM public.mutation_journal j
   WHERE j.accepted_at = (SELECT recorded_at FROM public.schema_compatibility WHERE singleton AND current_version = 27)
   GROUP BY j.family_id) appended
JOIN public.families f ON f.id = appended.family_id
WHERE appended.last_head <> f.next_sequence OR appended.first_head <> f.next_sequence - appended.heads + 1
\gset sequence_

SELECT count(*) AS missing FROM (
  SELECT 'baby' kind, family_id, id, entity_version, last_sequence FROM public.babies
  UNION ALL SELECT 'feed', family_id, id, entity_version, last_sequence FROM public.feed_entries
  UNION ALL SELECT 'sleep', family_id, id, entity_version, last_sequence FROM public.sleep_entries
  UNION ALL SELECT 'diaper', family_id, id, entity_version, last_sequence FROM public.diaper_entries
  UNION ALL SELECT 'supplement', family_id, id, entity_version, last_sequence FROM public.supplement_entries
  UNION ALL SELECT 'timer', family_id, id, entity_version, last_sequence FROM public.active_timers
  UNION ALL SELECT 'growth_profile', family_id, id, entity_version, last_sequence FROM public.growth_profiles
  UNION ALL SELECT 'measurement', family_id, id, entity_version, last_sequence FROM public.measurement_entries
) record
WHERE NOT EXISTS (SELECT FROM public.mutation_journal j
                   WHERE j.family_id = record.family_id AND j.entity_kind = record.kind AND j.entity_id = record.id
                     AND j.entity_version = record.entity_version AND j.sequence = record.last_sequence)
\gset heads_

-- Each live feed's and sleep's session link equals the link its journal head
-- publishes, so no link changed without a head.
SELECT count(*) AS differing FROM (
  SELECT f.family_id, f.last_sequence, 'feeding_session_id' AS key, f.feeding_session_id::text AS link
    FROM public.feed_entries f WHERE f.tombstoned_at IS NULL
  UNION ALL
  SELECT s.family_id, s.last_sequence, 'sleep_session_id', s.sleep_session_id::text
    FROM public.sleep_entries s WHERE s.tombstoned_at IS NULL
) record
JOIN public.mutation_journal j ON j.family_id = record.family_id AND j.sequence = record.last_sequence
WHERE (j.canonical_entity->'payload'->>record.key) IS DISTINCT FROM record.link
\gset links_

SELECT count(*) FILTER (WHERE c.relforcerowsecurity) AS forced,
       count(*) FILTER (WHERE c.relrowsecurity AND NOT c.relforcerowsecurity) AS unforced,
       count(*) FILTER (WHERE c.relname IN ('families','feed_entries','feed_components','sleep_entries','active_timers',
         'timer_components','mutation_journal','mutation_receipts','sync_page_sessions','sync_snapshots',
         'sync_snapshot_entities','sync_snapshot_admissions','import_sessions') AND c.relforcerowsecurity) AS bracket_forced
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relkind = 'r'
\gset rls_

SELECT (SELECT current_version FROM public.schema_compatibility WHERE singleton) AS version,
       (SELECT introduced_by FROM public.schema_compatibility WHERE singleton) = '000027_format_cleanup.sql' AS introduced_by_27,
       (SELECT max(version) FROM public.schema_migrations) AS last_migration,
       (SELECT count(*) FROM unnest(ARRAY['public.feed_side_intervals','public.feed_foods','public.timer_side_intervals']) t
         WHERE to_regclass(t) IS NOT NULL)
     + (SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public'
         AND ((table_name = 'feed_entries' AND column_name IN ('volume_ml','contents','amount'))
           OR (table_name = 'active_timers' AND column_name IN ('pending_volume_ml','pending_contents'))))
     + (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
         WHERE n.nspname = 'app' AND p.proname IN ('project_legacy_feed_components','project_legacy_feed_parent_components',
           'project_legacy_timer_components','project_legacy_timer_parent_components','rebuild_feed_components',
           'rebuild_timer_components','legacy_component_id','sha1')) AS legacy_storage_left
\gset schema_

SELECT (SELECT count(*) FROM public.schema_privilege_manifest WHERE schema_version = 27) AS rows_27,
       (SELECT count(*) FROM public.schema_privilege_manifest WHERE schema_version = 26) AS rows_26,
       (SELECT count(*) FROM (
          (SELECT principal, object_kind, object_identity, privileges FROM public.schema_privilege_manifest WHERE schema_version = 27
           EXCEPT
           SELECT principal, object_kind, object_identity, privileges FROM public.schema_privilege_manifest WHERE schema_version = 26
             AND NOT (object_kind = 'table' AND object_identity IN ('public.feed_side_intervals','public.feed_foods','public.timer_side_intervals')))
          UNION ALL
          (SELECT principal, object_kind, object_identity, privileges FROM public.schema_privilege_manifest WHERE schema_version = 26
             AND NOT (object_kind = 'table' AND object_identity IN ('public.feed_side_intervals','public.feed_foods','public.timer_side_intervals'))
           EXCEPT
           SELECT principal, object_kind, object_identity, privileges FROM public.schema_privilege_manifest WHERE schema_version = 27)
        ) differing) AS differing
\gset manifest_

-- ---------------------------------------------------------------------------
-- Invariants.
-- ---------------------------------------------------------------------------
SELECT string_agg(format('%s  %-74s expected %-26s got %s', CASE WHEN ok THEN 'PASS' ELSE 'FAIL' END,
         invariant, expected, actual), E'\n' ORDER BY n) AS report,
       bool_and(ok) AS ok, count(*) FILTER (WHERE NOT ok) AS failed
FROM (VALUES
  (1, 'preflight numbers present (FORMAT_CLEANUP_BEFORE)', '22 values', :before_keys::text, :before_keys = 22),
  (2, 'schema 27 (compatibility, introduced_by, last migration)', '27/t/27',
      concat_ws('/', :schema_version, :'schema_introduced_by_27', :schema_last_migration),
      :schema_version = 27 AND :'schema_introduced_by_27'::boolean AND :schema_last_migration = 27),
  (10, 'rows unchanged: families', :before_families::text, :after_families::text, :after_families = :before_families),
  (11, 'rows unchanged: memberships', :before_memberships::text, :after_memberships::text, :after_memberships = :before_memberships),
  (12, 'rows unchanged: babies', :before_babies::text, :after_babies::text, :after_babies = :before_babies),
  (13, 'rows unchanged: feed_entries', :before_feed_entries::text, :after_feed_entries::text, :after_feed_entries = :before_feed_entries),
  (14, 'rows unchanged: sleep_entries', :before_sleep_entries::text, :after_sleep_entries::text, :after_sleep_entries = :before_sleep_entries),
  (15, 'rows unchanged: diaper_entries', :before_diaper_entries::text, :after_diaper_entries::text, :after_diaper_entries = :before_diaper_entries),
  (16, 'rows unchanged: supplement_entries (meds)', :before_supplement_entries::text, :after_supplement_entries::text, :after_supplement_entries = :before_supplement_entries),
  (17, 'rows unchanged: growth_profiles', :before_growth_profiles::text, :after_growth_profiles::text, :after_growth_profiles = :before_growth_profiles),
  (18, 'rows unchanged: measurement_entries', :before_measurement_entries::text, :after_measurement_entries::text, :after_measurement_entries = :before_measurement_entries),
  (19, 'rows unchanged: active_timers', :before_active_timers::text, :after_active_timers::text, :after_active_timers = :before_active_timers),
  (20, 'rows unchanged: feed_components (parts)', :before_feed_components::text, :after_feed_components::text, :after_feed_components = :before_feed_components),
  (21, 'rows unchanged: timer_components (parts)', :before_timer_components::text, :after_timer_components::text, :after_timer_components = :before_timer_components),
  (22, 'rows unchanged: mutation_receipts', :before_mutation_receipts::text, :after_mutation_receipts::text, :after_mutation_receipts = :before_mutation_receipts),
  (30, 'every feed v4, every sleep v2, every timer v5 (others)', '0/0/0',
      concat_ws('/', :after_old_feeds, :after_old_sleeps, :after_old_timers), :after_old_feeds + :after_old_sleeps + :after_old_timers = 0),
  (31, 'every live feed has at least one part (feeds without)', '0', :after_feeds_without_parts::text, :after_feeds_without_parts = 0),
  (40, 'journal: payloads with legacy keys', '0', :stored_journal_legacy::text, :stored_journal_legacy = 0),
  (41, 'journal: payloads missing a session or bottle-phase key', '0', :stored_journal_missing_key::text, :stored_journal_missing_key = 0),
  (42, 'journal: solids parts without foods', '0', :stored_journal_solids::text, :stored_journal_solids = 0),
  (43, 'journal: legacy timer operations', '0', :after_legacy_operations::text, :after_legacy_operations = 0),
  (44, 'receipts: entities with legacy keys', '0', :stored_receipts_legacy::text, :stored_receipts_legacy = 0),
  (45, 'receipts: entities missing a session or bottle-phase key', '0', :stored_receipts_missing_key::text, :stored_receipts_missing_key = 0),
  (46, 'receipts: solids parts without foods', '0', :stored_receipts_solids::text, :stored_receipts_solids = 0),
  (50, 'journal rows after = before + appended', (:before_journal_rows + :appended_appended)::text, :after_journal_rows::text,
      :after_journal_rows = :before_journal_rows + :appended_appended),
  (51, 'appended heads = |converted UNION joined UNION relinked| (preflight)', :before_expected_appended::text, :appended_appended::text,
      :appended_appended = :before_expected_appended),
  (52, 'appended heads are exactly converted UNION joined UNION relinked (digest)', left(:'before_appended_digest', 12), left(:'appended_appended_digest', 12),
      :'appended_appended_digest' = :'before_appended_digest'),
  (53, 'joined by 27 = the preflight''s join replay (count)', :before_expected_joined::text, :appended_joined::text,
      :appended_joined = :before_expected_joined),
  (54, 'joined by 27 = the preflight''s join replay (digest)', left(:'before_joined_digest', 12), left(:'appended_joined_digest', 12),
      :'appended_joined_digest' = :'before_joined_digest'),
  (54.1, 'relinked by 27 (moved with their root) = the preflight''s replay (count)', :before_expected_relinked::text, :appended_relinked::text,
      :appended_relinked = :before_expected_relinked),
  (54.2, 'relinked by 27 = the preflight''s replay (digest)', left(:'before_relinked_digest', 12), left(:'appended_relinked_digest', 12),
      :'appended_relinked_digest' = :'before_relinked_digest'),
  (55, 'appended heads are all `update`', '0 others', :appended_not_updates::text, :appended_not_updates = 0),
  (56, 'next_sequence advanced by the appended count (sum)', (:before_next_sequence_sum + :appended_appended)::text, :after_next_sequence_sum::text,
      :after_next_sequence_sum = :before_next_sequence_sum + :appended_appended),
  (57, 'appended heads are each family''s newest sequences', '0 families off', :sequence_families_with_gaps::text, :sequence_families_with_gaps = 0),
  (58, 'every record''s (kind, id, version, last_sequence) is in the journal', '0 missing', :heads_missing::text, :heads_missing = 0),
  (59, 'live feed and sleep session links equal their journal head''s', '0 differing', :links_differing::text, :links_differing = 0),
  (60, 'FORCE RLS on every table with RLS', '0 unforced', :rls_unforced::text, :rls_unforced = 0),
  (61, 'forced tables = before - 3 dropped', (:before_forced_rls_tables - 3)::text, :rls_forced::text, :rls_forced = :before_forced_rls_tables - 3),
  (62, 'FORCE RLS back on the 13 bracketed tables', '13', :rls_bracket_forced::text, :rls_bracket_forced = 13),
  (63, 'legacy tables, columns and functions dropped', '0 left', :schema_legacy_storage_left::text, :schema_legacy_storage_left = 0),
  (64, 'manifest rows for 27 = rows for 26 minus 3 (and same rows)', (:manifest_rows_26 - 3)::text || ' / 0 differing',
      :manifest_rows_27::text || ' / ' || :manifest_differing::text, :manifest_rows_27 = :manifest_rows_26 - 3 AND :manifest_differing = 0)
) AS checks(n, invariant, expected, actual, ok)
\gset postflight_

\echo
\echo '-- Invariants (spec S-4)'
\echo :postflight_report

\echo
\echo '-- Coverage: 000027 rewrote the journal and the receipt outcomes (canonical and related entities).'
\echo '-- By design it does not rewrite import_chunks or import_conflict_receipts: they keep the bytes a'
\echo '-- client uploaded and are replayed only to that sender. Rows there (counts only):'
SELECT (SELECT count(*) FROM public.import_chunks) AS import_chunks,
       (SELECT count(*) FROM public.import_conflict_receipts) AS import_conflict_receipts;
\endif

-- ---------------------------------------------------------------------------
-- Sessions of the last 14 days, Europe/Zurich. A session belongs to the day
-- of its first part. `gaps` is each further part's start minus the latest end
-- of the parts before it. `linked`: who linked the parts.
-- ---------------------------------------------------------------------------
\echo
\echo '-- Sessions, last 14 days (Europe/Zurich): feeds and sleeps, parts and gaps'
WITH migration AS (
  SELECT recorded_at AS at FROM public.schema_compatibility WHERE singleton AND current_version = 27
), joined_by_27 AS (
  SELECT j.family_id, j.entity_kind, j.entity_id
    FROM public.mutation_journal j, migration
   WHERE j.accepted_at = migration.at AND j.entity_kind IN ('feed', 'sleep')
     AND (j.canonical_entity->'payload'->>CASE j.entity_kind WHEN 'feed' THEN 'feeding_session_id' ELSE 'sleep_session_id' END) IS NOT NULL
     AND (SELECT p.canonical_entity->'payload'->>CASE j.entity_kind WHEN 'feed' THEN 'feeding_session_id' ELSE 'sleep_session_id' END
            FROM public.mutation_journal p
           WHERE p.family_id = j.family_id AND p.entity_kind = j.entity_kind AND p.entity_id = j.entity_id AND p.sequence < j.sequence
           ORDER BY p.sequence DESC LIMIT 1) IS NULL
), baby AS (
  SELECT family_id, id, 'baby ' || row_number() OVER (ORDER BY family_id, sort_order, id) AS label FROM public.babies
), part AS (
  SELECT 'feed' AS type, f.family_id, f.baby_id, f.id, coalesce(f.feeding_session_id, f.id) AS root,
         f.started_at, f.ended_at, f.kind AS part_kind FROM public.feed_entries f WHERE f.tombstoned_at IS NULL
  UNION ALL
  SELECT 'sleep', s.family_id, s.baby_id, s.id, coalesce(s.sleep_session_id, s.id),
         s.started_at, s.ended_at, NULL FROM public.sleep_entries s WHERE s.tombstoned_at IS NULL
), ordered AS (
  SELECT part.*, EXISTS (SELECT FROM joined_by_27 j WHERE j.family_id = part.family_id AND j.entity_kind = part.type
                           AND j.entity_id = part.id) AS joined,
         extract(epoch FROM part.started_at - max(coalesce(part.ended_at, part.started_at)) OVER (
           PARTITION BY part.family_id, part.type, part.root ORDER BY part.started_at, part.id
           ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING))::bigint AS gap_seconds
    FROM part
), session AS (
  SELECT family_id, type, root, min(baby_id::text)::uuid AS baby_id, min(started_at) AS started_at,
         max(coalesce(ended_at, started_at)) AS ended_at, bool_or(ended_at IS NULL) AND type = 'sleep' AS open,
         count(*) AS parts, string_agg(part_kind, '+' ORDER BY started_at, id) AS kinds,
         string_agg(CASE WHEN gap_seconds % 60 = 0 THEN (gap_seconds / 60)::text ELSE round(gap_seconds / 60.0, 1)::text END || 'm',
                    ' ' ORDER BY started_at, id) FILTER (WHERE gap_seconds IS NOT NULL) AS gaps,
         count(*) FILTER (WHERE joined) AS joined
    FROM ordered GROUP BY family_id, type, root
)
SELECT to_char(session.started_at AT TIME ZONE 'Europe/Zurich', 'YYYY-MM-DD Dy') AS day, baby.label AS baby, session.type,
       to_char(session.started_at AT TIME ZONE 'Europe/Zurich', 'HH24:MI') AS start,
       CASE WHEN session.open THEN 'open'
            ELSE to_char(session.ended_at AT TIME ZONE 'Europe/Zurich', 'HH24:MI')
              || CASE WHEN (session.ended_at AT TIME ZONE 'Europe/Zurich')::date > (session.started_at AT TIME ZONE 'Europe/Zurich')::date
                      THEN ' +1d' ELSE '' END END AS "end",
       session.parts, session.kinds AS feed_parts, session.gaps,
       CASE WHEN session.parts = 1 THEN ''
            WHEN session.joined = session.parts - 1 THEN 'joined by 27'
            WHEN session.joined = 0 THEN 'already linked'
            ELSE 'joined by 27 + already linked' END AS linked
  FROM session JOIN baby ON baby.family_id = session.family_id AND baby.id = session.baby_id
 WHERE (session.started_at AT TIME ZONE 'Europe/Zurich')::date >= (now() AT TIME ZONE 'Europe/Zurich')::date - 13
 ORDER BY session.started_at, baby.label, session.type;

COMMIT;

\if :{?listing_only}
\elif :postflight_ok
\echo 'POSTFLIGHT: PASS (every invariant holds)'
\else
\echo 'POSTFLIGHT: FAIL -' :postflight_failed 'invariant(s) failed, see the FAIL lines above'
DO $$ BEGIN RAISE EXCEPTION 'format_cleanup_postflight: invariant(s) failed'; END $$;
\endif
