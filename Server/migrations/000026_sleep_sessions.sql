-- Session links for one-part timers and joined sleeps.
ALTER TABLE public.sleep_entries ADD COLUMN payload_version smallint NOT NULL DEFAULT 1
    CHECK (payload_version IN (1,2));
ALTER TABLE public.sleep_entries ADD COLUMN sleep_session_id uuid;
ALTER TABLE public.sleep_entries ADD CONSTRAINT sleep_session_requires_v2
    CHECK (sleep_session_id IS NULL OR payload_version = 2);
ALTER TABLE public.active_timers ADD COLUMN session_id uuid;
ALTER TABLE public.active_timers DROP CONSTRAINT active_timers_component_version_check;
ALTER TABLE public.active_timers ADD CONSTRAINT active_timers_component_version_check
    CHECK (component_version IN (1,2,3,4,5));
ALTER TABLE public.active_timers ADD CONSTRAINT active_timers_session_requires_v5
    CHECK (session_id IS NULL OR component_version = 5);
ALTER TABLE public.active_timers DROP CONSTRAINT active_timers_state_shape;
ALTER TABLE public.active_timers ADD CONSTRAINT active_timers_state_shape CHECK (
    (pending_volume_ml IS NULL) = (pending_contents IS NULL)
    AND ((timer_kind='feed' AND (current_side IS NULL)=(current_side_started_at IS NULL)
          AND (current_bottle_started_at IS NULL OR (component_version IN (3,4,5) AND current_side IS NULL)))
         OR (timer_kind='sleep' AND current_side IS NULL AND current_side_started_at IS NULL
             AND current_bottle_started_at IS NULL AND pending_volume_ml IS NULL AND pending_contents IS NULL))
    AND (completed_record_id IS NULL OR tombstoned_at IS NOT NULL)
);
CREATE INDEX sleep_entries_session ON public.sleep_entries(family_id, sleep_session_id)
    WHERE sleep_session_id IS NOT NULL;
CREATE INDEX active_timers_session ON public.active_timers(family_id, session_id)
    WHERE session_id IS NOT NULL;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 26,principal,object_kind,object_identity,privileges
 FROM public.schema_privilege_manifest WHERE schema_version=25;
-- 000025 recorded this grant with the timestamptz alias, but manifests hold
-- function identities as PostgreSQL prints them (regprocedure::text), which is
-- what the bootstrap's exact privilege check compares against.
UPDATE public.schema_privilege_manifest
 SET object_identity='app.cleanup_refresh_recovery_receipts(timestamp with time zone)'
 WHERE schema_version=26 AND principal='baby_tracker_maintenance' AND object_kind='function'
   AND object_identity='app.cleanup_refresh_recovery_receipts(timestamptz)';
UPDATE public.schema_compatibility SET current_version=26,
 introduced_by='000026_sleep_sessions.sql',recorded_at=now() WHERE singleton;
