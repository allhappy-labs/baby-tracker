-- Typed shared timers and their ordered breast-side intervals. Application
-- code using these tables lands in the following feature commit; this
-- migration is the deployable overlap predecessor for schema 10 -> 11.

CREATE TABLE public.active_timers (
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
    tombstoned_at timestamptz CHECK (tombstoned_at IS NULL OR
        (isfinite(tombstoned_at) AND tombstoned_at >= accepted_at)),
    last_sequence bigint NOT NULL DEFAULT 0 CHECK (last_sequence BETWEEN 0 AND 9007199254740991),
    storage_bytes bigint NOT NULL CHECK (storage_bytes BETWEEN 1 AND 9007199254740991),
    timer_kind text NOT NULL CHECK (timer_kind IN ('feed', 'sleep')),
    started_at timestamptz NOT NULL CHECK (isfinite(started_at)
        AND started_at >= '1900-01-01T00:00:00Z'::timestamptz
        AND started_at < '2101-01-01T00:00:00Z'::timestamptz),
    time_zone_id text NOT NULL CHECK (octet_length(time_zone_id) BETWEEN 1 AND 255),
    current_side text CHECK (current_side IN ('left', 'right')),
    current_side_started_at timestamptz CHECK (current_side_started_at IS NULL OR
        (isfinite(current_side_started_at)
         AND current_side_started_at >= started_at
         AND current_side_started_at < '2101-01-01T00:00:00Z'::timestamptz)),
    pending_volume_ml numeric(10,6) CHECK (pending_volume_ml BETWEEN 0 AND 2000),
    pending_contents text CHECK (pending_contents IN ('breast_milk', 'formula', 'mixed')),
    completed_record_id uuid CHECK (completed_record_id IS NULL OR
        completed_record_id <> '00000000-0000-0000-0000-000000000000'),
    PRIMARY KEY (family_id, id),
    CONSTRAINT active_timers_baby_fk FOREIGN KEY (family_id, baby_id)
        REFERENCES public.babies(family_id, id),
    CONSTRAINT active_timers_state_shape CHECK (
        (pending_volume_ml IS NULL) = (pending_contents IS NULL)
        AND ((timer_kind = 'feed'
              AND current_side IS NOT NULL
              AND current_side_started_at IS NOT NULL)
             OR (timer_kind = 'sleep'
                 AND current_side IS NULL
                 AND current_side_started_at IS NULL
                 AND pending_volume_ml IS NULL
                 AND pending_contents IS NULL))
        AND (completed_record_id IS NULL OR tombstoned_at IS NOT NULL))
);

CREATE UNIQUE INDEX one_live_timer_per_kind
    ON public.active_timers(family_id, baby_id, timer_kind)
    WHERE tombstoned_at IS NULL;
CREATE INDEX active_timers_live_baby_idx
    ON public.active_timers(family_id, baby_id, id)
    WHERE tombstoned_at IS NULL;

CREATE TABLE public.timer_side_intervals (
    family_id uuid NOT NULL,
    timer_id uuid NOT NULL CHECK (timer_id <> '00000000-0000-0000-0000-000000000000'),
    position integer NOT NULL CHECK (position BETWEEN 0 AND 255),
    side text NOT NULL CHECK (side IN ('left', 'right')),
    seconds integer NOT NULL CHECK (seconds BETWEEN 0 AND 604800),
    PRIMARY KEY (family_id, timer_id, position),
    CONSTRAINT timer_side_intervals_timer_fk FOREIGN KEY (family_id, timer_id)
        REFERENCES public.active_timers(family_id, id) ON DELETE CASCADE
);

ALTER TABLE public.active_timers ADD CONSTRAINT active_timers_last_sequence_fk
    FOREIGN KEY (family_id, id, entity_version, last_sequence)
    REFERENCES public.mutation_journal(family_id, entity_id, entity_version, sequence)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE public.active_timers ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.active_timers FORCE ROW LEVEL SECURITY;
ALTER TABLE public.timer_side_intervals ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.timer_side_intervals FORCE ROW LEVEL SECURITY;

CREATE POLICY family_context ON public.active_timers
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_context ON public.timer_side_intervals
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());

REVOKE ALL ON public.active_timers, public.timer_side_intervals
    FROM PUBLIC, baby_tracker_runtime, baby_tracker_projector,
    baby_tracker_apple_revoker;
GRANT SELECT, INSERT, UPDATE ON public.active_timers TO baby_tracker_runtime;
GRANT SELECT, INSERT, DELETE ON public.timer_side_intervals TO baby_tracker_runtime;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
SELECT 11,principal,object_kind,object_identity,privileges
  FROM public.schema_privilege_manifest WHERE schema_version=10;
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
    (11,'baby_tracker_runtime','table','public.active_timers','SELECT,INSERT,UPDATE'),
    (11,'baby_tracker_runtime','table','public.timer_side_intervals','SELECT,INSERT,DELETE');

UPDATE public.schema_compatibility
   SET current_version=11,introduced_by='000011_timer_commands.sql',recorded_at=now()
 WHERE singleton;
