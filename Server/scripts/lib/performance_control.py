"""Private bounded control/reporting for the owned synthetic performance runner."""
import json
import math
import datetime
import re
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tempfile
import time

LIMIT = 2_000_000
METRICS = ["pool_acquire_wait_seconds", "transaction_fence_wait_seconds", "family_fence_wait_seconds", "security_fence_wait_seconds", "db_connections", "cpu_seconds", "allocations_bytes", "rss_bytes", "network_bytes", "filesystem_bytes", "integrity_pass_seconds"]

class CommandFailure(Exception):
    def __init__(self, code): self.code = code

def command(args, timeout, *, cwd=None, new_session=True):
    """Owned process group, bounded output file, bounded wait and exact reap."""
    if timeout <= 0: raise CommandFailure("cleanup_budget_exhausted")
    with tempfile.TemporaryFile() as output:
        child = subprocess.Popen(args, cwd=cwd, stdout=output, stderr=subprocess.DEVNULL, start_new_session=new_session)
        deadline = time.monotonic() + timeout
        try:
            code = None
            while child.poll() is None:
                if time.monotonic() >= deadline or os.fstat(output.fileno()).st_size > LIMIT:
                    code = "command_timeout" if time.monotonic() >= deadline else "output_bound"
                    raise CommandFailure(code)
                time.sleep(.01)
            if child.wait(): raise CommandFailure("command_failed")
            output.seek(0); data = output.read(LIMIT + 1)
            if len(data) > LIMIT: raise CommandFailure("output_bound")
            return data.decode(errors="replace")
        finally:
            if child.poll() is None:
                try:
                    if new_session: os.killpg(child.pid, signal.SIGKILL)
                    else: child.kill()
                except ProcessLookupError: pass
            child.wait()

def atomic(path, value):
    path = Path(path)
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("w") as f: json.dump(value, f, sort_keys=True)
    os.chmod(temporary, 0o600)
    os.replace(temporary, path)

def finite(value):
    return type(value) in (int, float) and math.isfinite(value) and value >= 0

def validate_row(row):
    if not isinstance(row, dict) or type(row.get("cell")) is not int or not 1 <= row["cell"] <= 27: raise ValueError("cell")
    allowed={"cell","families","actual_families","concurrency","requested_events","actual_events","contention","completion","failure_stage","failure_code","actual_families_semantics","engine_failure_state","recovery_phase","recovery_reason","recovery_retries_observed","errors","rate_limits","timeouts","retries","attempted","successful","request_seconds","pairs","elapsed_seconds","measurement_seconds","planned_event_upper_bound","live_status_counts","actual_nonempty_pulls","not_caught_up_reads","request_categories_wire_equivalent","same_family_serialized","peak_pending_complete_operations","peak_active_requests","peak_active_writes"}
    if set(row)-allowed: raise ValueError("unexpected_artifact_field")
    ordinal=row["cell"]-1
    if (row.get("requested_events"),row.get("families"),row.get("concurrency"))!=([248,512,2048][ordinal//9],[1,32,128][ordinal%9//3],[1,16,64][ordinal%3]): raise ValueError("cell_geometry")
    for field in ["families", "concurrency", "requested_events", "elapsed_seconds", "measurement_seconds"]:
        if not finite(row.get(field)): raise ValueError("required_numeric")
    for field in ["pairs", "request_seconds"]:
        if not isinstance(row.get(field), list) or len(row[field]) > 10000: raise ValueError("sample_bound")
    seen = set()
    for pair in row["pairs"]:
        if not isinstance(pair, dict) or set(pair) - {"operation", "durable_seconds", "observable_seconds", "complete"}: raise ValueError("pair_fields")
        op = pair.get("operation")
        if type(op) is not int or not 1 <= op <= 100000 or op in seen or pair.get("complete", True) is not True: raise ValueError("pair_completion")
        if not finite(pair.get("durable_seconds")) or not finite(pair.get("observable_seconds")) or pair["observable_seconds"] < pair["durable_seconds"]: raise ValueError("pair_duration")
        seen.add(op)
    if any(not finite(v) for v in row["request_seconds"]): raise ValueError("request_duration")
    if row["pairs"] and row["measurement_seconds"] <= 0: raise ValueError("missing_measurement_interval")
    return row

def read_json(path):
    path = Path(path)
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_size > LIMIT: raise ValueError("file_bound")
    return json.loads(path.read_text())

def distribution(values):
    if not values: return None
    ordered = sorted(values)
    return dict(count=len(ordered), **{"p%d_seconds" % p: ordered[math.ceil(len(ordered)*p/100)-1] for p in [50,95,99]})

def metric_window(directory, record):
    resources={k:None for k in METRICS}
    unavailable={k:"no_samples_in_cell_window" for k in METRICS}
    unavailable["rss_bytes"]="getrusage_exposes_peak_rss_only"
    start=record.get("wall_started"); end=record.get("wall_ended")
    if start is None or end is None: return dict(resources=resources,unavailable=unavailable)
    directory=Path(directory)
    invalid_resource_timestamp = False
    def rows(path, resource_samples=False):
        nonlocal invalid_resource_timestamp
        if not path.exists() or path.stat().st_size>LIMIT: return []
        result=[]
        for line in path.read_text().splitlines():
            try:
                row=json.loads(line); timestamp=row.get("time")
                if isinstance(timestamp,str): timestamp=datetime.datetime.fromisoformat(timestamp.replace("Z","+00:00")).timestamp()
                if resource_samples and not finite(timestamp): invalid_resource_timestamp = True
                if finite(timestamp) and start<=timestamp<=end: result.append(row)
            except (ValueError,TypeError,AttributeError):
                if resource_samples: invalid_resource_timestamp = True
        return result
    samples=rows(directory/"resource-samples.jsonl", resource_samples=True)
    samples=[r for r in samples if r.get("sample_finished",end)<=end]
    if samples:
        for name in ["db_connections","container_cpu_percent","container_memory_bytes"]:
            values=[r["resources"][name] for r in samples if finite(r.get("resources",{}).get(name))]
            if values: resources[name]=max(values)
        for output,counter in [("network_bytes","network_counter_bytes"),("filesystem_bytes","filesystem_counter_bytes")]:
            values=[r["resources"][counter] for r in samples if finite(r.get("resources",{}).get(counter))]
            timestamps=[r.get("time") for r in samples]
            ordered=all(finite(t) for t in timestamps) and all(b>=a for a,b in zip(timestamps,timestamps[1:]))
            monotonic=all(b>=a for a,b in zip(values,values[1:]))
            if len(values)>=2 and ordered and monotonic: resources[output]=values[-1]-values[0]
            else: unavailable[output]="insufficient_counter_samples" if len(values)<2 else "counter_reset_or_out_of_order_samples"
        resources["docker_sample_count"]=len(samples)
    if invalid_resource_timestamp:
        # An unattributable sample can belong to any cell window.
        for output in ["network_bytes", "filesystem_bytes"]:
            resources[output] = None
            unavailable[output] = "invalid_resource_timestamp_unattributable_window"
    phases=rows(directory.parent/"server-phases.jsonl")
    mapping={"security_fence":"security_fence_wait_seconds","family_fence":"family_fence_wait_seconds","transaction_acquire":"transaction_acquire_seconds","authority":"authority_phase_seconds","database":"database_phase_seconds","observer":"observer_phase_seconds"}
    for phase,name in mapping.items():
        values=[r["elapsed_ns"] for r in phases if r.get("phase")==phase and finite(r.get("elapsed_ns"))]
        if values: resources[name]=sum(values)/1e9
    values=[r["elapsed_ns"] for r in phases if r.get("phase") in {"integrity_inventory","integrity_extension","integrity_verify"} and finite(r.get("elapsed_ns"))]
    if values: resources["integrity_pass_seconds"]=sum(values)/1e9
    unavailable["transaction_fence_wait_seconds"]="BeginTx_includes_pool_checkout_and_protocol_use_transaction_acquire_seconds"
    counters=rows(directory.parent/"server-resources.jsonl")
    if counters:
        values=[r["rss_peak_bytes"] for r in counters if finite(r.get("rss_peak_bytes"))]
        if values: resources["rss_peak_bytes"]=max(values)
        for target,key,scale in [("allocations_bytes","alloc_total_bytes",1),("cpu_seconds","cpu_total_seconds",1),("pool_acquire_wait_seconds","runtime_pool_wait_ns",1e9),("runtime_pool_acquire_seconds","runtime_pool_acquire_ns",1e9)]:
            values=[r[key] for r in counters if finite(r.get(key))]
            if len(values)>=2 and all(b>=a for a,b in zip(values,values[1:])): resources[target]=(values[-1]-values[0])/scale
            elif len(values)>=2: unavailable[target]="counter_reset_or_out_of_order_samples"
        resources["process_counter_sample_count"]=len(counters)
    return dict(resources=resources,unavailable={k:v for k,v in unavailable.items() if resources.get(k) is None},semantics="cell_window_samples; runtime_pool_only; counters_first_to_last_sample; Docker_memory_not_RSS; RSS_peak_not_current; timings_aggregate_not_exclusive")

def aggregate(log, destination, phase, build, evidence=None, checkpoints=None):
    seen = {}; invalid = set(); timing = {}; global_artifact_failure = False
    for line in Path(log).read_text().splitlines():
        if not line.startswith("capacity-matrix cell: "): continue
        candidate = None
        try:
            payload = json.loads(line.split(": ", 1)[1])
            if isinstance(payload, dict) and type(payload.get("cell")) is int and 1 <= payload["cell"] <= 27:
                candidate = payload["cell"]
            if candidate is None: raise ValueError("unattributable_cell")
            row = validate_row(payload)
            if candidate in seen: invalid.add(candidate)
            else: seen[candidate] = row
        except (ValueError, TypeError, KeyError):
            if candidate is None: global_artifact_failure = True
            else: invalid.add(candidate)
    if evidence and Path(evidence).exists():
        for line in Path(evidence).read_text().splitlines():
            record = json.loads(line)
            if type(record.get("cell")) is int and 1 <= record["cell"] <= 27: timing[record["cell"]] = record
    result = []; ordinal = 0
    for events in [248,512,2048]:
        for families in [1,32,128]:
            for concurrency in [1,16,64]:
                ordinal += 1
                row = seen.get(ordinal)
                source = "final_row" if row else "none"
                if row is None and checkpoints:
                    try: row = validate_row(read_json(Path(checkpoints)/("checkpoint-%d.json" % ordinal))); source = "checkpoint"
                    except (OSError, ValueError, TypeError, KeyError): pass
                if row is None:
                    row = dict(cell=ordinal,families=families,concurrency=concurrency,requested_events=events,actual_events=None,completion="unfinished_"+phase,pairs=[],request_seconds=[],elapsed_seconds=None,measurement_seconds=None)
                # Early Swift setup checkpoints predate any successful head
                # query. Their initialized zero is not an observed authority.
                if source == "checkpoint" and row.get("actual_events") == 0:
                    row["actual_events"] = None
                row["evidence_source"] = source
                record = timing.get(ordinal)
                row["measurement_interval_complete"] = source == "final_row"
                if record:
                    row["supervisor_elapsed_seconds"] = record.get("elapsed_seconds")
                    if record.get("outcome")=="started":
                        row["elapsed_lower_bound_seconds"]=row.get("elapsed_seconds")
                        row["elapsed_seconds"]=None
                    else: row["elapsed_seconds"] = record.get("elapsed_seconds", row["elapsed_seconds"])
                    if record.get("outcome") != "finished":
                        row["completion"] = "cell_forced_timeout" if record.get("outcome") == "timeout" else "cell_forced_stop"
                        row["measurement_interval_complete"] = False
                        row["unobserved_tail"] = True
                if global_artifact_failure or ordinal in invalid:
                    row["completion"] = "global_artifact_failure" if global_artifact_failure else "invalid_runtime_artifact"
                    row["pairs"] = []; row["request_seconds"] = []; row["measurement_interval_complete"] = False
                row["durable_peer"] = distribution([p["durable_seconds"] for p in row["pairs"]])
                row["observable_peer"] = distribution([p["observable_seconds"] for p in row["pairs"]])
                row["request"] = distribution(row["request_seconds"])
                interval = row.get("measurement_seconds")
                row["successful_qps"] = len(row["pairs"])/interval if row["measurement_interval_complete"] and interval and interval > 0 else None
                row.update({k:build.get(k) for k in ["source","image","build","environment"]})
                measured=metric_window(checkpoints,record or {}) if checkpoints else dict(resources={},unavailable={})
                resource = {k:None for k in METRICS}; resource.update(measured.get("resources",{}))
                reasons = {k:"no_completed_cell_sample" for k in METRICS}; reasons.update(measured.get("unavailable",{}))
                row["resource_semantics"]=measured.get("semantics","unobserved")
                if checkpoints:
                    try:
                        sample = read_json(Path(checkpoints)/("metrics-%d.json" % ordinal))
                        resource.update(sample.get("resources", {})); reasons.update(sample.get("unavailable", {}))
                    except (OSError, ValueError): pass
                row["resources"] = resource
                row["resource_unavailable_reasons"] = {k:v for k,v in reasons.items() if resource.get(k) is None}
                result.append(row)
    atomic(destination, result)
    return result

def supervise(cwd, args):
    child = subprocess.Popen(args, cwd=cwd, start_new_session=True)
    stopped = False; stage = "process_start"; wall_started = None; active = None; last = 0; started = None; deadline = None; partial = b""
    seconds = 1 if os.environ.get("BT_SYNC_PERF_TEST_FAULT") == "matrix_cell_timeout" else 118
    control_path = os.environ.get("BABY_TRACKER_MATRIX_CONTROL")
    control = open(control_path, "rb") if control_path else None
    ack = os.environ.get("BABY_TRACKER_MATRIX_ACK")
    evidence = os.environ.get("BABY_TRACKER_MATRIX_EVIDENCE")
    def record(outcome):
        if evidence and active is not None:
            with open(evidence, "a") as f: f.write(json.dumps(dict(cell=active,outcome=outcome,elapsed_seconds=time.monotonic()-started,wall_started=wall_started,wall_ended=time.time()))+"\n")
            os.chmod(evidence,0o600)
    def stop(sig, frame):
        nonlocal stopped
        if stopped: return
        stopped = True
        record("timeout" if stage == "cell_timeout" else "interrupted")
        try: os.killpg(child.pid, signal.SIGKILL)
        except ProcessLookupError: pass
    signal.signal(signal.SIGTERM,stop); signal.signal(signal.SIGINT,stop)
    while True:
        if control:
            try:
                partial += control.read(4096)
                if len(partial)>8192: raise ValueError()
                while b"\n" in partial:
                    line,partial=partial.split(b"\n",1)
                    if len(line)>1024: raise ValueError()
                    row=json.loads(line)
                    if set(row)!={"kind","cell"} or type(row["cell"]) is not int: raise ValueError()
                    if row["kind"]=="start":
                        if active is not None or row["cell"]!=last+1 or row["cell"]>27: raise ValueError()
                        active=row["cell"]; wall_started=time.time(); started=time.monotonic(); deadline=started+seconds; stage="cell"
                        record("started"); atomic(ack,dict(kind="start_ack",cell=active))
                    elif row["kind"]=="finish":
                        if active is None or row["cell"]!=active: raise ValueError()
                        record("finished"); last=active; active=None; deadline=None
                    else: raise ValueError()
                if len(partial)>1024: raise ValueError()
                if deadline is not None and time.monotonic()>=deadline: stage="cell_timeout"; stop(signal.SIGTERM,None)
            except (ValueError,KeyError,TypeError,OSError): stage="cell_protocol"; stop(signal.SIGTERM,None)
        if child.poll() is not None: break
        time.sleep(.02)
    status=child.wait()
    if control and (active is not None or partial) and not stopped: record("missing_finish"); stage="cell_protocol"; status=1
    if control: control.close()
    print("capacity-matrix supervisor: "+json.dumps(dict(stage=stage,status=124 if stopped else status)),flush=True)
    return 124 if stopped else status


def preserve_evidence(source, destination):
    source=Path(source); destination=Path(destination); destination.mkdir(mode=0o700,exist_ok=True)
    for ordinal in range(1,28):
        try:
            row=validate_row(read_json(source/("checkpoint-%d.json"%ordinal)))
            atomic(destination/("checkpoint-%d.json"%ordinal),row)
        except (OSError,ValueError,KeyError,TypeError): pass
    for name in ["control-evidence.jsonl","resource-samples.jsonl"]:
        path=source/name
        if path.exists() and path.stat().st_size <= LIMIT:
            (destination/name).write_bytes(path.read_bytes()); os.chmod(destination/name,0o600)

def quantity(text):
    match=re.fullmatch(r"([0-9.]+)(B|kB|KB|MB|GB|KiB|MiB|GiB)",text.strip())
    if not match: raise ValueError("unit")
    scale={"B":1,"kB":1000,"KB":1000,"MB":10**6,"GB":10**9,"KiB":1024,"MiB":1024**2,"GiB":1024**3}[match[2]]
    return float(match[1])*scale

def sample_loop(app, db, destination):
    def stop(sig,frame): raise SystemExit(0)
    signal.signal(signal.SIGTERM,stop); signal.signal(signal.SIGINT,stop)
    for _ in range(1200):
        row=dict(time=time.time(),resources={},unavailable={})
        try:
            value=json.loads(command(["docker","stats","--no-stream","--format","{{json .}}",app],2))
            row["resources"].update(container_cpu_percent=float(value["CPUPerc"].rstrip("%")),container_memory_bytes=quantity(value["MemUsage"].split("/")[0]),network_counter_bytes=sum(map(quantity,value["NetIO"].split("/"))),filesystem_counter_bytes=sum(map(quantity,value["BlockIO"].split("/"))))
        except (CommandFailure,OSError,ValueError,KeyError): row["unavailable"]["docker_sample"]="sample_failed_or_timed_out"
        try: row["resources"]["db_connections"]=int(command(["docker","exec",db,"psql","-XAt","-U","postgres","-d","babytracker","-c","SELECT count(*) FROM pg_stat_activity"],1))
        except (CommandFailure,OSError,ValueError): row["unavailable"]["db_connections"]="sample_failed_or_timed_out"
        row["sample_finished"]=time.time()
        with open(destination,"a") as f: f.write(json.dumps(row)+"\n")
        os.chmod(destination,0o600)
        if Path(destination).stat().st_size>LIMIT: return
        time.sleep(1)

def cleanup_worker(config, output, allowance):
    import shutil
    config = read_json(config); output = Path(output)
    os.environ.pop("COMPOSE_ENV_FILES",None); os.environ.pop("COMPOSE_DISABLE_ENV_FILE",None)
    deadline = time.monotonic() + allowance
    project = config["project"]; secrets = Path(config["secrets"])
    if not project.startswith("baby-tracker-protocol-") or not secrets.name.startswith("baby-tracker-protocol.") or secrets.is_symlink(): raise ValueError("ownership")
    prefix = ["docker","compose","--env-file",str(secrets/"compose.env"),"--project-name",project,"-f",str(Path(config["source"])/"Server/compose.yaml"),"-f",config["override"]]
    def run(args, maximum=10): return command(args,min(maximum,deadline-time.monotonic()),new_session=False)
    state = dict(containers=None,volumes=None,secrets_removed=False,cleanup_complete=False,failure_code="incomplete_cleanup")
    atomic(output/"cleanup.json",state)
    try:
        try:
            logs=run(prefix+["logs","--no-color","--tail","10000","app"],2)
            with (output/"server-phases.jsonl").open("w") as f:
                for line in logs.splitlines():
                    try: row=json.loads(line[line.index("{"):])
                    except (ValueError,TypeError): continue
                    if row.get("msg")=="sync diagnostic" and row.get("phase") in {"authority","database","observer","security_fence","family_fence","transaction_acquire","integrity_inventory","integrity_extension","integrity_verify"} and type(row.get("elapsed_ns")) is int:
                        f.write(json.dumps(dict(phase=row["phase"],elapsed_ns=row["elapsed_ns"],time=row.get("time")))+"\n")
        except CommandFailure: pass
        # Process/pool counters are emitted only by the diagnostic opt-in.
        with (output/"server-resources.jsonl").open("w") as f:
            for line in locals().get("logs", "").splitlines():
                try: row=json.loads(line[line.index("{"):])
                except (ValueError,TypeError): continue
                if row.get("msg")=="sync resource diagnostic":
                    allowed={k:row[k] for k in ["time","alloc_total_bytes","cpu_total_seconds","rss_peak_bytes","runtime_pool_acquire_ns","runtime_pool_acquire_count","runtime_pool_wait_ns","runtime_pool_total_connections"] if k in row}
                    f.write(json.dumps(allowed)+"\n")
        run(prefix+["down","--volumes","--remove-orphans"],30)
        containers=run(["docker","ps","-aq","--filter","label=com.docker.compose.project="+project],2).split()
        volumes=run(["docker","volume","ls","-q","--filter","label=com.docker.compose.project="+project],2).split()
        state.update(containers=len(containers),volumes=len(volumes))
        if containers or volumes: raise CommandFailure("owned_resources_remaining")
        # A failed image removal preserves the recovery manifest and secrets.
        run(["docker","image","rm",config["image"]],5)
        defaults=secrets/"matrix/defaults.jsonl"
        if defaults.exists():
            for suite in defaults.read_text().splitlines():
                if not suite.startswith(("two-caregiver-e2e-owner-","two-caregiver-e2e-peer-")): raise CommandFailure("defaults_ownership")
                try: run(["defaults","delete",suite],1)
                except CommandFailure as error:
                    if error.code!="command_failed": raise
        shutil.rmtree(secrets)
        state.update(secrets_removed=True,cleanup_complete=True,failure_code=None)
        atomic(output/"cleanup.json",state)
        (output/"cleanup-recovery.json").unlink(missing_ok=True)
        return 0
    except (CommandFailure,OSError,ValueError) as error:
        state["failure_code"]=error.code if isinstance(error,CommandFailure) else "cleanup_io_failure"
        atomic(output/"cleanup.json",state)
        return 1

def bounded_cleanup(config, output, remaining):
    output=Path(output)
    # Preserve the exact owned project/config pointers BEFORE any teardown.
    atomic(output/"cleanup-recovery.json",read_json(config))
    atomic(output/"cleanup.json",dict(containers=None,volumes=None,secrets_removed=False,cleanup_complete=False,failure_code="cleanup_budget_exhausted"))
    try:
        command([sys.executable,__file__,"cleanup-worker",str(config),str(output),str(max(0,remaining-.1))],remaining)
        return 0
    except (CommandFailure,OSError): return 1

def smoke_summary(lines):
    phases={}; counters={}; first={}; previous={}; resets=set(); count=0
    for line in lines:
        if count>=10000: break
        count+=1
        try: row=json.loads(line[line.index("{"):])
        except (ValueError,TypeError): continue
        if row.get("msg")=="sync diagnostic" and row.get("phase") in {"authority","database","observer","security_fence","family_fence","transaction_acquire","integrity_inventory","integrity_extension","integrity_verify"} and finite(row.get("elapsed_ns")):
            phase=row["phase"]; phases[phase]=phases.get(phase,0)+row["elapsed_ns"]
        if row.get("msg")=="sync resource diagnostic":
            for key in ["alloc_total_bytes","cpu_total_seconds","rss_peak_bytes","runtime_pool_acquire_ns","runtime_pool_acquire_count","runtime_pool_wait_ns","runtime_pool_total_connections"]:
                if finite(row.get(key)):
                    first.setdefault(key,row[key])
                    if key in previous and row[key]<previous[key]: resets.add(key)
                    previous[key]=row[key]; counters[key]=max(counters.get(key,0),row[key])
    print("sync-metric-proof: "+json.dumps(dict(phase_elapsed_ns=phases,cumulative_process_runtime_pool=counters,observed_counter_deltas={key:(None if key in resets else previous[key]-first[key]) for key in counters if key not in {"rss_peak_bytes","runtime_pool_total_connections"}},counter_reset_or_out_of_order=sorted(resets),scope="process/runtime-pool first-to-last observed sample; no per-request attribution"),sort_keys=True))

def sample_summary(path):
    rows=[]
    if Path(path).exists():
        for line in Path(path).read_text().splitlines():
            try: rows.append(json.loads(line))
            except ValueError: pass
    output=dict(sample_count=len(rows),scope="owned recovery gate Docker/database samples, not capacity")
    for key in ["container_cpu_percent","container_memory_bytes","db_connections"]:
        values=[r.get("resources",{}).get(key) for r in rows];values=[v for v in values if finite(v)]
        output[key+"_max"]=max(values) if values else None
    for key in ["network_counter_bytes","filesystem_counter_bytes"]:
        values=[r.get("resources",{}).get(key) for r in rows];values=[v for v in values if finite(v)]
        timestamps=[r.get("time") for r in rows]
        ordered=all(finite(t) for t in timestamps) and all(b>=a for a,b in zip(timestamps,timestamps[1:]))
        monotonic=all(b>=a for a,b in zip(values,values[1:]))
        output[key+"_delta"]=values[-1]-values[0] if len(values)>=2 and ordered and monotonic else None
        if output[key+"_delta"] is None:
            output[key+"_unavailable_reason"]="insufficient_counter_samples" if len(values)<2 else "counter_reset_or_out_of_order_samples"
    print("sync-resource-proof: "+json.dumps(output,sort_keys=True))

if __name__ == "__main__":
    if sys.argv[1] == "supervise": sys.exit(supervise(sys.argv[2],sys.argv[3:]))
    if sys.argv[1] == "aggregate":
        try: build=read_json(sys.argv[5])
        except (OSError,ValueError): build={}
        aggregate(sys.argv[2],sys.argv[3],sys.argv[4],build,sys.argv[6],sys.argv[7])

    if sys.argv[1] == "cleanup-worker": sys.exit(cleanup_worker(sys.argv[2],sys.argv[3],float(sys.argv[4])))
    if sys.argv[1] == "cleanup": sys.exit(bounded_cleanup(sys.argv[2],sys.argv[3],max(0,float(sys.argv[4])-time.time()-2)))

    if sys.argv[1] == "preserve": preserve_evidence(sys.argv[2],sys.argv[3])
    if sys.argv[1] == "sample-loop": sample_loop(sys.argv[2],sys.argv[3],sys.argv[4])

    if sys.argv[1] == "smoke-summary": smoke_summary(sys.stdin)

    if sys.argv[1] == "sample-summary": sample_summary(sys.argv[2])
