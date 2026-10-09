-- A feed may remain open with no breast or bottle component running.
ALTER TABLE public.mutation_journal DROP CONSTRAINT mutation_journal_operation_check;
ALTER TABLE public.mutation_journal ADD CONSTRAINT mutation_journal_operation_check CHECK (
    operation IN ('create','update','delete','timer_start','timer_switch_side','timer_pause',
                  'timer_attach_bottle','timer_start_bottle','timer_finish_bottle','timer_stop')
);

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 23,principal,object_kind,object_identity,privileges
 FROM public.schema_privilege_manifest WHERE schema_version=22;
UPDATE public.schema_compatibility SET current_version=23,
 introduced_by='000023_timer_pause.sql',recorded_at=now() WHERE singleton;
