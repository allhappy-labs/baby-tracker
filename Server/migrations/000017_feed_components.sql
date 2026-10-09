-- Ordered feed/timer components. This migration is additive and keeps the
-- schema-sixteen scalar/child projections synchronized so the predecessor
-- binary can remain live during the schema-first deployment window.

CREATE TABLE public.feed_components (
    family_id uuid NOT NULL,
    feed_id uuid NOT NULL CHECK (feed_id <> '00000000-0000-0000-0000-000000000000'),
    component_id uuid NOT NULL CHECK (component_id <> '00000000-0000-0000-0000-000000000000'),
    position integer NOT NULL CHECK (position BETWEEN 0 AND 511),
    kind text NOT NULL CHECK (kind IN ('breast','bottle','solids')),
    side text CHECK (side IN ('left','right')),
    seconds integer CHECK (seconds BETWEEN 1 AND 604800),
    volume_ml numeric(10,6) CHECK (volume_ml BETWEEN 0 AND 2000),
    contents text CHECK (contents IN ('breast_milk','formula','mixed')),
    foods text[],
    amount text CHECK (amount IN ('taste','some','lots')),
    PRIMARY KEY (family_id, feed_id, position),
    UNIQUE (family_id, feed_id, component_id),
    CONSTRAINT feed_components_feed_fk FOREIGN KEY (family_id, feed_id)
        REFERENCES public.feed_entries(family_id, id) ON DELETE CASCADE,
    CONSTRAINT feed_components_shape CHECK (
        (kind='breast' AND side IS NOT NULL AND seconds IS NOT NULL
            AND volume_ml IS NULL AND contents IS NULL AND foods IS NULL AND amount IS NULL)
        OR (kind='bottle' AND side IS NULL AND seconds IS NULL
            AND volume_ml IS NOT NULL AND contents IS NOT NULL AND foods IS NULL AND amount IS NULL)
        OR (kind='solids' AND side IS NULL AND seconds IS NULL
            AND volume_ml IS NULL AND contents IS NULL AND foods IS NOT NULL AND amount IS NOT NULL
            AND cardinality(foods) BETWEEN 0 AND 50)
    )
);

CREATE TABLE public.timer_components (
    family_id uuid NOT NULL,
    timer_id uuid NOT NULL CHECK (timer_id <> '00000000-0000-0000-0000-000000000000'),
    component_id uuid NOT NULL CHECK (component_id <> '00000000-0000-0000-0000-000000000000'),
    position integer NOT NULL CHECK (position BETWEEN 0 AND 511),
    kind text NOT NULL CHECK (kind IN ('breast','bottle','solids')),
    side text CHECK (side IN ('left','right')),
    seconds integer CHECK (seconds BETWEEN 1 AND 604800),
    volume_ml numeric(10,6) CHECK (volume_ml BETWEEN 0 AND 2000),
    contents text CHECK (contents IN ('breast_milk','formula','mixed')),
    foods text[],
    amount text CHECK (amount IN ('taste','some','lots')),
    PRIMARY KEY (family_id, timer_id, position),
    UNIQUE (family_id, timer_id, component_id),
    CONSTRAINT timer_components_timer_fk FOREIGN KEY (family_id, timer_id)
        REFERENCES public.active_timers(family_id, id) ON DELETE CASCADE,
    CONSTRAINT timer_components_shape CHECK (
        (kind='breast' AND side IS NOT NULL AND seconds IS NOT NULL
            AND volume_ml IS NULL AND contents IS NULL AND foods IS NULL AND amount IS NULL)
        OR (kind='bottle' AND side IS NULL AND seconds IS NULL
            AND volume_ml IS NOT NULL AND contents IS NOT NULL AND foods IS NULL AND amount IS NULL)
        OR (kind='solids' AND side IS NULL AND seconds IS NULL
            AND volume_ml IS NULL AND contents IS NULL AND foods IS NOT NULL AND amount IS NOT NULL
            AND cardinality(foods) BETWEEN 0 AND 50)
    )
);

ALTER TABLE public.feed_entries
    ADD COLUMN component_version smallint NOT NULL DEFAULT 1 CHECK (component_version IN (1,2));
ALTER TABLE public.active_timers
    ADD COLUMN component_version smallint NOT NULL DEFAULT 1 CHECK (component_version IN (1,2));

ALTER TABLE public.active_timers DROP CONSTRAINT active_timers_state_shape;
ALTER TABLE public.active_timers ADD CONSTRAINT active_timers_state_shape CHECK (
    (pending_volume_ml IS NULL) = (pending_contents IS NULL)
    AND ((timer_kind='feed'
          AND (current_side IS NULL)=(current_side_started_at IS NULL))
         OR (timer_kind='sleep'
             AND current_side IS NULL
             AND current_side_started_at IS NULL
             AND pending_volume_ml IS NULL
             AND pending_contents IS NULL))
    AND (completed_record_id IS NULL OR tombstoned_at IS NOT NULL)
);

ALTER TABLE public.feed_components ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.feed_components FORCE ROW LEVEL SECURITY;
ALTER TABLE public.timer_components ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.timer_components FORCE ROW LEVEL SECURITY;
CREATE POLICY family_context ON public.feed_components
    USING (family_id=app.current_family_id()) WITH CHECK (family_id=app.current_family_id());
CREATE POLICY family_context ON public.timer_components
    USING (family_id=app.current_family_id()) WITH CHECK (family_id=app.current_family_id());

CREATE FUNCTION app.sha1(p_input bytea) RETURNS bytea
LANGUAGE plpgsql IMMUTABLE STRICT
SET search_path = pg_catalog
AS $$
DECLARE
    data bytea := p_input || decode('80','hex');
    bit_length bigint := length(p_input)::bigint * 8;
    words bigint[];
    h0 bigint := 1732584193;
    h1 bigint := 4023233417;
    h2 bigint := 2562383102;
    h3 bigint := 271733878;
    h4 bigint := 3285377520;
    a bigint; b bigint; c bigint; d bigint; e bigint;
    f bigint; k bigint; temp bigint;
    block_offset integer; i integer; shift integer;
BEGIN
    WHILE length(data) % 64 <> 56 LOOP
        data := data || decode('00','hex');
    END LOOP;
    FOR shift IN REVERSE 56..0 BY 8 LOOP
        data := data || decode(lpad(to_hex((bit_length >> shift) & 255),2,'0'),'hex');
    END LOOP;
    FOR block_offset IN 0..(length(data)/64-1) LOOP
        words := array_fill(0::bigint,ARRAY[80],ARRAY[0]);
        FOR i IN 0..15 LOOP
            words[i] := (get_byte(data,block_offset*64+i*4)::bigint << 24)
                      | (get_byte(data,block_offset*64+i*4+1)::bigint << 16)
                      | (get_byte(data,block_offset*64+i*4+2)::bigint << 8)
                      | get_byte(data,block_offset*64+i*4+3)::bigint;
        END LOOP;
        FOR i IN 16..79 LOOP
            temp := words[i-3] # words[i-8] # words[i-14] # words[i-16];
            words[i] := ((temp << 1) | (temp >> 31)) & 4294967295;
        END LOOP;
        a := h0; b := h1; c := h2; d := h3; e := h4;
        FOR i IN 0..79 LOOP
            IF i < 20 THEN f := (b & c) | ((~b) & d); k := 1518500249;
            ELSIF i < 40 THEN f := b # c # d; k := 1859775393;
            ELSIF i < 60 THEN f := (b & c) | (b & d) | (c & d); k := 2400959708;
            ELSE f := b # c # d; k := 3395469782;
            END IF;
            temp := (((a << 5) | (a >> 27)) + f + e + k + words[i]) & 4294967295;
            e := d; d := c; c := ((b << 30) | (b >> 2)) & 4294967295; b := a; a := temp;
        END LOOP;
        h0 := (h0+a) & 4294967295; h1 := (h1+b) & 4294967295;
        h2 := (h2+c) & 4294967295; h3 := (h3+d) & 4294967295;
        h4 := (h4+e) & 4294967295;
    END LOOP;
    RETURN decode(lpad(to_hex(h0),8,'0')||lpad(to_hex(h1),8,'0')||
                  lpad(to_hex(h2),8,'0')||lpad(to_hex(h3),8,'0')||
                  lpad(to_hex(h4),8,'0'),'hex');
END
$$;

CREATE FUNCTION app.legacy_component_id(p_parent uuid, p_position integer) RETURNS uuid
LANGUAGE plpgsql IMMUTABLE STRICT
SET search_path = pg_catalog
AS $$
DECLARE
    value bytea;
    encoded text;
BEGIN
    value := app.sha1(uuid_send(p_parent) || convert_to('feed-component-v1:' || p_position::text, 'UTF8'));
    value := set_byte(value,6,(get_byte(value,6) & 15) | 80);
    value := set_byte(value,8,(get_byte(value,8) & 63) | 128);
    encoded := encode(substring(value FROM 1 FOR 16),'hex');
    RETURN (substring(encoded,1,8)||'-'||substring(encoded,9,4)||'-'||
            substring(encoded,13,4)||'-'||substring(encoded,17,4)||'-'||
            substring(encoded,21,12))::uuid;
END
$$;

CREATE FUNCTION app.rebuild_feed_components(p_family uuid, p_feed uuid) RETURNS void
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE side_count integer; bottle_count integer;
BEGIN
    DELETE FROM public.feed_components WHERE family_id=p_family AND feed_id=p_feed;
    INSERT INTO public.feed_components(family_id,feed_id,component_id,position,kind,side,seconds)
    SELECT i.family_id,i.feed_id,app.legacy_component_id(i.feed_id,(row_number() OVER (ORDER BY i.position)-1)::integer),
           row_number() OVER (ORDER BY i.position)-1,'breast',i.side,i.seconds
      FROM public.feed_side_intervals i
     WHERE i.family_id=p_family AND i.feed_id=p_feed AND i.seconds>0
     ORDER BY i.position;
    GET DIAGNOSTICS side_count=ROW_COUNT;
    INSERT INTO public.feed_components(family_id,feed_id,component_id,position,kind,volume_ml,contents)
    SELECT e.family_id,e.id,app.legacy_component_id(e.id,side_count),side_count,'bottle',e.volume_ml,e.contents
      FROM public.feed_entries e WHERE e.family_id=p_family AND e.id=p_feed
       AND e.volume_ml IS NOT NULL AND e.contents IS NOT NULL;
    GET DIAGNOSTICS bottle_count=ROW_COUNT;
    INSERT INTO public.feed_components(family_id,feed_id,component_id,position,kind,foods,amount)
    SELECT e.family_id,e.id,app.legacy_component_id(e.id,side_count+bottle_count),side_count+bottle_count,'solids',
           coalesce((SELECT array_agg(f.food ORDER BY f.position) FROM public.feed_foods f
                     WHERE f.family_id=e.family_id AND f.feed_id=e.id),ARRAY[]::text[]),e.amount
      FROM public.feed_entries e WHERE e.family_id=p_family AND e.id=p_feed AND e.amount IS NOT NULL;
END
$$;

CREATE FUNCTION app.rebuild_timer_components(p_family uuid, p_timer uuid) RETURNS void
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE side_count integer;
BEGIN
    DELETE FROM public.timer_components WHERE family_id=p_family AND timer_id=p_timer;
    INSERT INTO public.timer_components(family_id,timer_id,component_id,position,kind,side,seconds)
    SELECT i.family_id,i.timer_id,app.legacy_component_id(i.timer_id,(row_number() OVER (ORDER BY i.position)-1)::integer),
           row_number() OVER (ORDER BY i.position)-1,'breast',i.side,i.seconds
      FROM public.timer_side_intervals i
     WHERE i.family_id=p_family AND i.timer_id=p_timer AND i.seconds>0
     ORDER BY i.position;
    GET DIAGNOSTICS side_count=ROW_COUNT;
    INSERT INTO public.timer_components(family_id,timer_id,component_id,position,kind,volume_ml,contents)
    SELECT t.family_id,t.id,app.legacy_component_id(t.id,side_count),side_count,'bottle',t.pending_volume_ml,t.pending_contents
      FROM public.active_timers t WHERE t.family_id=p_family AND t.id=p_timer
       AND t.pending_volume_ml IS NOT NULL AND t.pending_contents IS NOT NULL;
END
$$;

CREATE FUNCTION app.project_legacy_feed_components() RETURNS trigger
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    PERFORM app.rebuild_feed_components(coalesce(NEW.family_id,OLD.family_id),coalesce(NEW.feed_id,OLD.feed_id));
    RETURN NULL;
END $$;
CREATE FUNCTION app.project_legacy_feed_parent_components() RETURNS trigger
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN PERFORM app.rebuild_feed_components(NEW.family_id,NEW.id); RETURN NEW; END $$;
CREATE FUNCTION app.project_legacy_timer_components() RETURNS trigger
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    PERFORM app.rebuild_timer_components(coalesce(NEW.family_id,OLD.family_id),coalesce(NEW.timer_id,OLD.timer_id));
    RETURN NULL;
END $$;
CREATE FUNCTION app.project_legacy_timer_parent_components() RETURNS trigger
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN PERFORM app.rebuild_timer_components(NEW.family_id,NEW.id); RETURN NEW; END $$;

CREATE TRIGGER feed_components_from_parent AFTER INSERT OR UPDATE OF volume_ml,contents,amount
    ON public.feed_entries FOR EACH ROW EXECUTE FUNCTION app.project_legacy_feed_parent_components();
CREATE TRIGGER feed_components_from_sides AFTER INSERT OR UPDATE OR DELETE
    ON public.feed_side_intervals FOR EACH ROW EXECUTE FUNCTION app.project_legacy_feed_components();
CREATE TRIGGER feed_components_from_foods AFTER INSERT OR UPDATE OR DELETE
    ON public.feed_foods FOR EACH ROW EXECUTE FUNCTION app.project_legacy_feed_components();
CREATE TRIGGER timer_components_from_parent AFTER INSERT OR UPDATE OF pending_volume_ml,pending_contents
    ON public.active_timers FOR EACH ROW EXECUTE FUNCTION app.project_legacy_timer_parent_components();
CREATE TRIGGER timer_components_from_sides AFTER INSERT OR UPDATE OR DELETE
    ON public.timer_side_intervals FOR EACH ROW EXECUTE FUNCTION app.project_legacy_timer_components();

SELECT app.rebuild_feed_components(e.family_id,e.id) FROM public.feed_entries e;
SELECT app.rebuild_timer_components(t.family_id,t.id) FROM public.active_timers t;

REVOKE ALL ON public.feed_components,public.timer_components
    FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;
GRANT SELECT,INSERT,DELETE ON public.feed_components,public.timer_components TO baby_tracker_runtime;
REVOKE ALL ON FUNCTION app.sha1(bytea),app.legacy_component_id(uuid,integer),app.rebuild_feed_components(uuid,uuid),
    app.rebuild_timer_components(uuid,uuid),app.project_legacy_feed_components(),
    app.project_legacy_feed_parent_components(),app.project_legacy_timer_components(),
    app.project_legacy_timer_parent_components()
    FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
SELECT 17,principal,object_kind,object_identity,privileges
  FROM public.schema_privilege_manifest WHERE schema_version=16;
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
    (17,'baby_tracker_runtime','table','public.feed_components','SELECT,INSERT,DELETE'),
    (17,'baby_tracker_runtime','table','public.timer_components','SELECT,INSERT,DELETE');

UPDATE public.schema_compatibility
   SET current_version=17,introduced_by='000017_feed_components.sql',recorded_at=now()
 WHERE singleton;
