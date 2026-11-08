-- 000034_pumping: care kind 10, pump. A pumping session belongs to the
-- family, not a baby: baby_id is NULL, as it is for a baby row, so the
-- baby-delete cascade and every per-baby read never see it. started_at and
-- ended_at are typed; the side, the amount and the note live in wire. A
-- family has at most one pump still running. No table, policy or grant changes.
ALTER TABLE public.care_entities DROP CONSTRAINT care_entities_kind_check;
ALTER TABLE public.care_entities ADD CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 10)));

ALTER TABLE public.care_entities DROP CONSTRAINT care_entities_kind_shape;
ALTER TABLE public.care_entities ADD CONSTRAINT care_entities_kind_shape CHECK ((((kind = ANY (ARRAY[1, 10])) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8, 9, 10])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4, 10]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL)))));

-- care_entities_seven_days already holds ended_at at or after started_at.
ALTER TABLE public.care_entities ADD CONSTRAINT care_entities_pump_six_hours
  CHECK (((kind <> 10) OR (ended_at IS NULL) OR (ended_at <= (started_at + '06:00:00'::interval))));

-- The backstop for "one running pump per family", and the index the write
-- path reads the running pump through. The rule itself is decided under the
-- family lock (care.applyRecord), which answers superseded; this makes a
-- missed check an error rather than a second running pump.
CREATE UNIQUE INDEX care_entities_one_open_pump ON public.care_entities USING btree (family_id)
  WHERE ((kind = 10) AND (tombstoned_at IS NULL) AND (ended_at IS NULL));

ALTER TABLE public.sync_page_bindings DROP CONSTRAINT sync_page_bindings_resume_kind_check;
ALTER TABLE public.sync_page_bindings ADD CONSTRAINT sync_page_bindings_resume_kind_check CHECK (((resume_kind >= 1) AND (resume_kind <= 10)));

COMMENT ON COLUMN public.care_entities.kind IS 'care kind: 1 baby, 2 feed, 3 sleep, 4 diaper, 5 timer, 6 supplement, 7 growth_profile, 8 measurement, 9 immunization, 10 pump';
COMMENT ON COLUMN public.sync_page_bindings.resume_kind IS 'care kind: 1 baby, 2 feed, 3 sleep, 4 diaper, 5 timer, 6 supplement, 7 growth_profile, 8 measurement, 9 immunization, 10 pump';

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 34,principal,object_kind,object_identity,privileges
 FROM public.schema_privilege_manifest WHERE schema_version=33;
UPDATE public.schema_compatibility SET current_version=34,
 introduced_by='000034_pumping.sql',recorded_at=now() WHERE singleton;
