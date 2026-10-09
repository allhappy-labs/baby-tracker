-- A baby's switcher icon. Assigned by the app, never derived here.
ALTER TABLE public.babies ADD COLUMN icon text
  CHECK (icon IS NULL OR icon IN ('boy.1','boy.2','boy.3','boy.4','girl.1','girl.2','girl.3','girl.4'));

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 29,principal,object_kind,object_identity,privileges
 FROM public.schema_privilege_manifest WHERE schema_version=28;
UPDATE public.schema_compatibility SET current_version=29,
 introduced_by='000029_baby_icon.sql',recorded_at=now() WHERE singleton;
