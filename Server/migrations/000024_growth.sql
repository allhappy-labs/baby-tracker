CREATE TABLE public.growth_profiles (
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
    reference_sex text NOT NULL CHECK(reference_sex IN ('girls','boys')),
    expected_due_date date CHECK(expected_due_date IS NULL OR expected_due_date BETWEEN '1900-01-01'::date AND '2100-12-31'::date),
    UNIQUE(family_id,baby_id),
    PRIMARY KEY(family_id,id),
    FOREIGN KEY(family_id,baby_id) REFERENCES public.babies(family_id,id)
);
ALTER TABLE public.growth_profiles ADD CONSTRAINT growth_profiles_last_sequence_fk FOREIGN KEY(family_id,id,entity_version,last_sequence)
 REFERENCES public.mutation_journal(family_id,entity_id,entity_version,sequence) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE public.growth_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.growth_profiles FORCE ROW LEVEL SECURITY;
CREATE POLICY family_context ON public.growth_profiles USING(family_id=app.current_family_id()) WITH CHECK(family_id=app.current_family_id());
REVOKE ALL ON public.growth_profiles FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;
GRANT SELECT,INSERT,UPDATE ON public.growth_profiles TO baby_tracker_runtime;

CREATE TABLE public.measurement_entries (
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
    measured_at timestamptz NOT NULL CHECK(isfinite(measured_at) AND measured_at >= '1900-01-01'::timestamptz AND measured_at < '2101-01-01'::timestamptz),
    logged_at timestamptz NOT NULL CHECK(isfinite(logged_at) AND logged_at >= '1900-01-01'::timestamptz AND logged_at < '2101-01-01'::timestamptz),
    time_zone_id text NOT NULL CHECK(octet_length(time_zone_id) BETWEEN 1 AND 255),
    weight_kg numeric CHECK(weight_kg IS NULL OR (weight_kg > 0 AND weight_kg NOT IN ('NaN'::numeric,'Infinity'::numeric,'-Infinity'::numeric))),
    length_cm numeric CHECK(length_cm IS NULL OR (length_cm > 0 AND length_cm NOT IN ('NaN'::numeric,'Infinity'::numeric,'-Infinity'::numeric))),
    head_circumference_cm numeric CHECK(head_circumference_cm IS NULL OR (head_circumference_cm > 0 AND head_circumference_cm NOT IN ('NaN'::numeric,'Infinity'::numeric,'-Infinity'::numeric))),
    length_position text CHECK(length_position IN ('lying','standing')),
    CHECK(weight_kg IS NOT NULL OR length_cm IS NOT NULL OR head_circumference_cm IS NOT NULL),
    CHECK((length_cm IS NULL) = (length_position IS NULL)),
    PRIMARY KEY(family_id,id),
    FOREIGN KEY(family_id,baby_id) REFERENCES public.babies(family_id,id)
);
CREATE INDEX measurement_entries_family_baby_idx ON public.measurement_entries(family_id,baby_id,measured_at DESC,id);
ALTER TABLE public.measurement_entries ADD CONSTRAINT measurement_entries_last_sequence_fk FOREIGN KEY(family_id,id,entity_version,last_sequence)
 REFERENCES public.mutation_journal(family_id,entity_id,entity_version,sequence) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE public.measurement_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.measurement_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY family_context ON public.measurement_entries USING(family_id=app.current_family_id()) WITH CHECK(family_id=app.current_family_id());
REVOKE ALL ON public.measurement_entries FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;
GRANT SELECT,INSERT,UPDATE ON public.measurement_entries TO baby_tracker_runtime;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 24,principal,object_kind,object_identity,privileges FROM public.schema_privilege_manifest WHERE schema_version=23;
INSERT INTO public.schema_privilege_manifest VALUES
 (24,'baby_tracker_runtime','table','public.growth_profiles','SELECT,INSERT,UPDATE'),
 (24,'baby_tracker_runtime','table','public.measurement_entries','SELECT,INSERT,UPDATE');
UPDATE public.schema_compatibility SET current_version=24,introduced_by='000024_growth.sql',recorded_at=now() WHERE singleton;

ALTER TABLE public.mutation_journal DROP CONSTRAINT mutation_journal_entity_kind_check;
ALTER TABLE public.mutation_journal ADD CONSTRAINT mutation_journal_entity_kind_check CHECK(entity_kind IN ('baby','feed','sleep','diaper','timer','supplement','growth_profile','measurement'));

ALTER TABLE public.sync_snapshot_entities DROP CONSTRAINT sync_snapshot_entities_entity_kind_check;
ALTER TABLE public.sync_snapshot_entities ADD CONSTRAINT sync_snapshot_entities_entity_kind_check CHECK(entity_kind IN ('baby','feed','sleep','diaper','timer','supplement','growth_profile','measurement'));

ALTER TABLE public.import_record_manifests DROP CONSTRAINT import_record_manifests_entity_kind_check;
ALTER TABLE public.import_record_manifests ADD CONSTRAINT import_record_manifests_entity_kind_check CHECK(entity_kind IN ('feed','sleep','diaper','timer','supplement','growth_profile','measurement'));
