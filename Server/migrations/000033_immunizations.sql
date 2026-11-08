-- 000033_immunizations: care kind 9, immunization. given_at is stored in
-- started_at; the vaccine and dose live in wire. No table, policy or grant changes.
ALTER TABLE public.care_entities DROP CONSTRAINT care_entities_kind_check;
ALTER TABLE public.care_entities ADD CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 9)));

ALTER TABLE public.care_entities DROP CONSTRAINT care_entities_kind_shape;
ALTER TABLE public.care_entities ADD CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8, 9])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL)))));

ALTER TABLE public.sync_page_bindings DROP CONSTRAINT sync_page_bindings_resume_kind_check;
ALTER TABLE public.sync_page_bindings ADD CONSTRAINT sync_page_bindings_resume_kind_check CHECK (((resume_kind >= 1) AND (resume_kind <= 9)));

ALTER TABLE public.import_record_manifests DROP CONSTRAINT import_record_manifests_entity_kind_check;
ALTER TABLE public.import_record_manifests ADD CONSTRAINT import_record_manifests_entity_kind_check
  CHECK ((entity_kind = ANY (ARRAY['feed'::text, 'sleep'::text, 'diaper'::text, 'timer'::text, 'supplement'::text, 'growth_profile'::text, 'measurement'::text, 'immunization'::text])));

COMMENT ON COLUMN public.care_entities.kind IS 'care kind: 1 baby, 2 feed, 3 sleep, 4 diaper, 5 timer, 6 supplement, 7 growth_profile, 8 measurement, 9 immunization';
COMMENT ON COLUMN public.sync_page_bindings.resume_kind IS 'care kind: 1 baby, 2 feed, 3 sleep, 4 diaper, 5 timer, 6 supplement, 7 growth_profile, 8 measurement, 9 immunization';

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 33,principal,object_kind,object_identity,privileges
 FROM public.schema_privilege_manifest WHERE schema_version=32;
UPDATE public.schema_compatibility SET current_version=33,
 introduced_by='000033_immunizations.sql',recorded_at=now() WHERE singleton;
