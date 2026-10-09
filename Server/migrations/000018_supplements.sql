CREATE TABLE public.supplement_entries (
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
    time_zone_id text NOT NULL CHECK (octet_length(time_zone_id) BETWEEN 1 AND 255),
    note text CHECK (octet_length(note) <= 4096),
    logged_at timestamptz NOT NULL CHECK (isfinite(logged_at)
        AND logged_at >= '1900-01-01T00:00:00Z'::timestamptz
        AND logged_at < '2101-01-01T00:00:00Z'::timestamptz),
    name text NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 64 AND octet_length(name) <= 256),
    PRIMARY KEY (family_id, id),
    CONSTRAINT supplement_entries_baby_fk FOREIGN KEY (family_id, baby_id)
        REFERENCES public.babies(family_id, id)
);
CREATE INDEX supplement_entries_family_baby_idx
    ON public.supplement_entries(family_id, baby_id, started_at DESC, id);
ALTER TABLE public.supplement_entries ADD CONSTRAINT supplement_entries_last_sequence_fk
 FOREIGN KEY (family_id,id,entity_version,last_sequence)
 REFERENCES public.mutation_journal(family_id,entity_id,entity_version,sequence) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE public.mutation_journal DROP CONSTRAINT mutation_journal_entity_kind_check;
ALTER TABLE public.mutation_journal ADD CONSTRAINT mutation_journal_entity_kind_check CHECK(entity_kind IN ('baby','feed','sleep','diaper','timer','supplement'));
ALTER TABLE public.supplement_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.supplement_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY family_context ON public.supplement_entries USING(family_id=app.current_family_id()) WITH CHECK(family_id=app.current_family_id());
REVOKE ALL ON public.supplement_entries FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;
GRANT SELECT,INSERT,UPDATE ON public.supplement_entries TO baby_tracker_runtime;
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 18,principal,object_kind,object_identity,privileges FROM public.schema_privilege_manifest WHERE schema_version=17;
INSERT INTO public.schema_privilege_manifest VALUES (18,'baby_tracker_runtime','table','public.supplement_entries','SELECT,INSERT,UPDATE');
UPDATE public.schema_compatibility SET current_version=18,introduced_by='000018_supplements.sql',recorded_at=now() WHERE singleton;

ALTER TABLE public.sync_snapshot_entities DROP CONSTRAINT sync_snapshot_entities_entity_kind_check;
ALTER TABLE public.sync_snapshot_entities ADD CONSTRAINT sync_snapshot_entities_entity_kind_check CHECK(entity_kind IN ('baby','feed','sleep','diaper','timer','supplement'));
ALTER TABLE public.import_record_manifests DROP CONSTRAINT import_record_manifests_entity_kind_check;
ALTER TABLE public.import_record_manifests ADD CONSTRAINT import_record_manifests_entity_kind_check CHECK(entity_kind IN ('feed','sleep','diaper','timer','supplement'));
