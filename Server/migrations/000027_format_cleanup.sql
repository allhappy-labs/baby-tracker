-- Format cleanup: every stored record and every row of sync history is put in
-- the current format (feed 4, timer 5, sleep 2, every other kind 1).
--
-- Not overlap-safe. The predecessor binary allows schema 26 at most, so the app
-- stays stopped while `migrate` runs, as it did for the 22 -> 26 cutover.
--
-- The migrate runner applies this file in one transaction, as the table-owning
-- migrator. That role has no BYPASSRLS and these tables FORCE row-level
-- security, so with no tenant context every statement below would match no
-- rows and the migration would "succeed" without converting anything. The
-- bracket at the top lifts FORCE for this transaction only; the bottom puts it
-- back before commit, so no other session ever sees the gap. The side and food
-- tables this file drops are never read or written here, so they stay out of
-- the bracket.

-- ---------------------------------------------------------------------------
-- RLS bracket: open.
-- ---------------------------------------------------------------------------
ALTER TABLE public.families NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.feed_entries NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.feed_components NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.sleep_entries NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.active_timers NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.timer_components NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.mutation_journal NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.mutation_receipts NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.sync_page_sessions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.sync_snapshots NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.sync_snapshot_entities NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.sync_snapshot_admissions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.import_sessions NO FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (
        SELECT FROM unnest(ARRAY[
            'public.families','public.feed_entries','public.feed_components','public.sleep_entries',
            'public.active_timers','public.timer_components','public.mutation_journal','public.mutation_receipts',
            'public.sync_page_sessions','public.sync_snapshots','public.sync_snapshot_entities',
            'public.sync_snapshot_admissions','public.import_sessions']::regclass[]) AS bracketed(table_oid)
        WHERE row_security_active(bracketed.table_oid)
    ) THEN
        RAISE EXCEPTION 'format_cleanup: row-level security still filters the migrator; refusing a partial conversion';
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- Stored-shape helpers, dropped again before commit. A canonical entity's
-- payload is converted by adding the keys the current marshal always writes
-- (`defaults || payload` keeps every key the payload already has).
-- ---------------------------------------------------------------------------
CREATE FUNCTION pg_temp.format_cleanup_is_legacy(entity jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT coalesce(jsonb_typeof(entity->'payload') = 'object' AND CASE entity->>'entity_kind'
        WHEN 'feed' THEN (entity->'payload') ?| ARRAY['sides','volume_ml','contents','foods','amount']
            OR NOT (entity->'payload') ? 'components'
        WHEN 'timer' THEN (entity->'payload') ?| ARRAY['completed_sides','pending_volume_ml','pending_contents']
            OR NOT (entity->'payload') ? 'completed_components'
        ELSE false END, false)
$$;

CREATE FUNCTION pg_temp.format_cleanup_receipt_entities(outcome jsonb) RETURNS SETOF jsonb
LANGUAGE sql IMMUTABLE AS $$
    SELECT outcome->'canonical_entity' WHERE jsonb_typeof(outcome->'canonical_entity') = 'object'
    UNION ALL
    SELECT related.entity FROM jsonb_array_elements(CASE WHEN jsonb_typeof(outcome->'related_entities') = 'array'
        THEN outcome->'related_entities' ELSE '[]'::jsonb END) AS related(entity)
$$;

CREATE FUNCTION pg_temp.format_cleanup_entity(entity jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE
        WHEN jsonb_typeof(entity->'payload') IS DISTINCT FROM 'object' THEN entity
        WHEN entity->>'entity_kind' = 'feed' THEN jsonb_set(entity, '{payload}',
            jsonb_build_object('feeding_session_id', NULL) || (entity->'payload'))
        WHEN entity->>'entity_kind' = 'timer' THEN jsonb_set(entity, '{payload}',
            jsonb_build_object('current_bottle_started_at', NULL, 'session_id', NULL) || (entity->'payload'))
        WHEN entity->>'entity_kind' = 'sleep' THEN jsonb_set(entity, '{payload}',
            jsonb_build_object('sleep_session_id', NULL) || (entity->'payload'))
        ELSE entity END
$$;

CREATE FUNCTION pg_temp.format_cleanup_outcome(outcome jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE AS $$
    SELECT outcome
        || CASE WHEN jsonb_typeof(outcome->'canonical_entity') = 'object'
            THEN jsonb_build_object('canonical_entity', pg_temp.format_cleanup_entity(outcome->'canonical_entity'))
            ELSE '{}'::jsonb END
        || CASE WHEN jsonb_typeof(outcome->'related_entities') = 'array'
            THEN jsonb_build_object('related_entities', (
                SELECT coalesce(jsonb_agg(pg_temp.format_cleanup_entity(related.entity) ORDER BY related.position), '[]'::jsonb)
                  FROM jsonb_array_elements(outcome->'related_entities') WITH ORDINALITY AS related(entity, position)))
            ELSE '{}'::jsonb END
$$;

-- ---------------------------------------------------------------------------
-- Preconditions (ruling B-R2: refuse, never rebuild or skip). Each runs after
-- the bracket, so it sees every family's rows.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    found bigint;
BEGIN
    -- Same predicate as app.import_unfinished_session_count (000012).
    SELECT count(*) INTO found FROM public.import_sessions
     WHERE NOT mapping_complete AND expired_at IS NULL AND expires_at > now();
    IF found > 0 THEN
        RAISE EXCEPTION 'format_cleanup_precondition: % unfinished import(s); finish or let them expire first', found;
    END IF;

    SELECT count(*) INTO found FROM public.feed_entries WHERE component_version = 1;
    IF found > 0 THEN
        RAISE EXCEPTION 'format_cleanup_precondition: % feed(s) at component_version 1 need a component rebuild', found;
    END IF;

    SELECT count(*) INTO found FROM public.active_timers WHERE component_version = 1;
    IF found > 0 THEN
        RAISE EXCEPTION 'format_cleanup_precondition: % timer(s) at component_version 1 need a component rebuild', found;
    END IF;

    -- A live timer row is a running (or paused) timer: stopping one tombstones it.
    SELECT count(*) INTO found FROM public.active_timers WHERE tombstoned_at IS NULL AND component_version < 5;
    IF found > 0 THEN
        RAISE EXCEPTION 'format_cleanup_precondition: % running timer(s) below component_version 5; stop them first', found;
    END IF;

    SELECT count(*) INTO found FROM public.feed_entries f
     WHERE f.tombstoned_at IS NULL
       AND NOT EXISTS (SELECT FROM public.feed_components c WHERE c.family_id = f.family_id AND c.feed_id = f.id);
    IF found > 0 THEN
        RAISE EXCEPTION 'format_cleanup_precondition: % live feed(s) without components', found;
    END IF;

    SELECT count(*) INTO found FROM public.mutation_journal
     WHERE pg_temp.format_cleanup_is_legacy(canonical_entity);
    IF found > 0 THEN
        RAISE EXCEPTION 'format_cleanup_precondition: % journal row(s) hold a scalar feed or completed_sides timer payload', found;
    END IF;

    SELECT count(*) INTO found FROM public.mutation_receipts r
     WHERE EXISTS (SELECT FROM pg_temp.format_cleanup_receipt_entities(r.outcome) AS entity
                    WHERE pg_temp.format_cleanup_is_legacy(entity));
    IF found > 0 THEN
        RAISE EXCEPTION 'format_cleanup_precondition: % receipt(s) hold a scalar feed or completed_sides timer payload', found;
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- Legacy projection triggers. Every write the server still accepts is feed v4
-- or timer v5: putFeed and putTimer write the component rows directly, and a
-- tombstone deletes them, so nothing depends on the scalar -> component
-- projection any more.
-- ---------------------------------------------------------------------------
DROP TRIGGER feed_components_from_parent ON public.feed_entries;
DROP TRIGGER feed_components_from_sides ON public.feed_side_intervals;
DROP TRIGGER feed_components_from_foods ON public.feed_foods;
DROP TRIGGER timer_components_from_parent ON public.active_timers;
DROP TRIGGER timer_components_from_sides ON public.timer_side_intervals;

-- ---------------------------------------------------------------------------
-- Live entities whose stored format changes here. Later steps of this file
-- append one journal head for each of them.
-- ---------------------------------------------------------------------------
CREATE TEMP TABLE format_cleanup_converted (
    kind_order integer NOT NULL,
    entity_kind text NOT NULL,
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    PRIMARY KEY (family_id, entity_kind, id)
) ON COMMIT DROP;
INSERT INTO format_cleanup_converted(kind_order, entity_kind, family_id, id)
SELECT 2, 'feed', family_id, id FROM public.feed_entries WHERE tombstoned_at IS NULL AND component_version < 4
UNION ALL
SELECT 3, 'sleep', family_id, id FROM public.sleep_entries WHERE tombstoned_at IS NULL AND payload_version < 2
UNION ALL
SELECT 5, 'timer', family_id, id FROM public.active_timers WHERE tombstoned_at IS NULL AND component_version < 5;

-- ---------------------------------------------------------------------------
-- Versions. A v2/v3 component is already a valid v4 component (step times and
-- bottle seconds are optional), so only the version moves. Session links stay
-- NULL.
-- ---------------------------------------------------------------------------
UPDATE public.feed_entries SET component_version = 4 WHERE component_version < 4;
UPDATE public.sleep_entries SET payload_version = 2 WHERE payload_version < 2;
UPDATE public.active_timers SET component_version = 5 WHERE component_version < 5;

-- ---------------------------------------------------------------------------
-- Past entries join sessions by the rule live tracking uses
-- (BabyStore.continuedSessionID and sleepFalls in SessionJoin.swift, with the
-- standard break limits: feed 1800 s, sleep 900 s; a gap exactly at the limit
-- joins).
--
-- Which entries are evaluated (ruling B-R9): every live feed (not solids) and
-- sleep with no session link that started before the redesign's production
-- cutover, whatever its stored version. Builds before the cutover never
-- joined anything automatically; from the cutover on, the installed build
-- decided every new part live by this same rule, so an unlinked part after it
-- is a deliberate choice (a new session or a Separate) and stays.
--
-- Links change only to move a whole session, never to split one (ruling
-- B-R13, narrowing B-R3). An evaluated entry may already be the root of a
-- session an old build's Combine made; when it joins an earlier session, every
-- live entry linked to it is relinked to the earlier root too, and each gets
-- its own journal head below. A part that is linked is never evaluated on its
-- own. A root that a running timer continues stays where it is: moving it
-- would split the timer's part from its session.
--
-- Entries are replayed per baby in (started_at, id) order, each seeing the
-- sessions formed by the entries before it, as a phone deciding at that part's
-- start would. A session's id is its root, the id of its first part
-- (effective id = link ?? id); its end is the latest (ended_at ?? started_at)
-- of its parts. Of the sessions ending at or before the part starts, the one
-- ending latest is the candidate. Tombstoned entries take no part.
--
-- The candidate sessions are built only from entries that started before the
-- part, whereas the Swift rule builds them from all live entries. The two
-- differ only when a later-starting entry is already linked into an earlier
-- session: Swift counts it in that session's end, this file does not.
-- ---------------------------------------------------------------------------
CREATE FUNCTION pg_temp.format_cleanup_join_cutoff() RETURNS timestamptz
LANGUAGE sql IMMUTABLE AS $$
    -- The redesign build (2961ca1) went live in production at this instant.
    SELECT timestamptz '2026-10-02T16:37:50Z'
$$;

CREATE TEMP TABLE format_cleanup_joined (
    kind_order integer NOT NULL,
    entity_kind text NOT NULL,
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    session_id uuid NOT NULL,
    PRIMARY KEY (family_id, entity_kind, id)
) ON COMMIT DROP;
-- Parts that moved with the root they were linked to.
CREATE TEMP TABLE format_cleanup_relinked (
    kind_order integer NOT NULL,
    entity_kind text NOT NULL,
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    session_id uuid NOT NULL,
    PRIMARY KEY (family_id, entity_kind, id)
) ON COMMIT DROP;

-- Feeds. Solids never join and are never joined to. A sleep, completed or
-- still running, that occupies any part of the gap keeps the parts apart, and
-- so does a running sleep timer that started before the part. Diapers,
-- supplements and growth are never consulted.
DO $$
DECLARE
    part record;
    previous record;
BEGIN
    FOR part IN
        SELECT e.family_id, e.baby_id, e.id, e.started_at
          FROM public.feed_entries e
         WHERE e.tombstoned_at IS NULL AND e.kind <> 'solids' AND e.feeding_session_id IS NULL
           AND e.started_at < pg_temp.format_cleanup_join_cutoff()
         ORDER BY e.family_id, e.baby_id, e.started_at, e.id
    LOOP
        CONTINUE WHEN EXISTS (SELECT FROM public.active_timers timer
                               WHERE timer.family_id = part.family_id AND timer.session_id = part.id
                                 AND timer.tombstoned_at IS NULL);
        SELECT candidate.root, candidate.ended_at INTO previous
          FROM (SELECT coalesce(earlier.feeding_session_id, earlier.id) AS root,
                       min(earlier.started_at) AS started_at,
                       max(coalesce(earlier.ended_at, earlier.started_at)) AS ended_at
                  FROM public.feed_entries earlier
                 WHERE earlier.family_id = part.family_id AND earlier.baby_id = part.baby_id
                   AND earlier.tombstoned_at IS NULL AND earlier.kind <> 'solids'
                   AND (earlier.started_at, earlier.id) < (part.started_at, part.id)
                 GROUP BY 1) candidate
         WHERE candidate.ended_at <= part.started_at AND candidate.root <> part.id
         -- Equal ends: the session that started first wins, then the smaller id, as
         -- max(by:) over projectSessions' (startedAt, id) order in SessionJoin.swift picks.
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
        UPDATE public.feed_entries SET feeding_session_id = previous.root
         WHERE family_id = part.family_id AND id = part.id;
        INSERT INTO format_cleanup_joined(kind_order, entity_kind, family_id, id, session_id)
        VALUES (2, 'feed', part.family_id, part.id, previous.root);
        -- The part's own session moves with it.
        WITH moved AS (
            UPDATE public.feed_entries SET feeding_session_id = previous.root
             WHERE family_id = part.family_id AND feeding_session_id = part.id AND tombstoned_at IS NULL
            RETURNING family_id, id)
        INSERT INTO format_cleanup_relinked(kind_order, entity_kind, family_id, id, session_id)
        SELECT 2, 'feed', moved.family_id, moved.id, previous.root FROM moved
        ON CONFLICT (family_id, entity_kind, id) DO UPDATE SET session_id = EXCLUDED.session_id;
    END LOOP;
END
$$;

-- Sleeps. Only a closed session (every part has ended) can be continued.
DO $$
DECLARE
    part record;
    previous record;
BEGIN
    FOR part IN
        SELECT e.family_id, e.baby_id, e.id, e.started_at
          FROM public.sleep_entries e
         WHERE e.tombstoned_at IS NULL AND e.sleep_session_id IS NULL
           AND e.started_at < pg_temp.format_cleanup_join_cutoff()
         ORDER BY e.family_id, e.baby_id, e.started_at, e.id
    LOOP
        CONTINUE WHEN EXISTS (SELECT FROM public.active_timers timer
                               WHERE timer.family_id = part.family_id AND timer.session_id = part.id
                                 AND timer.tombstoned_at IS NULL);
        SELECT candidate.root, candidate.ended_at INTO previous
          FROM (SELECT coalesce(earlier.sleep_session_id, earlier.id) AS root,
                       min(earlier.started_at) AS started_at,
                       max(earlier.ended_at) AS ended_at,
                       count(*) = count(earlier.ended_at) AS closed
                  FROM public.sleep_entries earlier
                 WHERE earlier.family_id = part.family_id AND earlier.baby_id = part.baby_id
                   AND earlier.tombstoned_at IS NULL
                   AND (earlier.started_at, earlier.id) < (part.started_at, part.id)
                 GROUP BY 1) candidate
         WHERE candidate.closed AND candidate.ended_at <= part.started_at AND candidate.root <> part.id
         -- Equal ends: the session that started first wins, then the smaller id, as
         -- max(by:) over projectSessions' (startedAt, id) order in SessionJoin.swift picks.
         ORDER BY candidate.ended_at DESC, candidate.started_at, candidate.root
         LIMIT 1;
        CONTINUE WHEN NOT FOUND OR part.started_at - previous.ended_at > interval '900 seconds';
        UPDATE public.sleep_entries SET sleep_session_id = previous.root
         WHERE family_id = part.family_id AND id = part.id;
        INSERT INTO format_cleanup_joined(kind_order, entity_kind, family_id, id, session_id)
        VALUES (3, 'sleep', part.family_id, part.id, previous.root);
        -- The part's own session moves with it.
        WITH moved AS (
            UPDATE public.sleep_entries SET sleep_session_id = previous.root
             WHERE family_id = part.family_id AND sleep_session_id = part.id AND tombstoned_at IS NULL
            RETURNING family_id, id)
        INSERT INTO format_cleanup_relinked(kind_order, entity_kind, family_id, id, session_id)
        SELECT 3, 'sleep', moved.family_id, moved.id, previous.root FROM moved
        ON CONFLICT (family_id, entity_kind, id) DO UPDATE SET session_id = EXCLUDED.session_id;
    END LOOP;
END
$$;

-- ---------------------------------------------------------------------------
-- Sync history in place: journal rows and receipt outcomes.
-- ---------------------------------------------------------------------------
UPDATE public.mutation_journal
   SET canonical_entity = pg_temp.format_cleanup_entity(canonical_entity)
 WHERE canonical_entity IS DISTINCT FROM pg_temp.format_cleanup_entity(canonical_entity);
UPDATE public.mutation_receipts
   SET outcome = pg_temp.format_cleanup_outcome(outcome)
 WHERE outcome IS DISTINCT FROM pg_temp.format_cleanup_outcome(outcome);

-- Timer commands that no longer exist were edits of the timer.
UPDATE public.mutation_journal SET operation = 'update'
 WHERE operation IN ('timer_switch_side', 'timer_pause', 'timer_attach_bottle', 'timer_start_bottle');
ALTER TABLE public.mutation_journal DROP CONSTRAINT mutation_journal_operation_check;
ALTER TABLE public.mutation_journal ADD CONSTRAINT mutation_journal_operation_check CHECK (
    operation IN ('create', 'update', 'delete', 'timer_start', 'timer_finish_bottle', 'timer_stop')
);

-- ---------------------------------------------------------------------------
-- Publish every converted, joined or relinked live entity as an ordinary
-- `update`, so phones holding a cursor receive it through incremental sync. Per family the
-- new heads take the sequences after next_sequence in (kind_order, id) order.
-- Each record gains a version and accepted_at; its last editor stays the
-- actor and the device of its previous head stays the device. The canonical
-- entity is built by the current-format branches of the snapshot SQL in
-- records/snapshot.go, so the head equals what a snapshot would serve.
-- ---------------------------------------------------------------------------
CREATE TEMP TABLE format_cleanup_heads (
    family_id uuid NOT NULL,
    kind_order integer NOT NULL,
    entity_kind text NOT NULL,
    id uuid NOT NULL,
    sequence bigint NOT NULL,
    device_id uuid,
    PRIMARY KEY (family_id, entity_kind, id),
    UNIQUE (family_id, sequence)
) ON COMMIT DROP;
INSERT INTO format_cleanup_heads(family_id, kind_order, entity_kind, id, sequence)
SELECT changed.family_id, changed.kind_order, changed.entity_kind, changed.id,
       owner.next_sequence + row_number() OVER (PARTITION BY changed.family_id ORDER BY changed.kind_order, changed.id)
  FROM (SELECT family_id, kind_order, entity_kind, id FROM format_cleanup_converted
        UNION
        SELECT family_id, kind_order, entity_kind, id FROM format_cleanup_joined
        UNION
        SELECT family_id, kind_order, entity_kind, id FROM format_cleanup_relinked) changed
  JOIN public.families owner ON owner.id = changed.family_id;

UPDATE format_cleanup_heads head SET device_id = journal.device_id
  FROM (SELECT 'feed' AS entity_kind, family_id, id, entity_version, last_sequence FROM public.feed_entries
        UNION ALL
        SELECT 'sleep', family_id, id, entity_version, last_sequence FROM public.sleep_entries) target
  JOIN public.mutation_journal journal ON journal.family_id = target.family_id AND journal.sequence = target.last_sequence
   AND journal.entity_id = target.id AND journal.entity_version = target.entity_version
 WHERE target.entity_kind = head.entity_kind AND target.family_id = head.family_id AND target.id = head.id;

-- Live timers below v5 were refused above, so only feeds and sleeps change.
DO $$
DECLARE
    found bigint;
BEGIN
    SELECT count(*) INTO found FROM format_cleanup_heads WHERE device_id IS NULL;
    IF found > 0 THEN
        RAISE EXCEPTION 'format_cleanup: % changed record(s) have no journal head to continue', found;
    END IF;
END
$$;

UPDATE public.feed_entries target
   SET entity_version = target.entity_version + 1, accepted_at = now(), last_sequence = head.sequence
  FROM format_cleanup_heads head
 WHERE head.entity_kind = 'feed' AND head.family_id = target.family_id AND head.id = target.id;
UPDATE public.sleep_entries target
   SET entity_version = target.entity_version + 1, accepted_at = now(), last_sequence = head.sequence
  FROM format_cleanup_heads head
 WHERE head.entity_kind = 'sleep' AND head.family_id = target.family_id AND head.id = target.id;

INSERT INTO public.mutation_journal(family_id, sequence, entity_kind, entity_id, entity_version,
    actor_account_id, device_id, operation, canonical_entity, accepted_at)
SELECT appended.family_id, appended.sequence, appended.entity_kind, appended.entity_id, appended.entity_version,
       appended.actor_account_id, appended.device_id, 'update', appended.canonical_entity, appended.accepted_at
  FROM (
    SELECT head.family_id, head.sequence, 'feed' AS entity_kind, f.id AS entity_id, f.entity_version,
           f.last_editor_account_id AS actor_account_id, head.device_id, f.accepted_at,
  jsonb_build_object('entity_kind','feed','meta',jsonb_build_object(
   'id',f.id::text,'family_id',f.family_id::text,'entity_version',f.entity_version,
   'creator_account_id',f.creator_account_id::text,'last_editor_account_id',f.last_editor_account_id::text,
   'client_mutation_time',to_char(f.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(f.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN f.tombstoned_at IS NULL THEN NULL ELSE to_char(f.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN f.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',f.baby_id::text,'kind',f.kind,
    'started_at',to_char(f.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'ended_at',CASE WHEN f.ended_at IS NULL THEN NULL ELSE to_char(f.ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
    'time_zone_id',f.time_zone_id,'note',f.note) || jsonb_build_object(
    'components',COALESCE((SELECT jsonb_agg(jsonb_strip_nulls(jsonb_build_object(
      'id',c.component_id::text,'kind',c.kind,'side',c.side,'seconds',c.seconds,
      'volume_ml',c.volume_ml,'contents',c.contents,'foods',c.foods,'amount',c.amount,
      'started_at',CASE WHEN c.started_at IS NULL THEN NULL ELSE to_char(c.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
      'ended_at',CASE WHEN c.ended_at IS NULL THEN NULL ELSE to_char(c.ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END)) ORDER BY c.position)
      FROM public.feed_components c WHERE c.family_id=f.family_id AND c.feed_id=f.id),'[]'::jsonb))
      || jsonb_build_object('feeding_session_id',f.feeding_session_id::text) END) AS canonical_entity
      FROM format_cleanup_heads head
      JOIN public.feed_entries f ON f.family_id = head.family_id AND f.id = head.id
     WHERE head.entity_kind = 'feed'
    UNION ALL
    SELECT head.family_id, head.sequence, 'sleep', s.id, s.entity_version,
           s.last_editor_account_id, head.device_id, s.accepted_at,
  jsonb_build_object('entity_kind','sleep','meta',jsonb_build_object(
   'id',s.id::text,'family_id',s.family_id::text,'entity_version',s.entity_version,
   'creator_account_id',s.creator_account_id::text,'last_editor_account_id',s.last_editor_account_id::text,
   'client_mutation_time',to_char(s.client_mutation_time AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'accepted_at',to_char(s.accepted_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'tombstoned_at',CASE WHEN s.tombstoned_at IS NULL THEN NULL ELSE to_char(s.tombstoned_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END),
   'payload',CASE WHEN s.tombstoned_at IS NOT NULL THEN NULL ELSE jsonb_build_object(
    'baby_id',s.baby_id::text,'started_at',to_char(s.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'ended_at',CASE WHEN s.ended_at IS NULL THEN NULL ELSE to_char(s.ended_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') END,
    'time_zone_id',s.time_zone_id,'note',s.note,'kind_override',s.kind_override) || jsonb_build_object('sleep_session_id',s.sleep_session_id::text) END)
      FROM format_cleanup_heads head
      JOIN public.sleep_entries s ON s.family_id = head.family_id AND s.id = head.id
     WHERE head.entity_kind = 'sleep'
  ) appended
 ORDER BY appended.family_id, appended.sequence;

UPDATE public.families owner SET next_sequence = owner.next_sequence + appended.heads
  FROM (SELECT family_id, count(*) AS heads FROM format_cleanup_heads GROUP BY family_id) appended
 WHERE owner.id = appended.family_id;

DO $$
DECLARE
    missing bigint;
BEGIN
    SELECT count(*) INTO missing FROM format_cleanup_heads head
     WHERE NOT EXISTS (SELECT FROM public.mutation_journal journal
                        WHERE journal.family_id = head.family_id AND journal.sequence = head.sequence
                          AND journal.entity_kind = head.entity_kind AND journal.entity_id = head.id);
    IF missing > 0 THEN
        RAISE EXCEPTION 'format_cleanup: % changed record(s) were not published', missing;
    END IF;
END
$$;

-- Check the deferred constraints now: every repointed last_sequence names its
-- new head, and the family-owner trigger the families update queued. ALTER
-- TABLE (closing the bracket below) refuses a table with pending trigger
-- events.
SET CONSTRAINTS ALL IMMEDIATE;

-- ---------------------------------------------------------------------------
-- Transient staging holds old shapes and page bindings that pin old sizes.
-- ---------------------------------------------------------------------------
DELETE FROM public.sync_page_sessions;
DELETE FROM public.sync_snapshot_entities;
DELETE FROM public.sync_snapshot_admissions;
DELETE FROM public.sync_snapshots;

-- ---------------------------------------------------------------------------
-- Legacy functions: the projection trigger functions, the rebuilds they called,
-- and the helpers only those used.
-- ---------------------------------------------------------------------------
DROP FUNCTION app.project_legacy_feed_components();
DROP FUNCTION app.project_legacy_feed_parent_components();
DROP FUNCTION app.project_legacy_timer_components();
DROP FUNCTION app.project_legacy_timer_parent_components();
DROP FUNCTION app.rebuild_feed_components(uuid, uuid);
DROP FUNCTION app.rebuild_timer_components(uuid, uuid);
DROP FUNCTION app.legacy_component_id(uuid, integer);
DROP FUNCTION app.sha1(bytea);

-- ---------------------------------------------------------------------------
-- Legacy storage. Every reader and writer uses the parts tables and the
-- session columns now: the scalar feed and timer columns and the side and food
-- tables go, and each version column is pinned to its kind's one format. None
-- of these statements queues a trigger event, so the bracket below still
-- closes on tables with nothing pending.
-- ---------------------------------------------------------------------------
DROP TABLE public.feed_side_intervals;
DROP TABLE public.feed_foods;
DROP TABLE public.timer_side_intervals;

-- The (volume_ml IS NULL) = (contents IS NULL) check goes with its columns.
ALTER TABLE public.feed_entries DROP COLUMN volume_ml, DROP COLUMN contents, DROP COLUMN amount;

ALTER TABLE public.active_timers DROP CONSTRAINT active_timers_state_shape;
ALTER TABLE public.active_timers DROP COLUMN pending_volume_ml, DROP COLUMN pending_contents;
ALTER TABLE public.active_timers ADD CONSTRAINT active_timers_state_shape CHECK (
    ((timer_kind='feed' AND (current_side IS NULL)=(current_side_started_at IS NULL)
      AND (current_bottle_started_at IS NULL OR current_side IS NULL))
     OR (timer_kind='sleep' AND current_side IS NULL AND current_side_started_at IS NULL
         AND current_bottle_started_at IS NULL))
    AND (completed_record_id IS NULL OR tombstoned_at IS NOT NULL)
);

-- With one version per kind a session link needs no version guard.
ALTER TABLE public.active_timers DROP CONSTRAINT active_timers_session_requires_v5;
ALTER TABLE public.sleep_entries DROP CONSTRAINT sleep_session_requires_v2;

ALTER TABLE public.feed_entries DROP CONSTRAINT feed_entries_component_version_check;
ALTER TABLE public.feed_entries ALTER COLUMN component_version SET DEFAULT 4;
ALTER TABLE public.feed_entries ADD CONSTRAINT feed_entries_component_version_check CHECK (component_version = 4);
ALTER TABLE public.active_timers DROP CONSTRAINT active_timers_component_version_check;
ALTER TABLE public.active_timers ALTER COLUMN component_version SET DEFAULT 5;
ALTER TABLE public.active_timers ADD CONSTRAINT active_timers_component_version_check CHECK (component_version = 5);
ALTER TABLE public.sleep_entries DROP CONSTRAINT sleep_entries_payload_version_check;
ALTER TABLE public.sleep_entries ALTER COLUMN payload_version SET DEFAULT 2;
ALTER TABLE public.sleep_entries ADD CONSTRAINT sleep_entries_payload_version_check CHECK (payload_version = 2);

DROP FUNCTION pg_temp.format_cleanup_join_cutoff();
DROP FUNCTION pg_temp.format_cleanup_outcome(jsonb);
DROP FUNCTION pg_temp.format_cleanup_entity(jsonb);
DROP FUNCTION pg_temp.format_cleanup_receipt_entities(jsonb);
DROP FUNCTION pg_temp.format_cleanup_is_legacy(jsonb);

-- ---------------------------------------------------------------------------
-- RLS bracket: close.
-- ---------------------------------------------------------------------------
ALTER TABLE public.families FORCE ROW LEVEL SECURITY;
ALTER TABLE public.feed_entries FORCE ROW LEVEL SECURITY;
ALTER TABLE public.feed_components FORCE ROW LEVEL SECURITY;
ALTER TABLE public.sleep_entries FORCE ROW LEVEL SECURITY;
ALTER TABLE public.active_timers FORCE ROW LEVEL SECURITY;
ALTER TABLE public.timer_components FORCE ROW LEVEL SECURITY;
ALTER TABLE public.mutation_journal FORCE ROW LEVEL SECURITY;
ALTER TABLE public.mutation_receipts FORCE ROW LEVEL SECURITY;
ALTER TABLE public.sync_page_sessions FORCE ROW LEVEL SECURITY;
ALTER TABLE public.sync_snapshots FORCE ROW LEVEL SECURITY;
ALTER TABLE public.sync_snapshot_entities FORCE ROW LEVEL SECURITY;
ALTER TABLE public.sync_snapshot_admissions FORCE ROW LEVEL SECURITY;
ALTER TABLE public.import_sessions FORCE ROW LEVEL SECURITY;

-- ---------------------------------------------------------------------------
-- Manifests: the schema-26 grants, less those on the dropped tables.
-- ---------------------------------------------------------------------------
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 27,principal,object_kind,object_identity,privileges
 FROM public.schema_privilege_manifest WHERE schema_version=26
  AND NOT (object_kind='table' AND object_identity IN
       ('public.feed_side_intervals','public.feed_foods','public.timer_side_intervals'));
UPDATE public.schema_compatibility SET current_version=27,
 introduced_by='000027_format_cleanup.sql',recorded_at=now() WHERE singleton;
