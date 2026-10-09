-- Persist an active bottle phase and optional measured bottle durations.
ALTER TABLE public.mutation_journal DROP CONSTRAINT mutation_journal_operation_check;
ALTER TABLE public.mutation_journal ADD CONSTRAINT mutation_journal_operation_check CHECK (
    operation IN ('create','update','delete','timer_start','timer_switch_side','timer_attach_bottle',
                  'timer_start_bottle','timer_finish_bottle','timer_stop')
);
ALTER TABLE public.feed_entries DROP CONSTRAINT feed_entries_component_version_check;
ALTER TABLE public.feed_entries ADD CONSTRAINT feed_entries_component_version_check CHECK (component_version IN (1,2,3));
ALTER TABLE public.active_timers DROP CONSTRAINT active_timers_component_version_check;
ALTER TABLE public.active_timers ADD CONSTRAINT active_timers_component_version_check CHECK (component_version IN (1,2,3));
ALTER TABLE public.active_timers ADD COLUMN current_bottle_started_at timestamptz;
ALTER TABLE public.active_timers DROP CONSTRAINT active_timers_state_shape;
ALTER TABLE public.active_timers ADD CONSTRAINT active_timers_state_shape CHECK (
    (pending_volume_ml IS NULL) = (pending_contents IS NULL)
    AND ((timer_kind='feed' AND (current_side IS NULL)=(current_side_started_at IS NULL)
          AND (current_bottle_started_at IS NULL OR (component_version=3 AND current_side IS NULL)))
         OR (timer_kind='sleep' AND current_side IS NULL AND current_side_started_at IS NULL
             AND current_bottle_started_at IS NULL AND pending_volume_ml IS NULL AND pending_contents IS NULL))
    AND (completed_record_id IS NULL OR tombstoned_at IS NOT NULL)
);
ALTER TABLE public.feed_components DROP CONSTRAINT feed_components_shape;
ALTER TABLE public.feed_components ADD CONSTRAINT feed_components_shape CHECK (
    (kind='breast' AND side IS NOT NULL AND seconds IS NOT NULL AND volume_ml IS NULL AND contents IS NULL AND foods IS NULL AND amount IS NULL)
    OR (kind='bottle' AND side IS NULL AND volume_ml IS NOT NULL AND contents IS NOT NULL AND foods IS NULL AND amount IS NULL)
    OR (kind='solids' AND side IS NULL AND seconds IS NULL AND volume_ml IS NULL AND contents IS NULL AND foods IS NOT NULL AND amount IS NOT NULL AND cardinality(foods) BETWEEN 0 AND 50)
);
ALTER TABLE public.timer_components DROP CONSTRAINT timer_components_shape;
ALTER TABLE public.timer_components ADD CONSTRAINT timer_components_shape CHECK (
    (kind='breast' AND side IS NOT NULL AND seconds IS NOT NULL AND volume_ml IS NULL AND contents IS NULL AND foods IS NULL AND amount IS NULL)
    OR (kind='bottle' AND side IS NULL AND volume_ml IS NOT NULL AND contents IS NOT NULL AND foods IS NULL AND amount IS NULL)
    OR (kind='solids' AND side IS NULL AND seconds IS NULL AND volume_ml IS NULL AND contents IS NULL AND foods IS NOT NULL AND amount IS NOT NULL AND cardinality(foods) BETWEEN 0 AND 50)
);

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 21,principal,object_kind,object_identity,privileges
 FROM public.schema_privilege_manifest WHERE schema_version=20;
UPDATE public.schema_compatibility SET current_version=21,
 introduced_by='000021_timed_bottles.sql',recorded_at=now() WHERE singleton;
