-- Format cleanup preflight (migration 000027), read-only.
--
-- Run as `postgres` (which bypasses row-level security) against a schema-26
-- database: production before the deploy (runbook S-2), or the restored copy
-- inside format-cleanup-dry-run.sh (S-4).
--
--   psql -X -v ON_ERROR_STOP=1 -U postgres -d babytracker -f format-cleanup-preflight.sql
--
-- Prints counts only: never names, notes, food text or ids. The two digests
-- are md5 sums over sorted entity keys, used only to prove set equality in the
-- postflight. Every must-be-0 check prints PASS or FAIL; any FAIL ends the run
-- with a non-zero exit after the report.
--
-- The last line, `FORMAT_CLEANUP_BEFORE ...`, carries the numbers the
-- postflight compares against; pass it there as `-v before="..."`.
\set QUIET 1
\set ON_ERROR_STOP 1
\pset footer off
\pset null '-'
SET client_min_messages = warning;
BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;

\echo '== Format cleanup preflight (read-only) =='
SELECT (SELECT current_version FROM public.schema_compatibility WHERE singleton) AS schema_version,
       (SELECT max(version) FROM public.schema_migrations) AS last_migration,
       to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS read_at;

\echo
\echo '-- Rows per table (all families)'
SELECT tables.name AS "table", tables.total, tables.live, tables.total - tables.live AS tombstoned
FROM (
  SELECT 1 n, 'families' name, count(*) total, count(*) FILTER (WHERE deleted_at IS NULL) live FROM public.families
  UNION ALL SELECT 2, 'memberships', count(*), count(*) FILTER (WHERE status = 'active') FROM public.memberships
  UNION ALL SELECT 3, 'babies', count(*), count(*) FILTER (WHERE tombstoned_at IS NULL) FROM public.babies
  UNION ALL SELECT 4, 'feed_entries', count(*), count(*) FILTER (WHERE tombstoned_at IS NULL) FROM public.feed_entries
  UNION ALL SELECT 5, 'sleep_entries', count(*), count(*) FILTER (WHERE tombstoned_at IS NULL) FROM public.sleep_entries
  UNION ALL SELECT 6, 'diaper_entries', count(*), count(*) FILTER (WHERE tombstoned_at IS NULL) FROM public.diaper_entries
  UNION ALL SELECT 7, 'supplement_entries', count(*), count(*) FILTER (WHERE tombstoned_at IS NULL) FROM public.supplement_entries
  UNION ALL SELECT 8, 'growth_profiles', count(*), count(*) FILTER (WHERE tombstoned_at IS NULL) FROM public.growth_profiles
  UNION ALL SELECT 9, 'measurement_entries', count(*), count(*) FILTER (WHERE tombstoned_at IS NULL) FROM public.measurement_entries
  UNION ALL SELECT 10, 'active_timers', count(*), count(*) FILTER (WHERE tombstoned_at IS NULL) FROM public.active_timers
  UNION ALL SELECT 11, 'feed_components', count(*), count(*) FROM public.feed_components
  UNION ALL SELECT 12, 'timer_components', count(*), count(*) FROM public.timer_components
  UNION ALL SELECT 13, 'mutation_journal', count(*), count(*) FROM public.mutation_journal
  UNION ALL SELECT 14, 'mutation_receipts', count(*), count(*) FROM public.mutation_receipts
) tables ORDER BY tables.n;

\echo
\echo '-- Records per stored version (feed 4, sleep 2 and timer 5 are current)'
SELECT kind, version, count(*) AS total, count(*) FILTER (WHERE live) AS live, count(*) FILTER (WHERE live AND linked) AS live_linked
FROM (
  SELECT 'feed' kind, component_version version, tombstoned_at IS NULL live, feeding_session_id IS NOT NULL linked FROM public.feed_entries
  UNION ALL SELECT 'sleep', payload_version, tombstoned_at IS NULL, sleep_session_id IS NOT NULL FROM public.sleep_entries
  UNION ALL SELECT 'timer', component_version, tombstoned_at IS NULL, session_id IS NOT NULL FROM public.active_timers
) records GROUP BY kind, version ORDER BY kind, version;

\echo
\echo '-- Journal rows per kind and operation'
SELECT entity_kind AS kind, operation, count(*) AS rows FROM public.mutation_journal GROUP BY 1, 2 ORDER BY 1, 2;

-- Shape class of one stored canonical entity, from the keys its payload holds.
\echo
\echo '-- Stored payload shapes, journal and receipts (receipts: canonical and related entities)'
SELECT source, shape, count(*) AS entities
FROM (
  SELECT 'journal' source, j.canonical_entity entity FROM public.mutation_journal j
  UNION ALL
  SELECT 'receipts', r.outcome->'canonical_entity' FROM public.mutation_receipts r WHERE jsonb_typeof(r.outcome->'canonical_entity') = 'object'
  UNION ALL
  SELECT 'receipts', related.entity FROM public.mutation_receipts r,
    jsonb_array_elements(CASE WHEN jsonb_typeof(r.outcome->'related_entities') = 'array' THEN r.outcome->'related_entities' ELSE '[]'::jsonb END) related(entity)
) stored
CROSS JOIN LATERAL (SELECT entity->>'entity_kind' AS kind, entity->'payload' AS payload) e
CROSS JOIN LATERAL (SELECT CASE
  WHEN jsonb_typeof(e.payload) IS DISTINCT FROM 'object' THEN e.kind || ': tombstone'
  WHEN e.kind = 'feed' THEN CASE
    WHEN e.payload ?| ARRAY['sides','volume_ml','contents','foods','amount'] OR NOT e.payload ? 'components' THEN 'feed: LEGACY scalar'
    WHEN e.payload ? 'feeding_session_id' THEN 'feed: v4 (session key)'
    ELSE 'feed: v2/v3 (components, no session key)' END
  WHEN e.kind = 'timer' THEN CASE
    WHEN e.payload ?| ARRAY['completed_sides','pending_volume_ml','pending_contents'] OR NOT e.payload ? 'completed_components' THEN 'timer: LEGACY completed_sides'
    WHEN e.payload ? 'session_id' THEN 'timer: v5 (session key)'
    WHEN e.payload ? 'current_bottle_started_at' THEN 'timer: v3/v4 (bottle phase, no session key)'
    ELSE 'timer: v2 (no bottle phase)' END
  WHEN e.kind = 'sleep' THEN CASE WHEN e.payload ? 'sleep_session_id' THEN 'sleep: v2 (session key)' ELSE 'sleep: v1 (no session key)' END
  ELSE e.kind || ': current' END AS shape) classified
GROUP BY source, shape ORDER BY source, shape;

-- ---------------------------------------------------------------------------
-- Must-be-0 checks. The legacy-shape predicate is migration 000027's own
-- (format_cleanup_is_legacy); a solids part must carry `foods` (B-R12).
-- ---------------------------------------------------------------------------
SELECT count(*) AS v1_feeds FROM public.feed_entries WHERE component_version = 1 \gset pf_
SELECT count(*) AS v1_timers FROM public.active_timers WHERE component_version = 1 \gset pf_
SELECT count(*) AS legacy_journal FROM public.mutation_journal j
 WHERE coalesce(jsonb_typeof(j.canonical_entity->'payload') = 'object' AND CASE j.canonical_entity->>'entity_kind'
   WHEN 'feed' THEN (j.canonical_entity->'payload') ?| ARRAY['sides','volume_ml','contents','foods','amount'] OR NOT (j.canonical_entity->'payload') ? 'components'
   WHEN 'timer' THEN (j.canonical_entity->'payload') ?| ARRAY['completed_sides','pending_volume_ml','pending_contents'] OR NOT (j.canonical_entity->'payload') ? 'completed_components'
   ELSE false END, false) \gset pf_
SELECT count(*) AS legacy_receipts FROM public.mutation_receipts r
 WHERE EXISTS (
   SELECT FROM (SELECT r.outcome->'canonical_entity' WHERE jsonb_typeof(r.outcome->'canonical_entity') = 'object'
                UNION ALL
                SELECT related.entity FROM jsonb_array_elements(CASE WHEN jsonb_typeof(r.outcome->'related_entities') = 'array'
                  THEN r.outcome->'related_entities' ELSE '[]'::jsonb END) related(entity)) stored(entity)
   WHERE coalesce(jsonb_typeof(stored.entity->'payload') = 'object' AND CASE stored.entity->>'entity_kind'
     WHEN 'feed' THEN (stored.entity->'payload') ?| ARRAY['sides','volume_ml','contents','foods','amount'] OR NOT (stored.entity->'payload') ? 'components'
     WHEN 'timer' THEN (stored.entity->'payload') ?| ARRAY['completed_sides','pending_volume_ml','pending_contents'] OR NOT (stored.entity->'payload') ? 'completed_components'
     ELSE false END, false)) \gset pf_
SELECT (SELECT count(*) FROM public.feed_components WHERE kind = 'solids' AND foods IS NULL)
     + (SELECT count(*) FROM public.timer_components WHERE kind = 'solids' AND foods IS NULL) AS solids_without_foods_rows \gset pf_
SELECT count(*) AS solids_without_foods_journal FROM public.mutation_journal j,
  jsonb_array_elements(CASE
    WHEN jsonb_typeof(j.canonical_entity->'payload'->'components') = 'array' THEN j.canonical_entity->'payload'->'components'
    WHEN jsonb_typeof(j.canonical_entity->'payload'->'completed_components') = 'array' THEN j.canonical_entity->'payload'->'completed_components'
    ELSE '[]'::jsonb END) part
 WHERE part->>'kind' = 'solids' AND jsonb_typeof(part->'foods') IS DISTINCT FROM 'array' \gset pf_
SELECT count(*) AS solids_without_foods_receipts FROM public.mutation_receipts r,
  LATERAL (SELECT r.outcome->'canonical_entity' WHERE jsonb_typeof(r.outcome->'canonical_entity') = 'object'
           UNION ALL
           SELECT related.entity FROM jsonb_array_elements(CASE WHEN jsonb_typeof(r.outcome->'related_entities') = 'array'
             THEN r.outcome->'related_entities' ELSE '[]'::jsonb END) related(entity)) stored(entity),
  jsonb_array_elements(CASE
    WHEN jsonb_typeof(stored.entity->'payload'->'components') = 'array' THEN stored.entity->'payload'->'components'
    WHEN jsonb_typeof(stored.entity->'payload'->'completed_components') = 'array' THEN stored.entity->'payload'->'completed_components'
    ELSE '[]'::jsonb END) part
 WHERE part->>'kind' = 'solids' AND jsonb_typeof(part->'foods') IS DISTINCT FROM 'array' \gset pf_
-- Same predicate as app.import_unfinished_session_count (000012).
SELECT count(*) AS unfinished_imports FROM public.import_sessions
 WHERE NOT mapping_complete AND expired_at IS NULL AND expires_at > now() \gset pf_
SELECT count(*) AS running_timers_below_v5 FROM public.active_timers WHERE tombstoned_at IS NULL AND component_version < 5 \gset pf_
SELECT count(*) AS live_feeds_without_parts FROM public.feed_entries f
 WHERE f.tombstoned_at IS NULL
   AND NOT EXISTS (SELECT FROM public.feed_components c WHERE c.family_id = f.family_id AND c.feed_id = f.id) \gset pf_
SELECT (SELECT current_version FROM public.schema_compatibility WHERE singleton) AS schema_version \gset pf_

-- ---------------------------------------------------------------------------
-- Expected changes. Converted: live records below their kind's current
-- version. Joined: 000027's join replayed without writing (rulings B-R3,
-- B-R9 cutoff 2026-10-02T16:37:50Z, B-R11 equal-end tie-break, B-R13 a
-- joining root takes its whole session along). Relinked: the linked parts
-- that moved with their root. The loops are copies of the migration's,
-- reading each earlier change from a map instead of the updated row.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    cutoff constant timestamptz := timestamptz '2026-10-02T16:37:50Z';
    joined jsonb := '{}';
    relinked jsonb := '{}';
    -- Every link the replay changed so far, joined or relinked.
    roots jsonb := '{}';
    moved jsonb;
    part record;
    previous record;
BEGIN
    FOR part IN
        SELECT e.family_id, e.baby_id, e.id, e.started_at
          FROM public.feed_entries e
         WHERE e.tombstoned_at IS NULL AND e.kind <> 'solids' AND e.feeding_session_id IS NULL
           AND e.started_at < cutoff
         ORDER BY e.family_id, e.baby_id, e.started_at, e.id
    LOOP
        CONTINUE WHEN EXISTS (SELECT FROM public.active_timers timer
                               WHERE timer.family_id = part.family_id AND timer.session_id = part.id
                                 AND timer.tombstoned_at IS NULL);
        SELECT candidate.root, candidate.ended_at INTO previous
          FROM (SELECT coalesce((roots->>('feed:' || earlier.family_id || ':' || earlier.id))::uuid,
                                earlier.feeding_session_id, earlier.id) AS root,
                       min(earlier.started_at) AS started_at,
                       max(coalesce(earlier.ended_at, earlier.started_at)) AS ended_at
                  FROM public.feed_entries earlier
                 WHERE earlier.family_id = part.family_id AND earlier.baby_id = part.baby_id
                   AND earlier.tombstoned_at IS NULL AND earlier.kind <> 'solids'
                   AND (earlier.started_at, earlier.id) < (part.started_at, part.id)
                 GROUP BY 1) candidate
         WHERE candidate.ended_at <= part.started_at AND candidate.root <> part.id
         ORDER BY candidate.ended_at DESC, candidate.started_at, candidate.root
         LIMIT 1;
        CONTINUE WHEN NOT FOUND OR part.started_at - previous.ended_at > interval '1800 seconds';
        CONTINUE WHEN EXISTS (SELECT FROM public.sleep_entries sleep
                               WHERE sleep.family_id = part.family_id AND sleep.baby_id = part.baby_id
                                 AND sleep.tombstoned_at IS NULL
                                 AND sleep.started_at < part.started_at
                                 AND coalesce(sleep.ended_at, now()) > previous.ended_at)
                   OR EXISTS (SELECT FROM public.active_timers timer
                               WHERE timer.family_id = part.family_id AND timer.baby_id = part.baby_id
                                 AND timer.tombstoned_at IS NULL AND timer.timer_kind = 'sleep'
                                 AND timer.started_at < part.started_at);
        joined := joined || jsonb_build_object('feed:' || part.family_id || ':' || part.id, previous.root);
        SELECT coalesce(jsonb_object_agg('feed:' || member.family_id || ':' || member.id, previous.root), '{}') INTO moved
          FROM public.feed_entries member
         WHERE member.family_id = part.family_id AND member.tombstoned_at IS NULL
           AND coalesce((roots->>('feed:' || member.family_id || ':' || member.id))::uuid, member.feeding_session_id) = part.id;
        relinked := relinked || moved;
        roots := roots || jsonb_build_object('feed:' || part.family_id || ':' || part.id, previous.root) || moved;
    END LOOP;

    FOR part IN
        SELECT e.family_id, e.baby_id, e.id, e.started_at
          FROM public.sleep_entries e
         WHERE e.tombstoned_at IS NULL AND e.sleep_session_id IS NULL
           AND e.started_at < cutoff
         ORDER BY e.family_id, e.baby_id, e.started_at, e.id
    LOOP
        CONTINUE WHEN EXISTS (SELECT FROM public.active_timers timer
                               WHERE timer.family_id = part.family_id AND timer.session_id = part.id
                                 AND timer.tombstoned_at IS NULL);
        SELECT candidate.root, candidate.ended_at INTO previous
          FROM (SELECT coalesce((roots->>('sleep:' || earlier.family_id || ':' || earlier.id))::uuid,
                                earlier.sleep_session_id, earlier.id) AS root,
                       min(earlier.started_at) AS started_at,
                       max(earlier.ended_at) AS ended_at,
                       count(*) = count(earlier.ended_at) AS closed
                  FROM public.sleep_entries earlier
                 WHERE earlier.family_id = part.family_id AND earlier.baby_id = part.baby_id
                   AND earlier.tombstoned_at IS NULL
                   AND (earlier.started_at, earlier.id) < (part.started_at, part.id)
                 GROUP BY 1) candidate
         WHERE candidate.closed AND candidate.ended_at <= part.started_at AND candidate.root <> part.id
         ORDER BY candidate.ended_at DESC, candidate.started_at, candidate.root
         LIMIT 1;
        CONTINUE WHEN NOT FOUND OR part.started_at - previous.ended_at > interval '900 seconds';
        joined := joined || jsonb_build_object('sleep:' || part.family_id || ':' || part.id, previous.root);
        SELECT coalesce(jsonb_object_agg('sleep:' || member.family_id || ':' || member.id, previous.root), '{}') INTO moved
          FROM public.sleep_entries member
         WHERE member.family_id = part.family_id AND member.tombstoned_at IS NULL
           AND coalesce((roots->>('sleep:' || member.family_id || ':' || member.id))::uuid, member.sleep_session_id) = part.id;
        relinked := relinked || moved;
        roots := roots || jsonb_build_object('sleep:' || part.family_id || ':' || part.id, previous.root) || moved;
    END LOOP;
    -- Kept for the queries below, inside this read-only transaction only.
    PERFORM set_config('format_cleanup.joined', joined::text, true);
    PERFORM set_config('format_cleanup.relinked', relinked::text, true);
END
$$;

-- Keys are kind:family:id, so the digests compare exact entity sets.
WITH converted AS (
  SELECT 'feed:' || family_id || ':' || id AS key FROM public.feed_entries WHERE tombstoned_at IS NULL AND component_version < 4
  UNION ALL SELECT 'sleep:' || family_id || ':' || id FROM public.sleep_entries WHERE tombstoned_at IS NULL AND payload_version < 2
  UNION ALL SELECT 'timer:' || family_id || ':' || id FROM public.active_timers WHERE tombstoned_at IS NULL AND component_version < 5
), joined AS (
  SELECT key FROM jsonb_object_keys(current_setting('format_cleanup.joined')::jsonb) key
), relinked AS (
  SELECT key FROM jsonb_object_keys(current_setting('format_cleanup.relinked')::jsonb) key
), appended AS (
  SELECT key FROM converted UNION SELECT key FROM joined UNION SELECT key FROM relinked
)
SELECT (SELECT count(*) FROM converted WHERE key LIKE 'feed:%') AS converted_feeds,
       (SELECT count(*) FROM converted WHERE key LIKE 'sleep:%') AS converted_sleeps,
       (SELECT count(*) FROM converted WHERE key LIKE 'timer:%') AS converted_timers,
       (SELECT count(*) FROM converted) AS converted,
       (SELECT count(*) FROM joined WHERE key LIKE 'feed:%') AS joined_feeds,
       (SELECT count(*) FROM joined WHERE key LIKE 'sleep:%') AS joined_sleeps,
       (SELECT count(*) FROM joined) AS joined,
       (SELECT count(*) FROM joined WHERE key NOT IN (SELECT key FROM converted)) AS joined_not_converted,
       (SELECT count(*) FROM relinked) AS relinked,
       (SELECT count(*) FROM appended) AS appended,
       (SELECT md5(coalesce(string_agg(key, ',' ORDER BY key), '')) FROM joined) AS joined_digest,
       (SELECT md5(coalesce(string_agg(key, ',' ORDER BY key), '')) FROM relinked) AS relinked_digest,
       (SELECT md5(coalesce(string_agg(key, ',' ORDER BY key), '')) FROM appended) AS appended_digest
\gset expected_
\echo
\echo '-- Expected changes by 000027 (appended journal heads = converted UNION joined UNION relinked, distinct)'
SELECT :expected_converted_feeds AS converted_feeds, :expected_converted_sleeps AS converted_sleeps,
       :expected_converted_timers AS converted_timers, :expected_joined_feeds AS joined_feeds,
       :expected_joined_sleeps AS joined_sleeps, :expected_joined_not_converted AS joined_not_converted,
       :expected_relinked AS relinked, :expected_appended AS appended_heads;

\echo
\echo '-- Checks (each must be 0, schema must be 26)'
SELECT CASE WHEN ok THEN 'PASS' ELSE 'FAIL' END AS result, check_name AS "check", found
FROM (VALUES
  (1, 'schema version is 26', :pf_schema_version::bigint, :pf_schema_version = 26),
  (2, 'feeds at component_version 1', :pf_v1_feeds::bigint, :pf_v1_feeds = 0),
  (3, 'timers at component_version 1', :pf_v1_timers::bigint, :pf_v1_timers = 0),
  (4, 'journal rows with a scalar feed or completed_sides timer payload', :pf_legacy_journal::bigint, :pf_legacy_journal = 0),
  (5, 'receipts with a scalar feed or completed_sides timer payload', :pf_legacy_receipts::bigint, :pf_legacy_receipts = 0),
  (6, 'solids parts without foods: feed/timer component rows', :pf_solids_without_foods_rows::bigint, :pf_solids_without_foods_rows = 0),
  (7, 'solids parts without foods: journal', :pf_solids_without_foods_journal::bigint, :pf_solids_without_foods_journal = 0),
  (8, 'solids parts without foods: receipts', :pf_solids_without_foods_receipts::bigint, :pf_solids_without_foods_receipts = 0),
  (9, 'unfinished imports', :pf_unfinished_imports::bigint, :pf_unfinished_imports = 0),
  (10, 'running timers below component_version 5', :pf_running_timers_below_v5::bigint, :pf_running_timers_below_v5 = 0),
  (11, 'live feeds without parts', :pf_live_feeds_without_parts::bigint, :pf_live_feeds_without_parts = 0)
) AS checks(n, check_name, found, ok) ORDER BY n;

SELECT (:pf_schema_version = 26 AND :pf_v1_feeds = 0 AND :pf_v1_timers = 0 AND :pf_legacy_journal = 0
        AND :pf_legacy_receipts = 0 AND :pf_solids_without_foods_rows = 0 AND :pf_solids_without_foods_journal = 0
        AND :pf_solids_without_foods_receipts = 0 AND :pf_unfinished_imports = 0
        AND :pf_running_timers_below_v5 = 0 AND :pf_live_feeds_without_parts = 0) AS preflight_ok \gset

-- The numbers the postflight compares against.
\pset tuples_only on
\pset format unaligned
SELECT 'FORMAT_CLEANUP_BEFORE'
  || ' families=' || (SELECT count(*) FROM public.families)
  || ' memberships=' || (SELECT count(*) FROM public.memberships)
  || ' babies=' || (SELECT count(*) FROM public.babies)
  || ' feed_entries=' || (SELECT count(*) FROM public.feed_entries)
  || ' sleep_entries=' || (SELECT count(*) FROM public.sleep_entries)
  || ' diaper_entries=' || (SELECT count(*) FROM public.diaper_entries)
  || ' supplement_entries=' || (SELECT count(*) FROM public.supplement_entries)
  || ' growth_profiles=' || (SELECT count(*) FROM public.growth_profiles)
  || ' measurement_entries=' || (SELECT count(*) FROM public.measurement_entries)
  || ' active_timers=' || (SELECT count(*) FROM public.active_timers)
  || ' feed_components=' || (SELECT count(*) FROM public.feed_components)
  || ' timer_components=' || (SELECT count(*) FROM public.timer_components)
  || ' mutation_receipts=' || (SELECT count(*) FROM public.mutation_receipts)
  || ' journal_rows=' || (SELECT count(*) FROM public.mutation_journal)
  || ' next_sequence_sum=' || (SELECT coalesce(sum(next_sequence), 0) FROM public.families)
  || ' forced_rls_tables=' || (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
                               WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relforcerowsecurity)
  || ' expected_appended=' || :expected_appended
  || ' expected_joined=' || :expected_joined
  || ' expected_relinked=' || :expected_relinked
  || ' appended_digest=' || :'expected_appended_digest'
  || ' joined_digest=' || :'expected_joined_digest'
  || ' relinked_digest=' || :'expected_relinked_digest';
\pset format aligned
\pset tuples_only off
COMMIT;

\if :preflight_ok
\echo 'PREFLIGHT: PASS'
\else
\echo 'PREFLIGHT: FAIL (see the FAIL lines above; migration 000027 would refuse or the dry-run must stop)'
DO $$ BEGIN RAISE EXCEPTION 'format_cleanup_preflight: a must-be-0 check failed'; END $$;
\endif
