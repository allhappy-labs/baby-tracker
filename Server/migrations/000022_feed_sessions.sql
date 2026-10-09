-- Preserve logical feeding links and optional absolute step boundaries.
ALTER TABLE public.feed_entries ADD COLUMN feeding_session_id uuid;
ALTER TABLE public.feed_components ADD COLUMN started_at timestamptz;
ALTER TABLE public.feed_components ADD COLUMN ended_at timestamptz;
ALTER TABLE public.timer_components ADD COLUMN started_at timestamptz;
ALTER TABLE public.timer_components ADD COLUMN ended_at timestamptz;

ALTER TABLE public.feed_entries DROP CONSTRAINT feed_entries_component_version_check;
ALTER TABLE public.feed_entries ADD CONSTRAINT feed_entries_component_version_check CHECK (component_version IN (1,2,3,4));
ALTER TABLE public.active_timers DROP CONSTRAINT active_timers_component_version_check;
ALTER TABLE public.active_timers ADD CONSTRAINT active_timers_component_version_check CHECK (component_version IN (1,2,3,4));
ALTER TABLE public.active_timers DROP CONSTRAINT active_timers_state_shape;
ALTER TABLE public.active_timers ADD CONSTRAINT active_timers_state_shape CHECK (
    (pending_volume_ml IS NULL) = (pending_contents IS NULL)
    AND ((timer_kind='feed' AND (current_side IS NULL)=(current_side_started_at IS NULL)
          AND (current_bottle_started_at IS NULL OR (component_version IN (3,4) AND current_side IS NULL)))
         OR (timer_kind='sleep' AND current_side IS NULL AND current_side_started_at IS NULL
             AND current_bottle_started_at IS NULL AND pending_volume_ml IS NULL AND pending_contents IS NULL))
    AND (completed_record_id IS NULL OR tombstoned_at IS NOT NULL)
);
ALTER TABLE public.feed_components ADD CONSTRAINT feed_component_time_order CHECK (
    started_at IS NULL OR ended_at IS NULL OR ended_at > started_at
);
ALTER TABLE public.timer_components ADD CONSTRAINT timer_component_time_order CHECK (
    started_at IS NULL OR ended_at IS NULL OR ended_at > started_at
);

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 22,principal,object_kind,object_identity,privileges
 FROM public.schema_privilege_manifest WHERE schema_version=21;
UPDATE public.schema_compatibility SET current_version=22,
 introduced_by='000022_feed_sessions.sql',recorded_at=now() WHERE singleton;
