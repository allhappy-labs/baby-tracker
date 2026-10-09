-- Typed canonical family records, durable mutation receipts, and the
-- commit-ordered family journal. Application code using these tables lands in
-- the following feature commit; this migration is the deployable overlap
-- predecessor for schema 8 -> 9.

ALTER TABLE public.families
    ADD COLUMN next_sequence bigint NOT NULL DEFAULT 0,
    ADD CONSTRAINT families_next_sequence_safe
        CHECK (next_sequence BETWEEN 0 AND 9007199254740991);

CREATE TABLE public.babies (
    family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    id uuid NOT NULL CHECK (id <> '00000000-0000-0000-0000-000000000000'),
    entity_version bigint NOT NULL CHECK (entity_version BETWEEN 1 AND 9007199254740991),
    creator_account_id uuid NOT NULL CHECK (creator_account_id <> '00000000-0000-0000-0000-000000000000'),
    last_editor_account_id uuid NOT NULL CHECK (last_editor_account_id <> '00000000-0000-0000-0000-000000000000'),
    client_mutation_time timestamptz NOT NULL CHECK (isfinite(client_mutation_time)
        AND client_mutation_time >= '1900-01-01T00:00:00Z'::timestamptz
        AND client_mutation_time < '2101-01-01T00:00:00Z'::timestamptz),
    accepted_at timestamptz NOT NULL CHECK (isfinite(accepted_at)),
    tombstoned_at timestamptz CHECK (tombstoned_at IS NULL OR (isfinite(tombstoned_at) AND tombstoned_at >= accepted_at)),
    last_sequence bigint NOT NULL DEFAULT 0 CHECK (last_sequence BETWEEN 0 AND 9007199254740991),
	storage_bytes bigint NOT NULL CHECK (storage_bytes BETWEEN 1 AND 9007199254740991),
    name text NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 400),
	normalized_name text NOT NULL CONSTRAINT babies_normalized_name_bytes
		CHECK (octet_length(normalized_name) BETWEEN 1 AND 1600),
    normalization_version text NOT NULL CHECK (normalization_version = 'nfc-trim-fold-nfc-v1'),
    birth_date date NOT NULL CHECK (birth_date >= DATE '1900-01-01' AND birth_date < DATE '2101-01-01'),
    sort_order bigint NOT NULL CHECK (sort_order BETWEEN 0 AND 1000000),
    PRIMARY KEY (family_id, id)
);
CREATE INDEX babies_active_match_idx
    ON public.babies(family_id, normalization_version, normalized_name, birth_date, id)
    WHERE tombstoned_at IS NULL;

CREATE TABLE public.feed_entries (
    family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    id uuid NOT NULL CHECK (id <> '00000000-0000-0000-0000-000000000000'),
    baby_id uuid NOT NULL CHECK (baby_id <> '00000000-0000-0000-0000-000000000000'),
    entity_version bigint NOT NULL CHECK (entity_version BETWEEN 1 AND 9007199254740991),
    creator_account_id uuid NOT NULL CHECK (creator_account_id <> '00000000-0000-0000-0000-000000000000'),
    last_editor_account_id uuid NOT NULL CHECK (last_editor_account_id <> '00000000-0000-0000-0000-000000000000'),
    client_mutation_time timestamptz NOT NULL CHECK (isfinite(client_mutation_time)
        AND client_mutation_time >= '1900-01-01T00:00:00Z'::timestamptz
        AND client_mutation_time < '2101-01-01T00:00:00Z'::timestamptz),
    accepted_at timestamptz NOT NULL CHECK (isfinite(accepted_at)),
    tombstoned_at timestamptz CHECK (tombstoned_at IS NULL OR (isfinite(tombstoned_at) AND tombstoned_at >= accepted_at)),
    last_sequence bigint NOT NULL DEFAULT 0 CHECK (last_sequence BETWEEN 0 AND 9007199254740991),
	storage_bytes bigint NOT NULL CHECK (storage_bytes BETWEEN 1 AND 9007199254740991),
    kind text NOT NULL CHECK (kind IN ('breast', 'bottle', 'solids')),
    started_at timestamptz NOT NULL CHECK (isfinite(started_at)
        AND started_at >= '1900-01-01T00:00:00Z'::timestamptz
        AND started_at < '2101-01-01T00:00:00Z'::timestamptz),
    ended_at timestamptz CHECK (ended_at IS NULL OR (isfinite(ended_at) AND ended_at >= started_at
        AND ended_at <= started_at + interval '7 days')),
    time_zone_id text NOT NULL CHECK (octet_length(time_zone_id) BETWEEN 1 AND 255),
    note text CHECK (octet_length(note) <= 4096),
    volume_ml numeric(10,6) CHECK (volume_ml BETWEEN 0 AND 2000),
    contents text CHECK (contents IN ('breast_milk', 'formula', 'mixed')),
    amount text CHECK (amount IN ('taste', 'some', 'lots')),
	CHECK ((volume_ml IS NULL) = (contents IS NULL)),
    PRIMARY KEY (family_id, id),
    CONSTRAINT feed_entries_baby_fk FOREIGN KEY (family_id, baby_id)
        REFERENCES public.babies(family_id, id)
);
CREATE INDEX feed_entries_family_baby_idx
    ON public.feed_entries(family_id, baby_id, started_at DESC, id);

CREATE TABLE public.sleep_entries (
    family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    id uuid NOT NULL CHECK (id <> '00000000-0000-0000-0000-000000000000'),
    baby_id uuid NOT NULL CHECK (baby_id <> '00000000-0000-0000-0000-000000000000'),
    entity_version bigint NOT NULL CHECK (entity_version BETWEEN 1 AND 9007199254740991),
    creator_account_id uuid NOT NULL CHECK (creator_account_id <> '00000000-0000-0000-0000-000000000000'),
    last_editor_account_id uuid NOT NULL CHECK (last_editor_account_id <> '00000000-0000-0000-0000-000000000000'),
    client_mutation_time timestamptz NOT NULL CHECK (isfinite(client_mutation_time)
        AND client_mutation_time >= '1900-01-01T00:00:00Z'::timestamptz
        AND client_mutation_time < '2101-01-01T00:00:00Z'::timestamptz),
    accepted_at timestamptz NOT NULL CHECK (isfinite(accepted_at)),
    tombstoned_at timestamptz CHECK (tombstoned_at IS NULL OR (isfinite(tombstoned_at) AND tombstoned_at >= accepted_at)),
    last_sequence bigint NOT NULL DEFAULT 0 CHECK (last_sequence BETWEEN 0 AND 9007199254740991),
	storage_bytes bigint NOT NULL CHECK (storage_bytes BETWEEN 1 AND 9007199254740991),
    started_at timestamptz NOT NULL CHECK (isfinite(started_at)
        AND started_at >= '1900-01-01T00:00:00Z'::timestamptz
        AND started_at < '2101-01-01T00:00:00Z'::timestamptz),
    ended_at timestamptz CHECK (ended_at IS NULL OR (isfinite(ended_at) AND ended_at >= started_at
        AND ended_at <= started_at + interval '7 days')),
    time_zone_id text NOT NULL CHECK (octet_length(time_zone_id) BETWEEN 1 AND 255),
    note text CHECK (octet_length(note) <= 4096),
    kind_override text CHECK (kind_override IN ('nap', 'night')),
    PRIMARY KEY (family_id, id),
    CONSTRAINT sleep_entries_baby_fk FOREIGN KEY (family_id, baby_id)
        REFERENCES public.babies(family_id, id)
);
CREATE INDEX sleep_entries_family_baby_idx
    ON public.sleep_entries(family_id, baby_id, started_at DESC, id);

CREATE TABLE public.diaper_entries (
    family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    id uuid NOT NULL CHECK (id <> '00000000-0000-0000-0000-000000000000'),
    baby_id uuid NOT NULL CHECK (baby_id <> '00000000-0000-0000-0000-000000000000'),
    entity_version bigint NOT NULL CHECK (entity_version BETWEEN 1 AND 9007199254740991),
    creator_account_id uuid NOT NULL CHECK (creator_account_id <> '00000000-0000-0000-0000-000000000000'),
    last_editor_account_id uuid NOT NULL CHECK (last_editor_account_id <> '00000000-0000-0000-0000-000000000000'),
    client_mutation_time timestamptz NOT NULL CHECK (isfinite(client_mutation_time)
        AND client_mutation_time >= '1900-01-01T00:00:00Z'::timestamptz
        AND client_mutation_time < '2101-01-01T00:00:00Z'::timestamptz),
    accepted_at timestamptz NOT NULL CHECK (isfinite(accepted_at)),
    tombstoned_at timestamptz CHECK (tombstoned_at IS NULL OR (isfinite(tombstoned_at) AND tombstoned_at >= accepted_at)),
    last_sequence bigint NOT NULL DEFAULT 0 CHECK (last_sequence BETWEEN 0 AND 9007199254740991),
	storage_bytes bigint NOT NULL CHECK (storage_bytes BETWEEN 1 AND 9007199254740991),
    started_at timestamptz NOT NULL CHECK (isfinite(started_at)
        AND started_at >= '1900-01-01T00:00:00Z'::timestamptz
        AND started_at < '2101-01-01T00:00:00Z'::timestamptz),
    ended_at timestamptz CHECK (ended_at IS NULL OR (isfinite(ended_at) AND ended_at >= started_at
        AND ended_at <= started_at + interval '7 days')),
    time_zone_id text NOT NULL CHECK (octet_length(time_zone_id) BETWEEN 1 AND 255),
    note text CHECK (octet_length(note) <= 4096),
    contents text NOT NULL CHECK (contents IN ('wet', 'dirty', 'mixed', 'dry')),
    consistency text CHECK (char_length(consistency) <= 64 AND octet_length(consistency) <= 256),
    color text CHECK (char_length(color) <= 64 AND octet_length(color) <= 256),
    PRIMARY KEY (family_id, id),
    CONSTRAINT diaper_entries_baby_fk FOREIGN KEY (family_id, baby_id)
        REFERENCES public.babies(family_id, id)
);
CREATE INDEX diaper_entries_family_baby_idx
    ON public.diaper_entries(family_id, baby_id, started_at DESC, id);

CREATE TABLE public.feed_side_intervals (
    family_id uuid NOT NULL,
    feed_id uuid NOT NULL CHECK (feed_id <> '00000000-0000-0000-0000-000000000000'),
    position integer NOT NULL CHECK (position BETWEEN 0 AND 255),
    side text NOT NULL CHECK (side IN ('left', 'right')),
    seconds integer NOT NULL CHECK (seconds BETWEEN 0 AND 604800),
    PRIMARY KEY (family_id, feed_id, position),
    CONSTRAINT feed_side_intervals_feed_fk FOREIGN KEY (family_id, feed_id)
        REFERENCES public.feed_entries(family_id, id) ON DELETE CASCADE
);

CREATE TABLE public.feed_foods (
    family_id uuid NOT NULL,
    feed_id uuid NOT NULL CHECK (feed_id <> '00000000-0000-0000-0000-000000000000'),
    position integer NOT NULL CHECK (position BETWEEN 0 AND 49),
    food text NOT NULL CHECK (octet_length(food) BETWEEN 1 AND 400),
    PRIMARY KEY (family_id, feed_id, position),
    CONSTRAINT feed_foods_feed_fk FOREIGN KEY (family_id, feed_id)
        REFERENCES public.feed_entries(family_id, id) ON DELETE CASCADE
);

CREATE TABLE public.mutation_receipts (
    family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    actor_account_id uuid NOT NULL CHECK (actor_account_id <> '00000000-0000-0000-0000-000000000000'),
    device_id uuid NOT NULL CHECK (device_id <> '00000000-0000-0000-0000-000000000000'),
    mutation_id uuid NOT NULL CHECK (mutation_id <> '00000000-0000-0000-0000-000000000000'),
    semantic_hash bytea NOT NULL CHECK (octet_length(semantic_hash) = 32),
    outcome jsonb NOT NULL CHECK (jsonb_typeof(outcome) = 'object'),
    accepted_at timestamptz NOT NULL CHECK (isfinite(accepted_at)),
    last_sequence bigint CHECK (last_sequence BETWEEN 1 AND 9007199254740991),
    PRIMARY KEY (device_id, mutation_id)
);
CREATE INDEX mutation_receipts_family_cursor_idx
    ON public.mutation_receipts(family_id, last_sequence, device_id, mutation_id);

CREATE TABLE public.family_record_usage (
	family_id uuid PRIMARY KEY REFERENCES public.families(id) ON DELETE CASCADE,
	canonical_record_count bigint NOT NULL DEFAULT 0
		CHECK (canonical_record_count BETWEEN 0 AND 9007199254740991),
	charged_storage_bytes bigint NOT NULL DEFAULT 0
		CHECK (charged_storage_bytes BETWEEN 0 AND 9007199254740991),
	updated_at timestamptz NOT NULL CHECK (isfinite(updated_at))
);

CREATE TABLE public.mutation_journal (
    family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    sequence bigint NOT NULL CHECK (sequence BETWEEN 1 AND 9007199254740991),
    entity_kind text NOT NULL CHECK (entity_kind IN ('baby', 'feed', 'sleep', 'diaper', 'timer')),
    entity_id uuid NOT NULL CHECK (entity_id <> '00000000-0000-0000-0000-000000000000'),
    entity_version bigint NOT NULL CHECK (entity_version BETWEEN 1 AND 9007199254740991),
    actor_account_id uuid NOT NULL CHECK (actor_account_id <> '00000000-0000-0000-0000-000000000000'),
    device_id uuid NOT NULL CHECK (device_id <> '00000000-0000-0000-0000-000000000000'),
    operation text NOT NULL CHECK (operation IN ('create', 'update', 'delete',
        'timer_start', 'timer_switch_side', 'timer_attach_bottle', 'timer_stop')),
    canonical_entity jsonb NOT NULL CHECK (jsonb_typeof(canonical_entity) = 'object'),
    accepted_at timestamptz NOT NULL CHECK (isfinite(accepted_at)),
    PRIMARY KEY (family_id, sequence),
	UNIQUE (family_id, entity_kind, entity_id, entity_version),
	CONSTRAINT mutation_journal_entity_revision_cursor_key
		UNIQUE (family_id, entity_id, entity_version, sequence)
);
CREATE INDEX mutation_journal_entity_idx
    ON public.mutation_journal(family_id, entity_kind, entity_id, sequence DESC);

ALTER TABLE public.babies ADD CONSTRAINT babies_last_sequence_fk
	FOREIGN KEY (family_id, id, entity_version, last_sequence)
	REFERENCES public.mutation_journal(family_id, entity_id, entity_version, sequence)
    DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE public.feed_entries ADD CONSTRAINT feed_entries_last_sequence_fk
	FOREIGN KEY (family_id, id, entity_version, last_sequence)
	REFERENCES public.mutation_journal(family_id, entity_id, entity_version, sequence)
    DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE public.sleep_entries ADD CONSTRAINT sleep_entries_last_sequence_fk
	FOREIGN KEY (family_id, id, entity_version, last_sequence)
	REFERENCES public.mutation_journal(family_id, entity_id, entity_version, sequence)
    DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE public.diaper_entries ADD CONSTRAINT diaper_entries_last_sequence_fk
	FOREIGN KEY (family_id, id, entity_version, last_sequence)
	REFERENCES public.mutation_journal(family_id, entity_id, entity_version, sequence)
    DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE public.mutation_receipts ADD CONSTRAINT mutation_receipts_last_sequence_fk
    FOREIGN KEY (family_id, last_sequence) REFERENCES public.mutation_journal(family_id, sequence)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE public.babies ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.babies FORCE ROW LEVEL SECURITY;
ALTER TABLE public.feed_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.feed_entries FORCE ROW LEVEL SECURITY;
ALTER TABLE public.sleep_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.sleep_entries FORCE ROW LEVEL SECURITY;
ALTER TABLE public.diaper_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.diaper_entries FORCE ROW LEVEL SECURITY;
ALTER TABLE public.feed_side_intervals ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.feed_side_intervals FORCE ROW LEVEL SECURITY;
ALTER TABLE public.feed_foods ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.feed_foods FORCE ROW LEVEL SECURITY;
ALTER TABLE public.mutation_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.mutation_receipts FORCE ROW LEVEL SECURITY;
ALTER TABLE public.family_record_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.family_record_usage FORCE ROW LEVEL SECURITY;
ALTER TABLE public.mutation_journal ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.mutation_journal FORCE ROW LEVEL SECURITY;

CREATE POLICY family_context ON public.babies
    USING (family_id = app.current_family_id()) WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_context ON public.feed_entries
    USING (family_id = app.current_family_id()) WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_context ON public.sleep_entries
    USING (family_id = app.current_family_id()) WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_context ON public.diaper_entries
    USING (family_id = app.current_family_id()) WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_context ON public.feed_side_intervals
    USING (family_id = app.current_family_id()) WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_context ON public.feed_foods
    USING (family_id = app.current_family_id()) WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_context ON public.mutation_receipts
    USING (family_id = app.current_family_id()) WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_context ON public.family_record_usage
	USING (family_id = app.current_family_id()) WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_context ON public.mutation_journal
    USING (family_id = app.current_family_id()) WITH CHECK (family_id = app.current_family_id());

-- A primary-key conflict can be hidden by RLS when a device reuses a mutation
-- ID after changing families. Attempting the insert under the caller's family
-- context lets PostgreSQL's global unique key detect that case without reading
-- or revealing the inaccessible receipt.
CREATE FUNCTION app.save_mutation_receipt(
	p_account_id uuid, p_family_id uuid, p_device_id uuid, p_mutation_id uuid,
	p_semantic_hash bytea, p_outcome jsonb, p_accepted_at timestamptz,
	p_last_sequence bigint
) RETURNS text
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = pg_catalog
AS $$
BEGIN
    IF p_account_id IS NULL OR p_account_id IS DISTINCT FROM app.current_account_id()
	   OR p_family_id IS NULL OR p_family_id IS DISTINCT FROM app.current_family_id()
       OR p_device_id IS NULL OR p_device_id = '00000000-0000-0000-0000-000000000000'
       OR p_mutation_id IS NULL OR p_mutation_id = '00000000-0000-0000-0000-000000000000'
	   OR p_semantic_hash IS NULL OR octet_length(p_semantic_hash) <> 32
	   OR p_outcome IS NULL OR jsonb_typeof(p_outcome) <> 'object'
	   OR p_accepted_at IS NULL OR NOT isfinite(p_accepted_at)
	   OR (p_last_sequence IS NOT NULL AND (p_last_sequence < 1 OR p_last_sequence > 9007199254740991))
       OR NOT EXISTS (
           SELECT FROM public.devices d
            WHERE d.id = p_device_id AND d.account_id = p_account_id
              AND d.binding_revoked_at IS NULL
	   ) OR NOT EXISTS (
	       SELECT FROM public.families f
	       JOIN public.memberships m ON m.family_id = f.id
	        WHERE f.id = p_family_id AND f.status = 'active'
	          AND m.account_id = p_account_id AND m.status = 'active'
       ) THEN
		RAISE EXCEPTION 'mutation receipt save denied' USING ERRCODE = '42501';
    END IF;
	BEGIN
		INSERT INTO public.mutation_receipts(family_id,actor_account_id,device_id,mutation_id,
			semantic_hash,outcome,accepted_at,last_sequence)
		VALUES(p_family_id,p_account_id,p_device_id,p_mutation_id,p_semantic_hash,p_outcome,
			p_accepted_at,p_last_sequence);
		RETURN 'inserted';
	EXCEPTION WHEN unique_violation THEN
		RETURN 'collision';
	END;
END
$$;
REVOKE ALL ON FUNCTION app.save_mutation_receipt(uuid,uuid,uuid,uuid,bytea,jsonb,timestamptz,bigint)
    FROM PUBLIC, baby_tracker_projector, baby_tracker_apple_revoker;

GRANT SELECT, INSERT, UPDATE ON public.babies, public.feed_entries,
    public.sleep_entries, public.diaper_entries TO baby_tracker_runtime;
GRANT SELECT, INSERT, DELETE ON public.feed_side_intervals,
    public.feed_foods TO baby_tracker_runtime;
GRANT SELECT ON public.mutation_receipts TO baby_tracker_runtime;
GRANT SELECT, INSERT ON public.mutation_journal TO baby_tracker_runtime;
GRANT SELECT, INSERT, UPDATE ON public.family_record_usage TO baby_tracker_runtime;
GRANT UPDATE (next_sequence) ON public.families TO baby_tracker_runtime;
GRANT EXECUTE ON FUNCTION app.save_mutation_receipt(uuid,uuid,uuid,uuid,bytea,jsonb,timestamptz,bigint)
    TO baby_tracker_runtime;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
SELECT 9,principal,object_kind,object_identity,privileges
  FROM public.schema_privilege_manifest WHERE schema_version=8;
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
    (9,'baby_tracker_runtime','column','public.families.next_sequence','UPDATE'),
    (9,'baby_tracker_runtime','table','public.babies','SELECT,INSERT,UPDATE'),
    (9,'baby_tracker_runtime','table','public.feed_entries','SELECT,INSERT,UPDATE'),
    (9,'baby_tracker_runtime','table','public.sleep_entries','SELECT,INSERT,UPDATE'),
    (9,'baby_tracker_runtime','table','public.diaper_entries','SELECT,INSERT,UPDATE'),
    (9,'baby_tracker_runtime','table','public.feed_side_intervals','SELECT,INSERT,DELETE'),
	(9,'baby_tracker_runtime','table','public.feed_foods','SELECT,INSERT,DELETE'),
	(9,'baby_tracker_runtime','table','public.mutation_receipts','SELECT'),
    (9,'baby_tracker_runtime','table','public.mutation_journal','SELECT,INSERT'),
	(9,'baby_tracker_runtime','table','public.family_record_usage','SELECT,INSERT,UPDATE'),
	(9,'baby_tracker_runtime','function','app.save_mutation_receipt(uuid,uuid,uuid,uuid,bytea,jsonb,timestamp with time zone,bigint)','EXECUTE');

UPDATE public.schema_compatibility
   SET current_version=9,introduced_by='000009_canonical_records.sql',recorded_at=now()
 WHERE singleton;
