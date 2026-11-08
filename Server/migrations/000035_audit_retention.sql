-- 000035_audit_retention: owner decision D1 (2026-10-16). Security audit
-- history is kept for 13 months. app.drop_expired_audit_partition drops the
-- oldest audit partition whose range ended more than 13 months ago, one per
-- call, after recording each chain's retention anchor in
-- audit_retention_anchors: the seq and hash of the chain's last dropped row.
-- The verifiers walk a chain from its anchor, so the first kept row must
-- link to it. (Routine refresh rotation writing no audit row, the other half
-- of D1, needs no schema change: session_rotated stays an audit kind for the
-- imported legacy history.)

-- One row per chain that lost rows to retention; only the definer below
-- writes it, and only forward (a higher anchor_seq). The runtime reads it in
-- its verifier transactions. dropped_before is the end of the partition the
-- anchor row was dropped with: every row at or below the anchor was stamped
-- before it, so a row at or below the anchor stamped well after it can only
-- have been inserted after the drop, and the verifier fails the chain.
CREATE TABLE public.audit_retention_anchors (
    scope app.audit_scope NOT NULL,
    scope_id uuid NOT NULL,
    anchor_seq bigint NOT NULL,
    anchor_hash bytea NOT NULL,
    anchored_at timestamp with time zone NOT NULL,
    dropped_before timestamp with time zone NOT NULL,
    CONSTRAINT audit_retention_anchors_anchor_hash_check CHECK ((octet_length(anchor_hash) = 32)),
    CONSTRAINT audit_retention_anchors_anchor_seq_check CHECK ((anchor_seq > 0))
);

ALTER TABLE ONLY public.audit_retention_anchors FORCE ROW LEVEL SECURITY;

-- The verifier reads a chain's anchor by its key, as it reads its checkpoint.
ALTER TABLE ONLY public.audit_retention_anchors
    ADD CONSTRAINT audit_retention_anchors_pkey PRIMARY KEY (scope_id, scope);

ALTER TABLE public.audit_retention_anchors ENABLE ROW LEVEL SECURITY;

CREATE POLICY authority_only ON public.audit_retention_anchors USING (app.authority_context()) WITH CHECK (app.authority_context());

GRANT SELECT ON TABLE public.audit_retention_anchors TO baby_tracker_runtime;

-- Drops the oldest audit partition whose upper bound is at or before
-- p_before (the app clock's now minus 13 months), and never one holding rows
-- from the last 13 months by the database's own clock: so never the default
-- partition (it has no bound), nor the current or the next one, whatever
-- clock the caller passes. Returns the dropped partition (NULL: none) and the
-- number of chains whose anchor it set or moved forward.
--
-- An anchor is taken only from rows that hold together: for every chain with
-- rows in the partition, the chain is not failed, it has an account or
-- family row whose head is at or past the new anchor, and its rows from its
-- previous anchor (or its start) through the new anchor, wherever they are (a
-- neighbouring partition near a month boundary, the default partition for
-- legacy rows), are exactly one per seq and link by prev_hash. Otherwise
-- nothing changes: it raises XX001 (data_corrupted) 'audit retention
-- refused', its DETAIL naming the partition, chain, seq and reason, and the
-- partition stays until an operator settles the chain. A backdated duplicate
-- of a seq, which would move an anchor past rows nobody verified, is refused
-- this way. (Row hashes are not recomputed here: canonical JSON is the
-- verifier's; the link is what the anchor rests on.)
--
-- One transaction: the partition roll's advisory lock (a roll and a drop
-- never interleave), then a SHARE lock on the partition (no audit row can
-- land in it while its chains are checked and anchored), the check, the
-- anchors, and the detach and drop, which take ACCESS EXCLUSIVE on the audit
-- table: lock_timeout 1s bounds how long audit inserts queue behind it, and
-- behind a long reader it fails with 55P03 and the next hourly run retries.
-- Each verifier transaction locks the audit table before its snapshot, so a
-- drop never commits between the anchor and the rows a walk reads.
CREATE FUNCTION app.drop_expired_audit_partition(p_before timestamp with time zone, OUT dropped text, OUT anchored integer) RETURNS record
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    SET "TimeZone" TO 'UTC'
    SET lock_timeout TO '1s'
    AS $$
DECLARE
  keep_from timestamptz;
  part text;
  part_end timestamptz;
  previous_authority text;
  bad record;
BEGIN
  anchored := 0;
  IF p_before IS NULL OR NOT isfinite(p_before) THEN
    RAISE EXCEPTION 'invalid audit retention cutoff' USING ERRCODE = '22023';
  END IF;
  keep_from := least(p_before, now() - interval '13 months');
  PERFORM pg_catalog.pg_advisory_xact_lock(3100031); -- app.ensure_audit_partitions' lock
  SELECT c.relname, b.upper_bound INTO part, part_end
    FROM pg_catalog.pg_inherits i
    JOIN pg_catalog.pg_class c ON c.oid = i.inhrelid
    CROSS JOIN LATERAL (SELECT (pg_catalog.regexp_match(pg_catalog.pg_get_expr(c.relpartbound, c.oid),
        '^FOR VALUES FROM \(''[^'']+''\) TO \(''([^'']+)''\)$'))[1]::timestamptz AS upper_bound) b
   WHERE i.inhparent = 'public.security_audit_events'::regclass AND b.upper_bound <= keep_from
   ORDER BY b.upper_bound, c.relname
   LIMIT 1;
  IF part IS NULL THEN
    RETURN;
  END IF;
  EXECUTE format('LOCK TABLE public.%I IN SHARE MODE', part);
  -- The audit rows, the chain heads and the anchors are authority rows: their
  -- policies admit this transaction only under app.authority, restored
  -- before the function returns.
  previous_authority := current_setting('app.authority', true);
  PERFORM pg_catalog.set_config('app.authority', '1', true);
  EXECUTE format($check$
    WITH last AS (
      SELECT DISTINCT ON (scope, scope_id) scope, scope_id, seq FROM public.%I ORDER BY scope, scope_id, seq DESC, at DESC
    ), chains AS (
      SELECT l.scope, l.scope_id, l.seq AS to_seq, coalesce(a.anchor_seq, 0) AS from_seq,
             coalesce(a.anchor_hash, '\x0000000000000000000000000000000000000000000000000000000000000000'::bytea) AS from_hash,
             coalesce(acc.chain_failed, fam.chain_failed) AS failed, coalesce(acc.audit_seq, fam.audit_seq) AS head
        FROM last l
        LEFT JOIN public.audit_retention_anchors a ON a.scope_id = l.scope_id AND a.scope = l.scope
        LEFT JOIN public.accounts acc ON l.scope = 'account' AND acc.id = l.scope_id
        LEFT JOIN public.families fam ON l.scope = 'family' AND fam.id = l.scope_id
    ), walk AS (
      SELECT c.scope, c.scope_id, c.from_seq, c.to_seq, c.from_hash, e.seq, e.prev_hash,
             lag(e.seq) OVER w AS before_seq, lag(e.hash) OVER w AS before_hash, count(*) OVER (PARTITION BY c.scope, c.scope_id) AS n
        FROM chains c
        JOIN public.security_audit_events e ON e.scope_id = c.scope_id AND e.scope = c.scope AND e.seq > c.from_seq AND e.seq <= c.to_seq
       WHERE c.to_seq > c.from_seq
      WINDOW w AS (PARTITION BY c.scope, c.scope_id ORDER BY e.seq, e.at)
    ), broken AS (
      SELECT scope, scope_id, to_seq AS seq, CASE WHEN failed IS NULL THEN 'no account or family row' ELSE 'chain_failed' END AS reason, 0 AS rank
        FROM chains WHERE failed IS DISTINCT FROM false
      UNION ALL
      SELECT scope, scope_id, to_seq, 'head behind the anchor', 0 FROM chains WHERE head < to_seq
      UNION ALL
      SELECT scope, scope_id, seq, CASE WHEN seq = before_seq THEN 'duplicate seq' WHEN seq <> coalesce(before_seq, from_seq) + 1 THEN 'missing row'
                                        ELSE 'prev_hash does not link' END, 0
        FROM walk WHERE seq <> coalesce(before_seq, from_seq) + 1 OR prev_hash <> coalesce(before_hash, from_hash)
      UNION ALL
      SELECT DISTINCT scope, scope_id, to_seq, 'duplicate or missing seq', 1 FROM walk WHERE n <> to_seq - from_seq
    )
    SELECT scope::text AS scope, scope_id::text AS scope_id, seq, reason FROM broken ORDER BY seq, rank, scope, scope_id, reason LIMIT 1
  $check$, part) INTO bad;
  IF bad.scope IS NOT NULL THEN
    RAISE EXCEPTION 'audit retention refused' USING ERRCODE = 'XX001',
      DETAIL = format('partition=%s scope=%s id=%s seq=%s reason=%s', part, bad.scope, bad.scope_id, bad.seq, bad.reason);
  END IF;
  EXECUTE format('INSERT INTO public.audit_retention_anchors AS a (scope, scope_id, anchor_seq, anchor_hash, anchored_at, dropped_before)
      SELECT DISTINCT ON (scope, scope_id) scope, scope_id, seq, hash, now(), %L::timestamptz FROM public.%I ORDER BY scope, scope_id, seq DESC, at DESC
      ON CONFLICT (scope_id, scope) DO UPDATE SET anchor_seq = excluded.anchor_seq, anchor_hash = excluded.anchor_hash, anchored_at = excluded.anchored_at,
        dropped_before = excluded.dropped_before
      WHERE a.anchor_seq < excluded.anchor_seq', part_end, part);
  GET DIAGNOSTICS anchored = ROW_COUNT;
  PERFORM pg_catalog.set_config('app.authority', coalesce(previous_authority, ''), true);
  EXECUTE format('ALTER TABLE public.security_audit_events DETACH PARTITION public.%I', part);
  EXECUTE format('DROP TABLE public.%I', part);
  dropped := part;
END $$;

REVOKE ALL ON FUNCTION app.drop_expired_audit_partition(p_before timestamp with time zone, OUT dropped text, OUT anchored integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.drop_expired_audit_partition(p_before timestamp with time zone, OUT dropped text, OUT anchored integer) TO baby_tracker_maintenance;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 35,principal,object_kind,object_identity,privileges
 FROM public.schema_privilege_manifest WHERE schema_version=34;
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
 (35,'baby_tracker_runtime','table','public.audit_retention_anchors','SELECT'),
 (35,'baby_tracker_maintenance','function','app.drop_expired_audit_partition(timestamp with time zone)','EXECUTE');
UPDATE public.schema_compatibility SET current_version=35,
 introduced_by='000035_audit_retention.sql',recorded_at=now() WHERE singleton;
