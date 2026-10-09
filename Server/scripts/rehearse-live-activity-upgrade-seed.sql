-- Synthetic seed for rehearse-live-activity-upgrade.sh --synthetic, applied
-- as postgres to the schema-27 database the old tree's API seeded (four
-- accounts and devices, one family, one baby, a completed feed from a timer,
-- imported feeds). It adds the shapes the API client does not create, in the
-- rows the format-cleanup tests write for schema 27: a feeding session of two
-- linked feeds with multi-component content, a bottle feed, a solids feed, a
-- sleep session of two linked sleeps, a deleted (tombstoned) feed, a diaper
-- and a running sleep timer.
-- Nothing here is real care data.
DO $$
DECLARE
    fam uuid;
    baby uuid;
    acct uuid;
    dev uuid;
    seq bigint;
    client_time timestamptz := '2026-09-21T07:00:00Z';
    accepted timestamptz := '2026-09-21T07:00:01Z';
    feed_a uuid := 'ad000000-0000-4000-8000-000000000f01';
    feed_b uuid := 'ad000000-0000-4000-8000-000000000f02';
    feed_bottle uuid := 'ad000000-0000-4000-8000-000000000f03';
    feed_solids uuid := 'ad000000-0000-4000-8000-000000000f04';
    sleep_a uuid := 'ad000000-0000-4000-8000-000000000a01';
    sleep_b uuid := 'ad000000-0000-4000-8000-000000000a02';
    diaper uuid := 'ad000000-0000-4000-8000-000000000d01';
    timer uuid := 'ad000000-0000-4000-8000-000000000701';
    feed_gone uuid := 'ad000000-0000-4000-8000-000000000f05';
BEGIN
    SELECT id INTO STRICT fam FROM families;
    SELECT id INTO STRICT baby FROM babies WHERE family_id = fam AND tombstoned_at IS NULL ORDER BY sort_order, id LIMIT 1;
    SELECT account_id INTO STRICT acct FROM memberships WHERE family_id = fam AND role = 'owner' AND status = 'active';
    SELECT next_sequence INTO STRICT seq FROM families WHERE id = fam;
    SELECT id INTO STRICT dev FROM devices WHERE account_id = acct ORDER BY created_at, id LIMIT 1;
    PERFORM set_config('app.account_id', acct::text, true), set_config('app.family_id', fam::text, true);

    -- Every canonical record references its journal head (family, entity,
    -- version, sequence), so the journal rows come first, as the server's
    -- own writes do.
    INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,actor_account_id,device_id,operation,canonical_entity,accepted_at) VALUES
        (fam, seq + 1, 'feed', feed_a, 1, acct, dev, 'create', jsonb_build_object('entity_kind','feed','id',feed_a,'entity_version',1,'baby_id',baby,'kind','breast','started_at','2026-09-21T06:00:00Z','ended_at','2026-09-21T06:20:00Z','components',2), accepted),
        (fam, seq + 2, 'feed', feed_b, 1, acct, dev, 'create', jsonb_build_object('entity_kind','feed','id',feed_b,'entity_version',1,'baby_id',baby,'kind','breast','started_at','2026-09-21T06:25:00Z','ended_at','2026-09-21T06:35:00Z','feeding_session_id',feed_a,'components',1), accepted),
        (fam, seq + 3, 'feed', feed_bottle, 2, acct, dev, 'update', jsonb_build_object('entity_kind','feed','id',feed_bottle,'entity_version',2,'baby_id',baby,'kind','bottle','started_at','2026-09-21T09:00:00Z','ended_at','2026-09-21T09:10:00Z','components',1), accepted),
        (fam, seq + 4, 'feed', feed_solids, 1, acct, dev, 'create', jsonb_build_object('entity_kind','feed','id',feed_solids,'entity_version',1,'baby_id',baby,'kind','solids','started_at','2026-09-21T12:00:00Z','components',1), accepted),
        (fam, seq + 5, 'sleep', sleep_a, 2, acct, dev, 'update', jsonb_build_object('entity_kind','sleep','id',sleep_a,'entity_version',2,'baby_id',baby,'started_at','2026-09-21T13:00:00Z','ended_at','2026-09-21T14:00:00Z'), accepted),
        (fam, seq + 6, 'sleep', sleep_b, 1, acct, dev, 'create', jsonb_build_object('entity_kind','sleep','id',sleep_b,'entity_version',1,'baby_id',baby,'started_at','2026-09-21T14:10:00Z','ended_at','2026-09-21T15:00:00Z','sleep_session_id',sleep_a), accepted),
        (fam, seq + 7, 'diaper', diaper, 1, acct, dev, 'create', jsonb_build_object('entity_kind','diaper','id',diaper,'entity_version',1,'baby_id',baby,'started_at','2026-09-21T08:00:00Z','contents','dirty'), accepted),
        (fam, seq + 8, 'timer', timer, 1, acct, dev, 'timer_start', jsonb_build_object('entity_kind','timer','id',timer,'entity_version',1,'baby_id',baby,'timer_kind','sleep','started_at','2026-09-21T20:00:00Z'), accepted),
        (fam, seq + 9, 'feed', feed_gone, 2, acct, dev, 'delete', jsonb_build_object('entity_kind','feed','id',feed_gone,'entity_version',2,'baby_id',baby,'kind','breast','deleted',true), accepted);

    -- A feeding session: a breast feed with both sides, then a linked top-up.
    INSERT INTO feed_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,kind,started_at,ended_at,time_zone_id,component_version,feeding_session_id)
        VALUES (fam, feed_a, baby, 1, acct, acct, client_time, accepted, NULL, seq + 1, 200, 'breast', '2026-09-21T06:00:00Z', '2026-09-21T06:20:00Z', 'Europe/Zurich', 4, NULL);
    DELETE FROM feed_components WHERE family_id = fam AND feed_id = feed_a;
    INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,side,seconds,started_at,ended_at) VALUES
        (fam, feed_a, 'ad000000-0000-4000-8000-000000000c01', 0, 'breast', 'left', 600, '2026-09-21T06:00:00Z', '2026-09-21T06:10:00Z'),
        (fam, feed_a, 'ad000000-0000-4000-8000-000000000c02', 1, 'breast', 'right', 600, '2026-09-21T06:10:00Z', '2026-09-21T06:20:00Z');
    INSERT INTO feed_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,kind,started_at,ended_at,time_zone_id,component_version,feeding_session_id)
        VALUES (fam, feed_b, baby, 1, acct, acct, client_time, accepted, NULL, seq + 2, 200, 'breast', '2026-09-21T06:25:00Z', '2026-09-21T06:35:00Z', 'Europe/Zurich', 4, feed_a);
    DELETE FROM feed_components WHERE family_id = fam AND feed_id = feed_b;
    INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,side,seconds,started_at,ended_at) VALUES
        (fam, feed_b, 'ad000000-0000-4000-8000-000000000c03', 0, 'breast', 'left', 600, '2026-09-21T06:25:00Z', '2026-09-21T06:35:00Z');

    -- A bottle and a solids feed.
    INSERT INTO feed_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,kind,started_at,ended_at,time_zone_id,component_version,feeding_session_id)
        VALUES (fam, feed_bottle, baby, 2, acct, acct, client_time, accepted, NULL, seq + 3, 200, 'bottle', '2026-09-21T09:00:00Z', '2026-09-21T09:10:00Z', 'Europe/Zurich', 4, NULL);
    DELETE FROM feed_components WHERE family_id = fam AND feed_id = feed_bottle;
    INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,volume_ml,contents) VALUES
        (fam, feed_bottle, 'ad000000-0000-4000-8000-000000000c04', 0, 'bottle', 90, 'formula');
    INSERT INTO feed_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,kind,started_at,ended_at,time_zone_id,component_version,feeding_session_id)
        VALUES (fam, feed_solids, baby, 1, acct, acct, client_time, accepted, NULL, seq + 4, 200, 'solids', '2026-09-21T12:00:00Z', NULL, 'Europe/Zurich', 4, NULL);
    DELETE FROM feed_components WHERE family_id = fam AND feed_id = feed_solids;
    INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,foods,amount) VALUES
        (fam, feed_solids, 'ad000000-0000-4000-8000-000000000c05', 0, 'solids', ARRAY['banana'], 'some');

    -- A deleted feed: a tombstone, its components gone with the delete.
    INSERT INTO feed_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,kind,started_at,ended_at,time_zone_id,component_version,feeding_session_id)
        VALUES (fam, feed_gone, baby, 2, acct, acct, client_time, accepted, accepted, seq + 9, 200, 'breast', '2026-09-20T06:00:00Z', '2026-09-20T06:10:00Z', 'Europe/Zurich', 4, NULL);
    DELETE FROM feed_components WHERE family_id = fam AND feed_id = feed_gone;

    -- A sleep session of two linked sleeps.
    INSERT INTO sleep_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,started_at,ended_at,time_zone_id,payload_version,sleep_session_id)
        VALUES (fam, sleep_a, baby, 2, acct, acct, client_time, accepted, NULL, seq + 5, 150, '2026-09-21T13:00:00Z', '2026-09-21T14:00:00Z', 'Europe/Zurich', 2, NULL);
    INSERT INTO sleep_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,started_at,ended_at,time_zone_id,payload_version,sleep_session_id)
        VALUES (fam, sleep_b, baby, 1, acct, acct, client_time, accepted, NULL, seq + 6, 150, '2026-09-21T14:10:00Z', '2026-09-21T15:00:00Z', 'Europe/Zurich', 2, sleep_a);

    -- A diaper and a running sleep timer.
    INSERT INTO diaper_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,started_at,time_zone_id,contents)
        VALUES (fam, diaper, baby, 1, acct, acct, client_time, accepted, NULL, seq + 7, 150, '2026-09-21T08:00:00Z', 'Europe/Zurich', 'dirty');
    INSERT INTO active_timers(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,last_sequence,storage_bytes,timer_kind,started_at,time_zone_id,component_version)
        VALUES (fam, timer, baby, 1, acct, acct, client_time, accepted, seq + 8, 100, 'sleep', '2026-09-21T20:00:00Z', 'Europe/Zurich', 5);

    UPDATE families SET next_sequence = seq + 9 WHERE id = fam;
END
$$;
