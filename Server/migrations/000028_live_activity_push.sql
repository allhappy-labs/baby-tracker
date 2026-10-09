-- Runtime tokens are account/family isolated. Worker functions see only leased
-- scheduling coordinates; the worker role cannot read tokens or care tables.
CREATE TABLE public.live_activity_devices (
 registration_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 installation_id uuid NOT NULL,
 account_id uuid NOT NULL REFERENCES public.accounts(id),
 device_id uuid NOT NULL,
 family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
 instance_id uuid NOT NULL,
 recovery_epoch bigint NOT NULL CHECK(recovery_epoch BETWEEN 1 AND 9007199254740991),
 registration_version bigint NOT NULL CHECK(registration_version BETWEEN 1 AND 9007199254740991),
 environment text NOT NULL CHECK(environment IN ('sandbox','production')),
 topic text NOT NULL CHECK(length(topic) BETWEEN 1 AND 255),
 push_token text NOT NULL CHECK(length(push_token) BETWEEN 2 AND 2048 AND push_token ~ '^[0-9a-f]+$' AND length(push_token)%2=0),
 payload_version bigint NOT NULL CHECK(payload_version=1),
 feed_break_minutes bigint NOT NULL CHECK(feed_break_minutes BETWEEN 5 AND 120 AND feed_break_minutes%5=0),
 session_id uuid NOT NULL,
 lineage_root_session_id uuid NOT NULL,
 membership_revision bigint NOT NULL CHECK(membership_revision BETWEEN 1 AND 9007199254740991),
 authority_expires_at timestamptz NOT NULL CHECK(isfinite(authority_expires_at)),
 revoked boolean NOT NULL DEFAULT false,
 FOREIGN KEY(device_id,account_id) REFERENCES public.devices(id,account_id),
 UNIQUE(instance_id,recovery_epoch,installation_id,family_id),
 UNIQUE(registration_id,account_id,family_id)
);
CREATE UNIQUE INDEX live_activity_device_token_owner ON public.live_activity_devices(environment,topic,push_token) WHERE NOT revoked;
CREATE TABLE public.live_activity_tokens (
 registration_id uuid NOT NULL,
 account_id uuid NOT NULL,
 family_id uuid NOT NULL,
 activity_id text NOT NULL CHECK(length(activity_id) BETWEEN 1 AND 256),
 baby_id uuid NOT NULL,
 kind text NOT NULL CHECK(kind IN ('feed','sleep')),
 environment text NOT NULL CHECK(environment IN ('sandbox','production')),
 topic text NOT NULL,
 token text NOT NULL CHECK(length(token) BETWEEN 2 AND 2048 AND token ~ '^[0-9a-f]+$' AND length(token)%2=0),
 token_version bigint NOT NULL CHECK(token_version BETWEEN 1 AND 9007199254740991),
 generation_id uuid NOT NULL DEFAULT gen_random_uuid(),
 known_generation_id uuid,
 timer_id uuid,
 session_id uuid,
 state text NOT NULL CHECK(state IN ('active','ended','dismissed')),
 PRIMARY KEY(registration_id,activity_id),
 FOREIGN KEY(registration_id,account_id,family_id) REFERENCES public.live_activity_devices(registration_id,account_id,family_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX live_activity_update_token_owner ON public.live_activity_tokens(environment,topic,token) WHERE state='active';
-- A single namespace covers both token kinds, including cross-kind takeover.
CREATE TABLE public.live_activity_token_owners (
 environment text NOT NULL,topic text NOT NULL,token text NOT NULL,
 registration_id uuid NOT NULL REFERENCES public.live_activity_devices(registration_id) ON DELETE CASCADE,
 slot text NOT NULL,PRIMARY KEY(environment,topic,token),UNIQUE(registration_id,slot)
);
CREATE FUNCTION app.live_activity_token_ownership() RETURNS trigger
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE token_slot text; live boolean; value text;
BEGIN
 IF TG_TABLE_NAME='live_activity_devices' THEN token_slot:='device';live:=NOT NEW.revoked AND NEW.push_token IS NOT NULL;value:=NEW.push_token;
 ELSE token_slot:='activity:'||NEW.activity_id;live:=NEW.state='active' AND NEW.token IS NOT NULL;value:=NEW.token;END IF;
 DELETE FROM public.live_activity_token_owners WHERE registration_id=NEW.registration_id AND slot=token_slot;
 IF live THEN
 INSERT INTO public.live_activity_token_owners VALUES(NEW.environment,NEW.topic,value,NEW.registration_id,token_slot);
 END IF;RETURN NULL;
END $$;
REVOKE ALL ON FUNCTION app.live_activity_token_ownership() FROM PUBLIC;
CREATE TRIGGER live_activity_device_token_owner AFTER INSERT OR UPDATE ON public.live_activity_devices FOR EACH ROW EXECUTE FUNCTION app.live_activity_token_ownership();
CREATE TRIGGER live_activity_update_token_owner AFTER INSERT OR UPDATE ON public.live_activity_tokens FOR EACH ROW EXECUTE FUNCTION app.live_activity_token_ownership();

ALTER TABLE public.live_activity_devices ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.live_activity_devices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_registration ON public.live_activity_devices
 USING(account_id=app.current_account_id() AND family_id=app.current_family_id())
 WITH CHECK(account_id=app.current_account_id() AND family_id=app.current_family_id());
ALTER TABLE public.live_activity_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.live_activity_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_registration ON public.live_activity_tokens
 USING(account_id=app.current_account_id() AND family_id=app.current_family_id())
 WITH CHECK(account_id=app.current_account_id() AND family_id=app.current_family_id());
GRANT SELECT,INSERT,UPDATE ON public.live_activity_devices,public.live_activity_tokens TO baby_tracker_runtime;

-- No token or care payload in the scheduling index. Only trigger/worker definers
-- can access it. It deliberately does not inherit actor-account RLS.
CREATE TABLE public.live_activity_registration_index (
 registration_id uuid PRIMARY KEY REFERENCES public.live_activity_devices(registration_id) ON DELETE CASCADE,
 account_id uuid NOT NULL,device_id uuid NOT NULL,family_id uuid NOT NULL,installation_id uuid NOT NULL,
 instance_id uuid NOT NULL,recovery_epoch bigint NOT NULL,registration_version bigint NOT NULL,
 authority_expires_at timestamptz NOT NULL,revoked boolean NOT NULL
);
CREATE INDEX live_activity_index_family ON public.live_activity_registration_index(family_id);
CREATE TABLE public.live_activity_family_dirty (
 family_id uuid PRIMARY KEY REFERENCES public.families(id) ON DELETE CASCADE,
 instance_id uuid NOT NULL,recovery_epoch bigint NOT NULL,
 dirty_version bigint NOT NULL DEFAULT 1 CHECK(dirty_version>0),
 journal_sequence bigint NOT NULL DEFAULT 0 CHECK(journal_sequence>=0),
 authority_checked_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE public.live_activity_delivery (
 job_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 registration_id uuid NOT NULL REFERENCES public.live_activity_registration_index(registration_id) ON DELETE CASCADE,
 registration_version bigint NOT NULL,
 account_id uuid NOT NULL,device_id uuid NOT NULL,
 family_id uuid NOT NULL,baby_id uuid NOT NULL,kind text NOT NULL CHECK(kind IN ('feed','sleep')),
 instance_id uuid NOT NULL,recovery_epoch bigint NOT NULL,
 dirty_version bigint NOT NULL,completed_version bigint NOT NULL DEFAULT 0,
 not_before timestamptz NOT NULL DEFAULT now(),attempts bigint NOT NULL DEFAULT 0,
 lease_id uuid,lease_deadline timestamptz,leased_dirty_version bigint,leased_registration_version bigint,
 last_reason text,
 UNIQUE(registration_id,baby_id,kind),
 CHECK((lease_id IS NULL AND lease_deadline IS NULL AND leased_dirty_version IS NULL) OR
       (lease_id IS NOT NULL AND lease_deadline IS NOT NULL AND leased_dirty_version IS NOT NULL))
);
CREATE INDEX live_activity_delivery_due ON public.live_activity_delivery(not_before) WHERE dirty_version>completed_version;

-- Installation-wide version fence prevents a delayed old-family request from
-- reactivating a scope after a family switch.
CREATE TABLE public.live_activity_installations (
 instance_id uuid NOT NULL,recovery_epoch bigint NOT NULL,installation_id uuid NOT NULL,
 account_id uuid NOT NULL,device_id uuid NOT NULL,family_id uuid NOT NULL,
 version bigint NOT NULL,revoked boolean NOT NULL DEFAULT false,authority_expires_at timestamptz NOT NULL,
 PRIMARY KEY(instance_id,recovery_epoch,installation_id)
);
-- Recovery makes all earlier realm/epoch token claims ineligible, irrespective
-- of account. Retire only against the database identity, never a caller claim.
CREATE FUNCTION app.retire_stale_live_activity_registrations() RETURNS void
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r record; realm public.server_state; previous_account text:=coalesce(current_setting('app.account_id',true),'');previous_family text:=coalesce(current_setting('app.family_id',true),'');
BEGIN
 SELECT * INTO realm FROM public.server_state WHERE singleton;
 IF NOT FOUND THEN RETURN;END IF;
 FOR r IN SELECT * FROM public.live_activity_registration_index
 WHERE NOT revoked AND (instance_id<>realm.instance_id OR recovery_epoch<>realm.recovery_epoch)
 ORDER BY registration_id LOOP
 PERFORM set_config('app.account_id',r.account_id::text,true);PERFORM set_config('app.family_id',r.family_id::text,true);
 UPDATE public.live_activity_devices SET revoked=true WHERE registration_id=r.registration_id AND NOT revoked;
 END LOOP;
 PERFORM set_config('app.account_id',previous_account,true);PERFORM set_config('app.family_id',previous_family,true);
END $$;
REVOKE ALL ON FUNCTION app.retire_stale_live_activity_registrations() FROM PUBLIC;

CREATE FUNCTION app.fence_live_activity_installation(p_instance uuid,p_epoch bigint,p_installation uuid,p_account uuid,p_device uuid,p_family uuid,p_version bigint,p_expires timestamptz,p_revoked boolean)
 RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE owned public.live_activity_installations; old_scope record; previous_family text:=coalesce(current_setting('app.family_id',true),'');
BEGIN
 IF NOT EXISTS(SELECT FROM public.server_state WHERE singleton AND instance_id=p_instance AND recovery_epoch=p_epoch) THEN RAISE EXCEPTION 'registration realm denied' USING ERRCODE='42501';END IF;
 PERFORM app.retire_stale_live_activity_registrations();
 IF p_account IS DISTINCT FROM app.current_account_id() OR p_family IS DISTINCT FROM app.current_family_id() OR p_version IS NULL OR p_version NOT BETWEEN 1 AND 9007199254740991 THEN RETURN false; END IF;
 INSERT INTO public.live_activity_installations VALUES(p_instance,p_epoch,p_installation,p_account,p_device,p_family,p_version,p_revoked,p_expires) ON CONFLICT DO NOTHING;
 SELECT * INTO owned FROM public.live_activity_installations WHERE instance_id=p_instance AND recovery_epoch=p_epoch AND installation_id=p_installation FOR UPDATE;
 IF owned.account_id<>p_account OR owned.device_id<>p_device THEN RAISE EXCEPTION 'installation ownership denied' USING ERRCODE='42501'; END IF;
 IF owned.version>p_version OR (owned.version=p_version AND (owned.family_id<>p_family OR owned.revoked<>p_revoked)) THEN RETURN false; END IF;
 IF p_expires IS NULL OR NOT isfinite(p_expires) OR NOT EXISTS(
 SELECT FROM public.sessions s JOIN public.devices d ON d.id=s.device_id AND d.account_id=s.account_id
 WHERE s.account_id=p_account AND s.device_id=p_device AND s.server_instance_id=p_instance AND s.recovery_epoch=p_epoch
 AND s.family_id=p_family AND s.scope='family' AND s.absolute_expires_at=p_expires AND s.status='active' AND s.revoked_at IS NULL AND d.binding_revoked_at IS NULL) THEN
 RAISE EXCEPTION 'installation authority denied' USING ERRCODE='42501'; END IF;
 IF owned.version<p_version THEN
 UPDATE public.live_activity_installations SET version=p_version,family_id=p_family,revoked=p_revoked,authority_expires_at=greatest(authority_expires_at,p_expires)
 WHERE instance_id=p_instance AND recovery_epoch=p_epoch AND installation_id=p_installation;
 END IF;
 FOR old_scope IN SELECT r.family_id FROM public.live_activity_registration_index r
 WHERE r.instance_id=p_instance AND r.recovery_epoch=p_epoch AND r.installation_id=p_installation AND r.family_id<>p_family AND NOT r.revoked LOOP
 PERFORM set_config('app.family_id',old_scope.family_id::text,true);
 UPDATE public.live_activity_devices SET revoked=true,registration_version=p_version WHERE instance_id=p_instance AND recovery_epoch=p_epoch AND installation_id=p_installation AND family_id=old_scope.family_id AND NOT revoked;
 END LOOP;
 PERFORM set_config('app.family_id',previous_family,true);RETURN true;
END $$;
REVOKE ALL ON FUNCTION app.fence_live_activity_installation(uuid,bigint,uuid,uuid,uuid,uuid,bigint,timestamptz,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.fence_live_activity_installation(uuid,bigint,uuid,uuid,uuid,uuid,bigint,timestamptz,boolean) TO baby_tracker_runtime;

CREATE FUNCTION app.live_activity_session_binding(p_account uuid,p_device uuid,p_session uuid,p_epoch bigint)
 RETURNS TABLE(instance_id uuid,lineage_root_session_id uuid,authority_expires_at timestamptz)
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT s.server_instance_id,s.lineage_root_session_id,s.absolute_expires_at
 FROM public.sessions s JOIN public.devices d ON d.id=s.device_id AND d.account_id=s.account_id
 WHERE p_account=app.current_account_id() AND s.account_id=p_account AND s.device_id=p_device
 AND s.session_id=p_session AND s.recovery_epoch=p_epoch AND s.status='active' AND s.revoked_at IS NULL
 AND EXISTS(SELECT FROM public.server_state realm WHERE realm.singleton AND realm.instance_id=s.server_instance_id AND realm.recovery_epoch=s.recovery_epoch)
 AND d.binding_revoked_at IS NULL AND s.absolute_expires_at>clock_timestamp() AND s.family_id=app.current_family_id() AND s.scope='family'
$$;
REVOKE ALL ON FUNCTION app.live_activity_session_binding(uuid,uuid,uuid,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.live_activity_session_binding(uuid,uuid,uuid,bigint) TO baby_tracker_runtime;

CREATE FUNCTION app.dirty_live_activity_family(p_family uuid,p_sequence bigint) RETURNS void
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v bigint; realm public.server_state; previous_family text:=coalesce(current_setting('app.family_id',true),'');
BEGIN
 IF NOT EXISTS(SELECT FROM public.live_activity_registration_index WHERE family_id=p_family AND NOT revoked) THEN RETURN; END IF;
 SELECT * INTO realm FROM public.server_state WHERE singleton;IF NOT FOUND THEN RETURN;END IF;
 INSERT INTO public.live_activity_family_dirty(family_id,instance_id,recovery_epoch,journal_sequence) VALUES(p_family,realm.instance_id,realm.recovery_epoch,p_sequence)
 ON CONFLICT(family_id) DO UPDATE SET dirty_version=public.live_activity_family_dirty.dirty_version+1,
 instance_id=EXCLUDED.instance_id,recovery_epoch=EXCLUDED.recovery_epoch,
 journal_sequence=CASE WHEN public.live_activity_family_dirty.instance_id<>EXCLUDED.instance_id OR public.live_activity_family_dirty.recovery_epoch<>EXCLUDED.recovery_epoch THEN EXCLUDED.journal_sequence ELSE greatest(public.live_activity_family_dirty.journal_sequence,EXCLUDED.journal_sequence) END
 RETURNING dirty_version INTO v;
 PERFORM set_config('app.family_id',p_family::text,true);
 INSERT INTO public.live_activity_delivery(registration_id,registration_version,account_id,device_id,family_id,baby_id,kind,instance_id,recovery_epoch,dirty_version)
 SELECT r.registration_id,r.registration_version,r.account_id,r.device_id,r.family_id,b.id,k.kind,r.instance_id,r.recovery_epoch,v
 FROM public.live_activity_registration_index r CROSS JOIN (VALUES('feed'),('sleep')) k(kind)
 JOIN public.babies b ON b.family_id=r.family_id
 WHERE r.family_id=p_family AND NOT r.revoked AND r.instance_id=realm.instance_id AND r.recovery_epoch=realm.recovery_epoch
 ON CONFLICT(registration_id,baby_id,kind) DO UPDATE SET
 dirty_version=EXCLUDED.dirty_version,registration_version=EXCLUDED.registration_version,not_before=least(public.live_activity_delivery.not_before,now());
 PERFORM set_config('app.family_id',previous_family,true);
END $$;
REVOKE ALL ON FUNCTION app.dirty_live_activity_family(uuid,bigint) FROM PUBLIC;
CREATE FUNCTION app.live_activity_journal_dirty() RETURNS trigger
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE affected record;
BEGIN
 FOR affected IN SELECT family_id,max(sequence) AS sequence FROM inserted_journal GROUP BY family_id ORDER BY family_id LOOP
 PERFORM app.dirty_live_activity_family(affected.family_id,affected.sequence);
 END LOOP; RETURN NULL;
END $$;
REVOKE ALL ON FUNCTION app.live_activity_journal_dirty() FROM PUBLIC;
CREATE TRIGGER live_activity_journal_dirty AFTER INSERT ON public.mutation_journal
 REFERENCING NEW TABLE AS inserted_journal FOR EACH STATEMENT EXECUTE FUNCTION app.live_activity_journal_dirty();
CREATE FUNCTION app.live_activity_registration_dirty() RETURNS trigger
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 -- Journal enqueue owns dirty before delivery. Revocation and coordinate
 -- retirement must take the same lock before deleting deliveries or ending
 -- tokens (whose update trigger schedules the family again).
 PERFORM 1 FROM public.live_activity_family_dirty WHERE family_id=NEW.family_id FOR UPDATE;
 IF TG_TABLE_NAME='live_activity_devices' THEN
 INSERT INTO public.live_activity_registration_index VALUES(NEW.registration_id,NEW.account_id,NEW.device_id,NEW.family_id,NEW.installation_id,NEW.instance_id,NEW.recovery_epoch,NEW.registration_version,NEW.authority_expires_at,NEW.revoked)
 ON CONFLICT(registration_id) DO UPDATE SET family_id=EXCLUDED.family_id,registration_version=EXCLUDED.registration_version,authority_expires_at=EXCLUDED.authority_expires_at,revoked=EXCLUDED.revoked;
 IF TG_OP='UPDATE' AND (OLD.environment IS DISTINCT FROM NEW.environment OR OLD.topic IS DISTINCT FROM NEW.topic) THEN
 -- Keep token version/generation tombstones; an old identical retry cannot
 -- resurrect a token in a different APNs namespace. A newer token version
 -- explicitly binds it to the device's current environment/topic.
 UPDATE public.live_activity_tokens SET state='ended' WHERE registration_id=NEW.registration_id AND state='active'
 AND (environment IS DISTINCT FROM NEW.environment OR topic IS DISTINCT FROM NEW.topic);
 END IF;
 IF NEW.revoked THEN
 DELETE FROM public.live_activity_delivery WHERE registration_id=NEW.registration_id;
 UPDATE public.live_activity_tokens SET state='ended' WHERE registration_id=NEW.registration_id AND state='active';
 END IF;
 END IF;
 PERFORM app.dirty_live_activity_family(NEW.family_id,0);RETURN NULL;
END $$;
REVOKE ALL ON FUNCTION app.live_activity_registration_dirty() FROM PUBLIC;
CREATE TRIGGER live_activity_device_dirty AFTER INSERT OR UPDATE ON public.live_activity_devices FOR EACH ROW EXECUTE FUNCTION app.live_activity_registration_dirty();
CREATE TRIGGER live_activity_token_dirty AFTER INSERT OR UPDATE ON public.live_activity_tokens FOR EACH ROW EXECUTE FUNCTION app.live_activity_registration_dirty();

-- Cursor sweep repairs missed notifications/trigger downtime. Only registered
-- families are inspected, under the explicit family RLS context.
CREATE FUNCTION app.sweep_live_activity_work() RETURNS void
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r record; seq bigint; realm public.server_state; previous_family text:=coalesce(current_setting('app.family_id',true),'');
BEGIN
 SELECT * INTO realm FROM public.server_state WHERE singleton;IF NOT FOUND THEN RETURN;END IF;
 FOR r IN SELECT DISTINCT family_id FROM public.live_activity_registration_index WHERE NOT revoked AND instance_id=realm.instance_id AND recovery_epoch=realm.recovery_epoch ORDER BY family_id LOOP
 PERFORM set_config('app.family_id',r.family_id::text,true);
 SELECT next_sequence INTO seq FROM public.families WHERE id=r.family_id;
 IF EXISTS(SELECT FROM public.live_activity_family_dirty WHERE family_id=r.family_id AND authority_checked_at<=clock_timestamp()-interval '60 seconds') OR seq>coalesce((SELECT journal_sequence FROM public.live_activity_family_dirty WHERE family_id=r.family_id AND instance_id=realm.instance_id AND recovery_epoch=realm.recovery_epoch),-1) THEN
 PERFORM app.dirty_live_activity_family(r.family_id,seq);
 UPDATE public.live_activity_family_dirty SET authority_checked_at=clock_timestamp() WHERE family_id=r.family_id;
 END IF;
 END LOOP;
 PERFORM set_config('app.family_id',previous_family,true);
END $$;
REVOKE ALL ON FUNCTION app.sweep_live_activity_work() FROM PUBLIC;
CREATE FUNCTION app.claim_live_activity_work(p_now timestamptz,p_limit integer) RETURNS SETOF jsonb
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF p_now IS NULL OR NOT isfinite(p_now) OR p_limit IS NULL OR p_limit<1 OR p_limit>1000 THEN RAISE EXCEPTION 'invalid claim bounds' USING ERRCODE='22023'; END IF;
 PERFORM app.retire_stale_live_activity_registrations();
 PERFORM app.sweep_live_activity_work();
 RETURN QUERY WITH due AS (
 SELECT d.job_id FROM public.live_activity_delivery d JOIN public.live_activity_registration_index r USING(registration_id)
 WHERE d.dirty_version>d.completed_version AND d.not_before<=p_now AND (d.lease_deadline IS NULL OR d.lease_deadline<=p_now)
 AND NOT r.revoked AND r.authority_expires_at>p_now
 ORDER BY d.not_before,d.job_id LIMIT p_limit FOR UPDATE OF d SKIP LOCKED
 ), claimed AS (
 UPDATE public.live_activity_delivery d SET lease_id=gen_random_uuid(),lease_deadline=p_now+interval '30 seconds',leased_dirty_version=d.dirty_version,leased_registration_version=d.registration_version,attempts=d.attempts+1
 FROM due WHERE d.job_id=due.job_id RETURNING d.*)
 SELECT jsonb_build_object('job_id',job_id,'lease_id',lease_id,'registration_id',registration_id,'registration_version',registration_version,
 'account_id',account_id,'device_id',device_id,'family_id',family_id,'baby_id',baby_id,'kind',kind,'instance_id',instance_id,'recovery_epoch',recovery_epoch,
 'dirty_version',leased_dirty_version,'lease_deadline',lease_deadline) FROM claimed;
END $$;
REVOKE ALL ON FUNCTION app.claim_live_activity_work(timestamptz,integer) FROM PUBLIC;
CREATE FUNCTION app.finish_live_activity_work(p_claim jsonb,p_retry_at timestamptz,p_reason text) RETURNS boolean
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d public.live_activity_delivery; affected integer;
BEGIN
 IF p_retry_at IS NOT NULL AND (NOT isfinite(p_retry_at) OR p_retry_at<clock_timestamp() OR p_reason IS NULL OR p_reason !~ '^[a-z][a-z0-9_]{0,63}$') THEN RAISE EXCEPTION 'invalid retry' USING ERRCODE='22023'; END IF;
 SELECT * INTO d FROM public.live_activity_delivery WHERE job_id=(p_claim->>'job_id')::uuid FOR UPDATE;
 IF NOT FOUND OR d.lease_deadline<=clock_timestamp() OR d.lease_id IS NULL OR d.dirty_version<>d.leased_dirty_version OR
 (d.lease_id,d.registration_id,d.registration_version,d.account_id,d.device_id,d.family_id,d.baby_id,d.kind,d.instance_id,d.recovery_epoch,d.leased_dirty_version,d.lease_deadline)
 IS DISTINCT FROM ((p_claim->>'lease_id')::uuid,(p_claim->>'registration_id')::uuid,(p_claim->>'registration_version')::bigint,
 (p_claim->>'account_id')::uuid,(p_claim->>'device_id')::uuid,(p_claim->>'family_id')::uuid,(p_claim->>'baby_id')::uuid,p_claim->>'kind',
 (p_claim->>'instance_id')::uuid,(p_claim->>'recovery_epoch')::bigint,(p_claim->>'dirty_version')::bigint,(p_claim->>'lease_deadline')::timestamptz)
 THEN RETURN false; END IF;
 UPDATE public.live_activity_delivery SET completed_version=CASE WHEN p_retry_at IS NULL THEN d.dirty_version ELSE completed_version END,
 not_before=coalesce(p_retry_at,not_before),last_reason=p_reason,lease_id=NULL,lease_deadline=NULL,leased_dirty_version=NULL,leased_registration_version=NULL WHERE job_id=d.job_id;
 RETURN true;
END $$;
REVOKE ALL ON FUNCTION app.finish_live_activity_work(jsonb,timestamptz,text) FROM PUBLIC;
CREATE FUNCTION app.retain_live_activity_work() RETURNS bigint
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r record; removed bigint:=0; previous_account text:=coalesce(current_setting('app.account_id',true),'');previous_family text:=coalesce(current_setting('app.family_id',true),'');
BEGIN
 FOR r IN SELECT registration_id,account_id,family_id FROM public.live_activity_registration_index WHERE authority_expires_at<=clock_timestamp() LOOP
 PERFORM set_config('app.account_id',r.account_id::text,true);PERFORM set_config('app.family_id',r.family_id::text,true);
 DELETE FROM public.live_activity_devices WHERE registration_id=r.registration_id AND authority_expires_at<=clock_timestamp();
 IF FOUND THEN removed:=removed+1;END IF;
 END LOOP;
 PERFORM set_config('app.account_id',previous_account,true);PERFORM set_config('app.family_id',previous_family,true);
 DELETE FROM public.live_activity_installations WHERE authority_expires_at<=clock_timestamp();
 DELETE FROM public.live_activity_history WHERE recorded_at<clock_timestamp()-interval '7 days';
 RETURN removed;
END $$;
REVOKE ALL ON FUNCTION app.retain_live_activity_work() FROM PUBLIC;
-- Runtime checks only a fully matching current lease, never enumerates delivery
-- work or reads tokens through a definer. Both tenant coordinates are required.
CREATE FUNCTION app.live_activity_claim_live(p_claim jsonb) RETURNS boolean
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT EXISTS(SELECT FROM public.live_activity_delivery d
 JOIN public.live_activity_registration_index r USING(registration_id)
 JOIN public.server_state s ON s.singleton
 WHERE d.job_id=(p_claim->>'job_id')::uuid AND d.account_id=app.current_account_id() AND d.family_id=app.current_family_id()
 AND NOT r.revoked AND r.registration_version=d.registration_version
 AND s.instance_id=d.instance_id AND s.recovery_epoch=d.recovery_epoch
 AND d.lease_deadline>clock_timestamp() AND d.dirty_version=d.leased_dirty_version
 AND (d.lease_id,d.registration_id,d.registration_version,d.account_id,d.device_id,d.family_id,d.baby_id,d.kind,d.instance_id,d.recovery_epoch,d.leased_dirty_version,d.lease_deadline)
 = ((p_claim->>'lease_id')::uuid,(p_claim->>'registration_id')::uuid,(p_claim->>'registration_version')::bigint,
 (p_claim->>'account_id')::uuid,(p_claim->>'device_id')::uuid,(p_claim->>'family_id')::uuid,(p_claim->>'baby_id')::uuid,p_claim->>'kind',
 (p_claim->>'instance_id')::uuid,(p_claim->>'recovery_epoch')::bigint,(p_claim->>'dirty_version')::bigint,(p_claim->>'lease_deadline')::timestamptz))
$$;
REVOKE ALL ON FUNCTION app.live_activity_claim_live(jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.live_activity_claim_live(jsonb) TO baby_tracker_runtime;

-- Periodic authority sweep enumeration reveals scheduling identities only.
CREATE FUNCTION app.live_activity_registration_subjects(p_after uuid,p_limit integer) RETURNS SETOF jsonb
 LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 1000 THEN RAISE EXCEPTION 'invalid sweep limit' USING ERRCODE='22023';END IF;
 RETURN QUERY SELECT jsonb_build_object('registration_id',r.registration_id,'registration_version',r.registration_version,
 'account_id',r.account_id,'device_id',r.device_id,'family_id',r.family_id,'instance_id',r.instance_id,'recovery_epoch',r.recovery_epoch)
 FROM public.live_activity_registration_index r WHERE NOT r.revoked AND r.registration_id>p_after ORDER BY r.registration_id LIMIT p_limit;
END $$;
REVOKE ALL ON FUNCTION app.live_activity_registration_subjects(uuid,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.live_activity_registration_subjects(uuid,integer) TO baby_tracker_runtime;

GRANT USAGE ON SCHEMA public,app TO baby_tracker_live_activity;
GRANT SELECT ON public.schema_compatibility,public.schema_privilege_manifest TO baby_tracker_live_activity;
GRANT EXECUTE ON FUNCTION app.claim_live_activity_work(timestamptz,integer),app.finish_live_activity_work(jsonb,timestamptz,text),app.retain_live_activity_work() TO baby_tracker_live_activity;
ALTER TABLE public.schema_privilege_manifest DROP CONSTRAINT schema_privilege_manifest_principal_check;
ALTER TABLE public.schema_privilege_manifest ADD CONSTRAINT schema_privilege_manifest_principal_check CHECK(principal IN ('baby_tracker_maintenance','baby_tracker_runtime','baby_tracker_projector','baby_tracker_apple_revoker','baby_tracker_live_activity'));
INSERT INTO public.schema_privilege_manifest SELECT 28,principal,object_kind,object_identity,privileges FROM public.schema_privilege_manifest WHERE schema_version=27;
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
 (28,'baby_tracker_runtime','table','public.live_activity_devices','SELECT,INSERT,UPDATE'),
 (28,'baby_tracker_runtime','table','public.live_activity_tokens','SELECT,INSERT,UPDATE'),
 (28,'baby_tracker_runtime','function','app.fence_live_activity_installation(uuid,bigint,uuid,uuid,uuid,uuid,bigint,timestamp with time zone,boolean)','EXECUTE'),
 (28,'baby_tracker_runtime','function','app.live_activity_session_binding(uuid,uuid,uuid,bigint)','EXECUTE'),
 (28,'baby_tracker_runtime','function','app.live_activity_claim_live(jsonb)','EXECUTE'),
 (28,'baby_tracker_runtime','function','app.live_activity_registration_subjects(uuid,integer)','EXECUTE'),
 (28,'baby_tracker_live_activity','schema','public','USAGE'),(28,'baby_tracker_live_activity','schema','app','USAGE'),
 (28,'baby_tracker_live_activity','table','public.schema_compatibility','SELECT'),(28,'baby_tracker_live_activity','table','public.schema_privilege_manifest','SELECT'),
 (28,'baby_tracker_live_activity','function','app.claim_live_activity_work(timestamp with time zone,integer)','EXECUTE'),
 (28,'baby_tracker_live_activity','function','app.finish_live_activity_work(jsonb,timestamp with time zone,text)','EXECUTE'),
 (28,'baby_tracker_live_activity','function','app.retain_live_activity_work()','EXECUTE');
UPDATE public.schema_compatibility SET current_version=28,introduced_by='000028_live_activity_push.sql',recorded_at=now() WHERE singleton;

-- Canonical projection reads: latest relevant milk/breast and one effective
-- session only, independent of the family's accumulated care history.
CREATE INDEX live_activity_latest_milk ON public.feed_entries
 (family_id,baby_id,(COALESCE(ended_at,started_at)) DESC,(COALESCE(feeding_session_id,id)) DESC,id DESC)
 WHERE tombstoned_at IS NULL AND kind<>'solids';
CREATE INDEX live_activity_latest_breast ON public.feed_entries
 (family_id,baby_id,started_at DESC,id DESC)
 WHERE tombstoned_at IS NULL AND kind='breast';
CREATE INDEX live_activity_session_members ON public.feed_entries
 (family_id,baby_id,feeding_session_id,started_at DESC,id DESC)
 WHERE tombstoned_at IS NULL AND kind<>'solids';

-- Durable lifecycle intent precedes network I/O; diagnostic history has no care
-- payload or raw token. Runtime snapshots remain read-only.
ALTER TABLE public.live_activity_delivery ADD COLUMN target_state jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE public.live_activity_devices ALTER COLUMN push_token DROP NOT NULL;
ALTER TABLE public.live_activity_devices ADD COLUMN registered_sequence bigint NOT NULL DEFAULT 0;
ALTER TABLE public.live_activity_tokens ALTER COLUMN token DROP NOT NULL;
-- One row per send attempt: the event at intent, then APNs' answer to it — the
-- response class, HTTP status and an allowlisted APNs reason, nothing else.
CREATE TABLE public.live_activity_history (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 job_id uuid NOT NULL, event text NOT NULL,
 result text CHECK(result IN ('accepted','invalidToken','retryable','configurationFailure','ambiguous')),
 http_status integer CHECK(http_status BETWEEN 100 AND 599),
 reason text CHECK(reason ~ '^[A-Za-z]{1,64}$'),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX live_activity_history_age ON public.live_activity_history(recorded_at);
CREATE INDEX live_activity_history_job ON public.live_activity_history(job_id,recorded_at);
CREATE FUNCTION app.live_activity_target(p_claim jsonb,p_state jsonb,p_finish boolean,p_deadline timestamptz,p_invalid jsonb,p_outcome jsonb) RETURNS jsonb
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d public.live_activity_delivery; result jsonb; outcome_status integer;
 previous_account text:=coalesce(current_setting('app.account_id',true),'');
 previous_family text:=coalesce(current_setting('app.family_id',true),'');
BEGIN
 -- Token invalidation fires family dirty triggers: match journal lock order.
 PERFORM set_config('app.account_id',p_claim->>'account_id',true);
 PERFORM set_config('app.family_id',p_claim->>'family_id',true);
 -- Registration owns device before its dirty trigger. Take that order before
 -- a captured-token invalidation can touch the same device/token rows.
 PERFORM 1 FROM public.live_activity_devices WHERE registration_id=(p_claim->>'registration_id')::uuid FOR UPDATE;
 PERFORM 1 FROM public.live_activity_family_dirty WHERE family_id=(p_claim->>'family_id')::uuid FOR UPDATE;
 SELECT * INTO d FROM public.live_activity_delivery WHERE job_id=(p_claim->>'job_id')::uuid FOR UPDATE;
 IF NOT app.live_activity_claim_live(p_claim) THEN result:=NULL;
 ELSE
 result:=d.target_state;
 IF p_state IS NOT NULL THEN
 IF jsonb_typeof(p_state)<>'object' OR octet_length(p_state::text)>16384 THEN RAISE EXCEPTION 'invalid lifecycle state' USING ERRCODE='22023';END IF;
 IF p_deadline IS NOT NULL AND NOT isfinite(p_deadline) THEN RAISE EXCEPTION 'invalid lifecycle deadline' USING ERRCODE='22023';END IF;
 UPDATE public.live_activity_delivery SET target_state=p_state,
 completed_version=CASE WHEN p_finish AND p_deadline IS NULL THEN dirty_version ELSE completed_version END,
 not_before=CASE WHEN p_finish THEN coalesce(p_deadline,not_before) ELSE not_before END,
 lease_id=CASE WHEN p_finish THEN NULL ELSE lease_id END,
 lease_deadline=CASE WHEN p_finish THEN NULL ELSE lease_deadline END,
 leased_dirty_version=CASE WHEN p_finish THEN NULL ELSE leased_dirty_version END,
 leased_registration_version=CASE WHEN p_finish THEN NULL ELSE leased_registration_version END
 WHERE job_id=d.job_id;
 IF NOT p_finish AND p_state->>'pending_event' IS NOT NULL THEN
 INSERT INTO public.live_activity_history(job_id,event) VALUES(d.job_id,p_state->>'pending_event');
 END IF;
 -- APNs' answer belongs to this lease's own attempt: the job's latest intent.
 IF p_outcome IS NOT NULL THEN
 IF jsonb_typeof(p_outcome)<>'object' OR coalesce(p_outcome->>'result','') NOT IN ('accepted','invalidToken','retryable','configurationFailure','ambiguous')
 OR coalesce(p_outcome->>'reason','') !~ '^([A-Za-z]{1,64})?$' THEN RAISE EXCEPTION 'invalid push outcome' USING ERRCODE='22023';END IF;
 outcome_status:=(p_outcome->>'http_status')::integer;
 IF outcome_status NOT BETWEEN 100 AND 599 THEN RAISE EXCEPTION 'invalid push outcome' USING ERRCODE='22023';END IF;
 UPDATE public.live_activity_history h SET result=p_outcome->>'result',http_status=outcome_status,reason=p_outcome->>'reason'
 WHERE h.id=(SELECT a.id FROM public.live_activity_history a WHERE a.job_id=d.job_id ORDER BY a.recorded_at DESC,a.id DESC LIMIT 1) AND h.result IS NULL;
 UPDATE public.live_activity_delivery SET last_reason=CASE WHEN p_outcome->>'result'='accepted' THEN NULL ELSE p_outcome->>'result' END WHERE job_id=d.job_id;
 END IF;
 IF p_invalid IS NOT NULL THEN
 IF p_invalid->>'activity_id'='' THEN
 UPDATE public.live_activity_devices SET push_token=NULL WHERE registration_id=d.registration_id AND registration_version=(p_invalid->>'version')::bigint;
 ELSE
 UPDATE public.live_activity_tokens SET token=NULL,state='ended',server_retired=true WHERE registration_id=d.registration_id AND activity_id=p_invalid->>'activity_id' AND token_version=(p_invalid->>'version')::bigint;
 END IF;
 END IF;
 result:=p_state;
 END IF;
 END IF;
 PERFORM set_config('app.account_id',previous_account,true);PERFORM set_config('app.family_id',previous_family,true);
 RETURN result;
END $$;
REVOKE ALL ON FUNCTION app.live_activity_target(jsonb,jsonb,boolean,timestamptz,jsonb,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.live_activity_target(jsonb,jsonb,boolean,timestamptz,jsonb,jsonb) TO baby_tracker_live_activity;
INSERT INTO public.schema_privilege_manifest VALUES(28,'baby_tracker_live_activity','function','app.live_activity_target(jsonb,jsonb,boolean,timestamp with time zone,jsonb,jsonb)','EXECUTE');

-- Known-unsent starts can be released after a snapshot becomes stale. Unlike
-- acknowledgement this intentionally permits newer dirty/registration versions,
-- but never a replaced lease or another persisted generation/attempt. Only the
-- delivery row is changed; no authority, token, or family dirty lock is needed.
CREATE FUNCTION app.abandon_live_activity_start(p_claim jsonb,p_generation text,p_attempt text,p_absence_allowed boolean,p_absence_used boolean) RETURNS boolean
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d public.live_activity_delivery;
BEGIN
 SELECT * INTO d FROM public.live_activity_delivery WHERE job_id=(p_claim->>'job_id')::uuid FOR UPDATE;
 IF NOT FOUND OR d.lease_id IS NULL OR
 (d.lease_id,d.registration_id,d.leased_registration_version,d.account_id,d.device_id,d.family_id,d.baby_id,d.kind,d.instance_id,d.recovery_epoch,d.leased_dirty_version,d.lease_deadline)
 IS DISTINCT FROM ((p_claim->>'lease_id')::uuid,(p_claim->>'registration_id')::uuid,(p_claim->>'registration_version')::bigint,
 (p_claim->>'account_id')::uuid,(p_claim->>'device_id')::uuid,(p_claim->>'family_id')::uuid,(p_claim->>'baby_id')::uuid,p_claim->>'kind',
 (p_claim->>'instance_id')::uuid,(p_claim->>'recovery_epoch')::bigint,(p_claim->>'dirty_version')::bigint,(p_claim->>'lease_deadline')::timestamptz)
 OR (d.target_state->>'generation_id',d.target_state->>'attempt_id',d.target_state->>'start_attempt_id',d.target_state->>'status',d.target_state->>'pending_event')
 IS DISTINCT FROM (p_generation,p_attempt,p_attempt,'startUnconfirmed','start')
 THEN RETURN false;END IF;
 UPDATE public.live_activity_delivery SET target_state=(target_state-'attempt_id'-'start_attempt_id'-'pending_event'-'retry_at')||
 jsonb_build_object('status','pending','absence_retry_allowed',p_absence_allowed,'absence_retry_used',p_absence_used),
 not_before=clock_timestamp(),lease_id=NULL,lease_deadline=NULL,leased_dirty_version=NULL,leased_registration_version=NULL WHERE job_id=d.job_id;
 RETURN true;
END $$;
REVOKE ALL ON FUNCTION app.abandon_live_activity_start(jsonb,text,text,boolean,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.abandon_live_activity_start(jsonb,text,text,boolean,boolean) TO baby_tracker_live_activity;
INSERT INTO public.schema_privilege_manifest VALUES(28,'baby_tracker_live_activity','function','app.abandon_live_activity_start(jsonb,text,text,boolean,boolean)','EXECUTE');

-- A family re-dirty bumps every job, including one another runner holds, and
-- that lease can then never be acknowledged (claim_live requires its dirty
-- version). Its holder releases the exact lease once its own dispatch has
-- returned, so the newer work is reclaimable at the next poll rather than at
-- lease expiry. Only the lease is cleared: target_state keeps any persisted
-- (start-unconfirmed or ambiguous) intent, completed_version and not_before are
-- untouched, and nothing is enqueued, so no newer work is consumed. A fresh
-- lease_id per claim makes this an exact claim-attempt CAS; a reclaimed or
-- replaced lease is never touched. Only the delivery row is locked.
CREATE FUNCTION app.release_stale_live_activity_lease(p_claim jsonb) RETURNS boolean
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d public.live_activity_delivery;
BEGIN
 SELECT * INTO d FROM public.live_activity_delivery WHERE job_id=(p_claim->>'job_id')::uuid FOR UPDATE;
 IF NOT FOUND OR d.lease_id IS NULL OR d.dirty_version<=d.leased_dirty_version OR
 (d.lease_id,d.registration_id,d.leased_registration_version,d.account_id,d.device_id,d.family_id,d.baby_id,d.kind,d.instance_id,d.recovery_epoch,d.leased_dirty_version,d.lease_deadline)
 IS DISTINCT FROM ((p_claim->>'lease_id')::uuid,(p_claim->>'registration_id')::uuid,(p_claim->>'registration_version')::bigint,
 (p_claim->>'account_id')::uuid,(p_claim->>'device_id')::uuid,(p_claim->>'family_id')::uuid,(p_claim->>'baby_id')::uuid,p_claim->>'kind',
 (p_claim->>'instance_id')::uuid,(p_claim->>'recovery_epoch')::bigint,(p_claim->>'dirty_version')::bigint,(p_claim->>'lease_deadline')::timestamptz)
 THEN RETURN false;END IF;
 UPDATE public.live_activity_delivery SET lease_id=NULL,lease_deadline=NULL,leased_dirty_version=NULL,leased_registration_version=NULL WHERE job_id=d.job_id;
 RETURN true;
END $$;
REVOKE ALL ON FUNCTION app.release_stale_live_activity_lease(jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.release_stale_live_activity_lease(jsonb) TO baby_tracker_live_activity;
INSERT INTO public.schema_privilege_manifest VALUES(28,'baby_tracker_live_activity','function','app.release_stale_live_activity_lease(jsonb)','EXECUTE');

ALTER TABLE public.live_activity_devices ADD COLUMN inventory_version bigint NOT NULL DEFAULT 0;
ALTER TABLE public.live_activity_devices ADD COLUMN foreground_inventory jsonb;
-- Account/family-scoped metadata only. Foreground evidence consumes an exact
-- attempt once; it does not cancel a delayed APNs submission.
CREATE FUNCTION app.live_activity_registration_state(p_registration uuid,p_inventory jsonb) RETURNS jsonb
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE device public.live_activity_devices; proof jsonb; version bigint; result jsonb;
BEGIN
 SELECT * INTO device FROM public.live_activity_devices WHERE registration_id=p_registration
 AND account_id=app.current_account_id() AND family_id=app.current_family_id() AND NOT revoked FOR UPDATE;
 IF NOT FOUND THEN RETURN NULL;END IF;
 IF p_inventory IS NOT NULL THEN
 version:=(p_inventory->>'inventory_version')::bigint;
 IF version<device.inventory_version OR (version=device.inventory_version AND p_inventory IS DISTINCT FROM device.foreground_inventory) THEN
 RAISE EXCEPTION 'live_activity_version_conflict' USING ERRCODE='P0001';END IF;
 IF version>device.inventory_version THEN
 PERFORM 1 FROM public.live_activity_family_dirty WHERE family_id=device.family_id FOR UPDATE;
 UPDATE public.live_activity_devices SET inventory_version=version,foreground_inventory=p_inventory WHERE registration_id=p_registration;
 FOR proof IN SELECT value FROM jsonb_array_elements(p_inventory->'absent_generations') LOOP
 UPDATE public.live_activity_delivery SET target_state=target_state||'{"absence_retry_allowed":true}'::jsonb,not_before=clock_timestamp()
 WHERE registration_id=p_registration AND instance_id=device.instance_id AND recovery_epoch=device.recovery_epoch
 AND target_state->>'generation_id'=proof->>'generation_id' AND target_state->>'start_attempt_id'=proof->>'observed_attempt_id'
 AND target_state->>'status'='startUnconfirmed' AND NOT coalesce((target_state->>'absence_retry_used')::boolean,false);
 END LOOP;
 END IF;
 END IF;
 SELECT coalesce(jsonb_agg(jsonb_build_object('baby_id',baby_id,'kind',kind,'state',target_state) ORDER BY baby_id,kind),'[]'::jsonb) INTO result
 FROM public.live_activity_delivery WHERE registration_id=p_registration;
 RETURN result;
END $$;
REVOKE ALL ON FUNCTION app.live_activity_registration_state(uuid,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.live_activity_registration_state(uuid,jsonb) TO baby_tracker_runtime;
INSERT INTO public.schema_privilege_manifest VALUES(28,'baby_tracker_runtime','function','app.live_activity_registration_state(uuid,jsonb)','EXECUTE');

ALTER TABLE public.live_activity_tokens ADD COLUMN token_fingerprint bytea;
CREATE FUNCTION app.live_activity_token_fingerprint() RETURNS trigger
 LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF TG_TABLE_NAME='live_activity_devices' THEN
 IF NEW.revoked THEN NEW.push_token:=NULL;END IF;RETURN NEW;END IF;
 IF NEW.token IS NOT NULL THEN NEW.token_fingerprint:=sha256(convert_to(NEW.token,'UTF8'));
 ELSIF TG_OP='UPDATE' THEN NEW.token_fingerprint:=OLD.token_fingerprint;END IF;
 IF NEW.state<>'active' THEN NEW.token:=NULL;END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION app.live_activity_token_fingerprint() FROM PUBLIC;
CREATE TRIGGER live_activity_token_fingerprint BEFORE INSERT OR UPDATE ON public.live_activity_tokens FOR EACH ROW EXECUTE FUNCTION app.live_activity_token_fingerprint();

ALTER TABLE public.live_activity_tokens ADD COLUMN server_retired boolean NOT NULL DEFAULT false;

CREATE TRIGGER live_activity_device_retire_token BEFORE UPDATE ON public.live_activity_devices FOR EACH ROW EXECUTE FUNCTION app.live_activity_token_fingerprint();
