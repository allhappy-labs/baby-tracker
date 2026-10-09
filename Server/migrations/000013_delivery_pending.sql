-- Durable per-device cursor wake intent. Application queue/live composition
-- lands in the following feature commit; this migration is the deployable
-- overlap predecessor for schema 12 -> 13.

CREATE TABLE public.delivery_pending (
    family_id uuid NOT NULL,
    device_id uuid NOT NULL,
    instance_id uuid NOT NULL,
    recovery_epoch bigint NOT NULL,
    sequence bigint NOT NULL,
    generation bigint NOT NULL,
    not_before timestamptz NOT NULL,
    attempts bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (family_id, device_id),
    CONSTRAINT delivery_pending_family_fk FOREIGN KEY (family_id)
        REFERENCES public.families(id) ON DELETE CASCADE,
    CONSTRAINT delivery_pending_device_fk FOREIGN KEY (device_id)
        REFERENCES public.devices(id) ON DELETE CASCADE,
    CONSTRAINT delivery_pending_uuid_shape CHECK (
        family_id <> '00000000-0000-0000-0000-000000000000'
        AND device_id <> '00000000-0000-0000-0000-000000000000'
        AND instance_id <> '00000000-0000-0000-0000-000000000000'),
    CONSTRAINT delivery_pending_cursor_bounds CHECK (
        recovery_epoch BETWEEN 1 AND 9007199254740991
        AND sequence BETWEEN 1 AND 9007199254740991
        AND generation BETWEEN 1 AND 9007199254740991),
    CONSTRAINT delivery_pending_attempts_nonnegative CHECK (attempts >= 0),
    CONSTRAINT delivery_pending_timestamp_shape CHECK (
        isfinite(not_before) AND isfinite(created_at) AND isfinite(updated_at)
        AND updated_at >= created_at)
);

CREATE INDEX delivery_pending_due_idx
    ON public.delivery_pending(not_before, family_id, device_id);
CREATE INDEX delivery_pending_created_idx
    ON public.delivery_pending(created_at, family_id, device_id);
CREATE INDEX devices_active_account_device_idx
    ON public.devices(account_id, id) WHERE binding_revoked_at IS NULL;

ALTER TABLE public.delivery_pending ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.delivery_pending FORCE ROW LEVEL SECURITY;
CREATE POLICY family_context ON public.delivery_pending
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());

CREATE FUNCTION app.schedule_family_wakes(
    p_family_id uuid,
    p_actor_device_id uuid,
    p_instance_id uuid,
    p_recovery_epoch bigint,
    p_sequence bigint
) RETURNS void
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    caller_account_id uuid := app.current_account_id();
    caller_family_id uuid := app.current_family_id();
    previous_account text := coalesce(current_setting('app.account_id', true), '');
    target_account_id uuid;
    scheduling_time timestamptz := now();
BEGIN
    IF caller_account_id IS NULL
       OR caller_family_id IS NULL
       OR p_family_id IS NULL
       OR p_family_id = '00000000-0000-0000-0000-000000000000'::uuid
       OR p_family_id IS DISTINCT FROM caller_family_id
       OR p_actor_device_id IS NULL
       OR p_actor_device_id = '00000000-0000-0000-0000-000000000000'::uuid
       OR p_instance_id IS NULL
       OR p_instance_id = '00000000-0000-0000-0000-000000000000'::uuid
       OR p_recovery_epoch IS NULL
       OR p_recovery_epoch NOT BETWEEN 1 AND 9007199254740991
       OR p_sequence IS NULL
       OR p_sequence NOT BETWEEN 1 AND 9007199254740991
       OR NOT EXISTS (
           SELECT FROM public.accounts a
            WHERE a.id = caller_account_id AND a.status = 'active'
       )
       OR NOT EXISTS (
           SELECT FROM public.families f
            WHERE f.id = p_family_id AND f.status = 'active'
              AND f.next_sequence = p_sequence
       )
       OR NOT EXISTS (
           SELECT FROM public.family_slots s
            WHERE s.account_id = caller_account_id
              AND s.family_id = p_family_id AND s.state = 'active'
       )
       OR NOT EXISTS (
           SELECT FROM public.memberships m
            WHERE m.family_id = p_family_id
              AND m.account_id = caller_account_id AND m.status = 'active'
       )
       OR NOT EXISTS (
           SELECT FROM public.family_entitlements e
            WHERE e.family_id = p_family_id
              AND e.entitlement_key = 'family_sync' AND e.status = 'active'
              AND (e.expires_at IS NULL OR e.expires_at > scheduling_time)
       )
       OR NOT EXISTS (
           SELECT FROM public.devices d
            WHERE d.id = p_actor_device_id AND d.account_id = caller_account_id
              AND d.server_instance_id = p_instance_id
              AND d.binding_revoked_at IS NULL
       )
       OR NOT EXISTS (
           SELECT FROM public.server_state s
            WHERE s.singleton AND s.instance_id = p_instance_id
              AND s.recovery_epoch = p_recovery_epoch
       ) THEN
        RAISE EXCEPTION 'family wake scheduling denied' USING ERRCODE = '42501';
    END IF;

    FOR target_account_id IN
        SELECT m.account_id
          FROM public.memberships m
         WHERE m.family_id = p_family_id AND m.status = 'active'
         ORDER BY m.account_id
    LOOP
        -- accounts and family_slots are forced-RLS account tables. Preserve
        -- their semantics by selecting each member only under its own account
        -- context instead of granting the runtime a cross-account helper.
        PERFORM set_config('app.account_id', target_account_id::text, true);
        IF EXISTS (
            SELECT FROM public.accounts a
             WHERE a.id = target_account_id AND a.status = 'active'
        ) AND EXISTS (
            SELECT FROM public.family_slots s
             WHERE s.account_id = target_account_id
               AND s.family_id = p_family_id AND s.state = 'active'
        ) THEN
            INSERT INTO public.delivery_pending(
                family_id, device_id, instance_id, recovery_epoch, sequence,
                generation, not_before, attempts, created_at, updated_at)
            SELECT p_family_id, d.id, p_instance_id, p_recovery_epoch, p_sequence,
                   1, scheduling_time, 0, scheduling_time, scheduling_time
              FROM public.devices d
             WHERE d.account_id = target_account_id
               AND d.server_instance_id = p_instance_id
               AND d.binding_revoked_at IS NULL
               AND d.id <> p_actor_device_id
            ON CONFLICT (family_id, device_id) DO UPDATE SET
                instance_id = EXCLUDED.instance_id,
                recovery_epoch = EXCLUDED.recovery_epoch,
                sequence = EXCLUDED.sequence,
                generation = public.delivery_pending.generation + 1,
                not_before = CASE
                    WHEN public.delivery_pending.instance_id IS DISTINCT FROM EXCLUDED.instance_id
                      OR public.delivery_pending.recovery_epoch IS DISTINCT FROM EXCLUDED.recovery_epoch
                    THEN EXCLUDED.not_before
                    ELSE public.delivery_pending.not_before
                END,
                attempts = CASE
                    WHEN public.delivery_pending.instance_id IS DISTINCT FROM EXCLUDED.instance_id
                      OR public.delivery_pending.recovery_epoch IS DISTINCT FROM EXCLUDED.recovery_epoch
                    THEN 0
                    ELSE public.delivery_pending.attempts
                END,
                created_at = EXCLUDED.created_at,
                updated_at = EXCLUDED.updated_at
            WHERE (
                public.delivery_pending.instance_id IS DISTINCT FROM EXCLUDED.instance_id
                OR public.delivery_pending.recovery_epoch IS DISTINCT FROM EXCLUDED.recovery_epoch
                OR public.delivery_pending.sequence < EXCLUDED.sequence
            );
        END IF;
    END LOOP;
    PERFORM set_config('app.account_id', previous_account, true);
EXCEPTION WHEN OTHERS THEN
    PERFORM set_config('app.account_id', previous_account, true);
    RAISE;
END
$$;

REVOKE ALL ON public.delivery_pending
    FROM PUBLIC, baby_tracker_runtime, baby_tracker_projector,
    baby_tracker_apple_revoker;
REVOKE ALL ON FUNCTION app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)
    FROM PUBLIC, baby_tracker_projector, baby_tracker_apple_revoker;
GRANT EXECUTE ON FUNCTION app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)
    TO baby_tracker_runtime;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
SELECT 13,principal,object_kind,object_identity,privileges
  FROM public.schema_privilege_manifest WHERE schema_version=12;
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
    (13,'baby_tracker_runtime','function','app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)','EXECUTE');

UPDATE public.schema_compatibility
   SET current_version=13,introduced_by='000013_delivery_pending.sql',recorded_at=now()
 WHERE singleton;
