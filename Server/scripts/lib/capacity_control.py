"""Private bounded capacity supervisor. Never discovers/deletes unowned resources."""
import platform
import tempfile
from contextlib import contextmanager
import argparse, hashlib, importlib.util, json, os, pathlib, re, shutil, signal, stat, subprocess, sys, time
from capacity_metrics import Sampler
from performance_control import CommandFailure

LABEL = "com.babytracker.capacity.owner"
LIMIT = 16 * 1024 * 1024
class Unsafe(ValueError): pass

# The runner owns signals from preflight/builds through the last cleanup. Signal
# handlers only set state; the currently owned command joins its own group.
_cancellation = None
class Cancellation:
    def __init__(self): self.interrupted = [False]; self.cleaning = False
    @contextmanager
    def own(self):
        global _cancellation
        previous_state = _cancellation; previous = {}
        _cancellation = self
        try:
            for sig in (signal.SIGINT, signal.SIGTERM):
                previous[sig] = signal.signal(sig, lambda *_: self.interrupted.__setitem__(0, True))
            yield self
        finally:
            for sig, handler in previous.items(): signal.signal(sig, handler)
            _cancellation = previous_state
    def check(self):
        if self.interrupted[0] and not self.cleaning: raise CommandFailure("interrupted")

def command(args, timeout, *, cwd=None, env=None):
    if _cancellation: _cancellation.check()
    if timeout <= 0: raise CommandFailure("cleanup_budget_exhausted")
    with tempfile.TemporaryFile() as output:
        child = subprocess.Popen(args, cwd=cwd, env=env, stdout=output, stderr=subprocess.DEVNULL, start_new_session=True)
        deadline = time.monotonic() + timeout
        try:
            while child.poll() is None:
                if _cancellation: _cancellation.check()
                if time.monotonic() >= deadline: raise CommandFailure("command_timeout")
                if os.fstat(output.fileno()).st_size > LIMIT: raise CommandFailure("output_bound")
                time.sleep(.01)
            if _cancellation: _cancellation.check()
            if child.wait(): raise CommandFailure("command_failed")
            output.seek(0); data=output.read(LIMIT+1)
            if len(data)>LIMIT: raise CommandFailure("output_bound")
            return data.decode(errors="replace")
        finally:
            if child.poll() is None:
                try: os.killpg(child.pid,signal.SIGTERM)
                except ProcessLookupError: pass
                try: child.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    try: os.killpg(child.pid,signal.SIGKILL)
                    except ProcessLookupError: pass
            child.wait()
            if not reap_group(child.pid,time.monotonic()+4): raise CommandFailure("command_group_cleanup_failed")

def atomic(path, value):
    path = pathlib.Path(path)
    temporary = path.with_name(path.name + ".supervisor.tmp")
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(fd, "w") as out:
            json.dump(value, out, sort_keys=True); out.flush(); os.fsync(out.fileno())
        os.replace(temporary, path)
        parent = os.open(path.parent, os.O_RDONLY)
        try: os.fsync(parent)
        finally: os.close(parent)
    finally:
        if temporary.exists(): temporary.unlink()

def safe(path, directory=False, fresh=False):
    p = pathlib.Path(path)
    if not p.is_absolute() or str(p) != os.path.normpath(str(p)): raise Unsafe("absolute_clean_path_required")
    for part in [p, *p.parents]:
        if part.is_symlink(): raise Unsafe("symlink_refused")
    if fresh:
        if p.exists() or not p.parent.is_dir() or p.parent.stat().st_uid != os.getuid(): raise Unsafe("fresh_owned_path_required")
        return p
    st = p.lstat()
    if st.st_uid != os.getuid() or stat.S_IMODE(st.st_mode) != (0o700 if directory else 0o600): raise Unsafe("private_owned_path_required")
    if not (stat.S_ISDIR(st.st_mode) if directory else stat.S_ISREG(st.st_mode)): raise Unsafe("regular_path_required")
    return p

def read(path):
    path = safe(path)
    if path.stat().st_size > LIMIT: raise Unsafe("artifact_bound")
    return json.loads(path.read_text())

def reserved_path(path):
    p=pathlib.Path(path)
    if not p.is_absolute() or str(p)!=os.path.normpath(str(p)):raise Unsafe("receipt_path")
    for part in [p,*p.parents]:
        if part.is_symlink():raise Unsafe("receipt_symlink")
    if p.exists():safe(p,directory=True)
    else:
        ancestor=next((v for v in p.parents if v.exists()),None)
        if ancestor is None or not ancestor.is_dir() or ancestor.stat().st_uid!=os.getuid():raise Unsafe("receipt_parent")

def receipt(path, control):
    r = read(path)
    if set(r) - {"version", "owner", "root", "restore_root", "fixture_root", "fixture_sealed", "containers", "networks"} or r.get("version") != 1: raise Unsafe("receipt_schema")
    owner = r.get("owner", "")
    if not re.fullmatch(r"[a-f0-9]{32}", owner) or path.name != "resources-" + owner + ".json": raise Unsafe("receipt_owner")
    prefix = "bt-capacity-" + owner
    root = pathlib.Path(r["root"])
    if root.parent != pathlib.Path(os.path.realpath(__import__('tempfile').gettempdir())) or not root.name.startswith(prefix + "-"): raise Unsafe("receipt_private_root")
    reserved_path(root)
    restore = r.get("restore_root")
    if restore:
        restore = pathlib.Path(restore)
        owned_verify = restore.name == "verify" and restore.parent.parent == root.parent and re.fullmatch(r"bt-capacity-[a-f0-9]{32}-[a-f0-9]+", restore.parent.name)
        if owned_verify:
            parent_owner=restore.parent.name.split('-')[2]
            parent_receipt=read(control/('resources-'+parent_owner+'.json'))
            if parent_receipt.get('root')!=str(restore.parent):raise Unsafe("receipt_verify_owner")
        if not owned_verify and (restore.name != "restore" or restore.parent.parent != control.parent.parent): raise Unsafe("receipt_restore_root")
        reserved_path(restore)
    fixture = r.get("fixture_root")
    if fixture:
        fixture = pathlib.Path(fixture)
        if fixture.parent != control.parent.parent or fixture.name in {".private", "restore"}: raise Unsafe("receipt_fixture_root")
        reserved_path(fixture)
        if type(r.get("fixture_sealed",False)) is not bool: raise Unsafe("receipt_fixture_seal")
    allowed = {prefix, prefix + "-app", prefix + "-proxy"}
    if not isinstance(r.get("containers"), list) or len(r["containers"]) > 3 or len(set(r["containers"])) != len(r["containers"]) or set(r["containers"]) - allowed: raise Unsafe("receipt_container")
    if r.get("networks") is None: r["networks"] = []
    if not isinstance(r.get("networks"), list) or r["networks"] not in [[], [prefix + "-net"]]: raise Unsafe("receipt_network")
    return r

def remaining(deadline): return max(0, deadline - time.monotonic())

def cleanup(control, deadline, runner=command):
    control = safe(control, directory=True)
    for path in sorted(control.glob("resources-*.json")):
        r = receipt(path, control)
        for kind, names in [("container", list(reversed(r["containers"]))), ("network", r["networks"])]:
            for name in names:
                # Exact names only. A replaced/mislabeled object is never removed.
                args = ["docker", "ps", "--all", "--quiet", "--filter", "name=^/" + name + "$"] if kind == "container" else ["docker", "network", "ls", "--quiet", "--filter", "name=^" + name + "$"]
                present = runner(args, min(5, remaining(deadline))).strip()
                if not present: continue
                template = '{{index .Config.Labels "' + LABEL + '"}}' if kind == "container" else '{{index .Labels "' + LABEL + '"}}'
                observed = runner(["docker", kind, "inspect", "--format", template, name], min(5, remaining(deadline))).strip()
                if observed != r["owner"]: raise Unsafe("cleanup_ownership_failed")
                args = ["docker", "rm", "--force", "--volumes", name] if kind == "container" else ["docker", "network", "rm", name]
                runner(args, min(10, remaining(deadline)))
                if runner(["docker", kind, "ls", "--quiet", "--filter", "label=" + LABEL + "=" + r["owner"]], min(5, remaining(deadline))).strip() and kind == "network": raise Unsafe("cleanup_network_remains")
        for kind in ["container", "volume", "network"]:
            args = ["docker", "ps", "--all", "--quiet"] if kind == "container" else ["docker", kind, "ls", "--quiet"]
            if runner(args + ["--filter", "label=" + LABEL + "=" + r["owner"]], min(5, remaining(deadline))).strip(): raise Unsafe("cleanup_resources_remain")
        for name in [r.get("restore_root"), r["root"], r.get("fixture_root") if not r.get("fixture_sealed") else None]:
            if name and pathlib.Path(name).exists():
                safe(name, directory=True)
                shutil.rmtree(name)
                if pathlib.Path(name).exists(): raise Unsafe("cleanup_private_material_remains")
    return {"complete": True, "reason": "owned_resources_absent"}

def recover(path, reason):
    try:
        row = read(path)
        if not isinstance(row, dict) or row.get("completion") not in {"complete", "interrupted", "deadline", "setup_failed", "sample_incomplete", "cleanup_failed", "unmeasured", "gated"}: raise Unsafe("result_schema")
        # Invalid/corrupt private files are not promoted into a successful report.
        for key in ["attempts", "successes", "refusals", "unexpected_errors", "acknowledged", "peer_applied"]:
            if type(row.get(key, 0)) is not int or row.get(key, 0) < 0: raise Unsafe("result_counts")
    except (OSError, ValueError, TypeError): row = {"version": 1, "measurement_seconds": 0, "latency_seconds": [], "completion": "invalid_result"}
    if reason: row["completion"] = reason
    row["passing"] = False; row["percentiles"] = None; row["peer_http_percentiles"] = None
    atomic(path, row)

def reap_group(pid, deadline):
    # Every supervised child starts its own session. Its Swift descendants stay
    # in that owned group; even a normally exited launcher may leave one behind.
    try: os.killpg(pid,signal.SIGTERM)
    except ProcessLookupError: return True
    grace=min(deadline,time.monotonic()+2)
    while time.monotonic()<grace:
        try: os.killpg(pid,0)
        except ProcessLookupError: return True
        time.sleep(.02)
    try: os.killpg(pid,signal.SIGKILL)
    except ProcessLookupError: return True
    while time.monotonic()<min(deadline,grace+2):
        try: os.killpg(pid,0)
        except ProcessLookupError: return True
        time.sleep(.02)
    return False

def supervise(args, control, checkpoint, work_deadline, final_deadline, cwd=None, env=None):
    control = safe(control, directory=True)
    child_env = dict(os.environ if env is None else env); child_env["BT_CAPACITY_CONTROL_DIR"] = str(control)
    interrupted = _cancellation.interrupted if _cancellation else [False]
    previous = {}
    for sig in ([] if _cancellation else [signal.SIGTERM, signal.SIGINT]):
        previous[sig] = signal.signal(sig, lambda *_: interrupted.__setitem__(0, True))
    code = 1; reason = None
    try:
        with open(control / "child.private.log", "ab", buffering=0) as log:
            os.chmod(control / "child.private.log", 0o600)
            child = subprocess.Popen(args, cwd=cwd, env=child_env, stdout=log, stderr=log, start_new_session=True)
            atomic(control / "child.json", {"pid": child.pid})
            def owned_containers():
                # Receipt validation prevents arbitrary host/container discovery.
                rows=[]
                for path in control.glob("resources-*.json"):
                    r=receipt(path,control)
                    for name in r['containers']:
                        role='app' if name.endswith('-app') else 'proxy' if name.endswith('-proxy') else 'database'
                        rows.append((role,name))
                return rows[-3:]
            sampler=Sampler(child.pid,owned_containers);sampler.start()
            try:
                while child.poll() is None:
                    if interrupted[0] or time.monotonic() >= work_deadline or log.tell() > LIMIT:
                        reason = "interrupted" if interrupted[0] else "deadline"
                        os.killpg(child.pid, signal.SIGTERM)
                        term_deadline = min(final_deadline, time.monotonic() + 5)
                        while child.poll() is None and time.monotonic() < term_deadline: time.sleep(.02)
                        if child.poll() is None: os.killpg(child.pid, signal.SIGKILL)
                        break
                    time.sleep(.02)
                code = child.wait()
                if code and reason is None:
                    reason = "interrupted" if code < 0 else "setup_failed"
                    if code > 0:
                        try:
                            observed = read(checkpoint).get("completion")
                            if observed in {"interrupted", "deadline", "cleanup_failed", "sample_incomplete"}: reason = observed
                        except (OSError,ValueError,TypeError): pass
            finally:
                if child.poll() is None:
                    os.killpg(child.pid, signal.SIGKILL); child.wait()
                if not reap_group(child.pid,final_deadline):reason="cleanup_failed";code=1
                atomic(checkpoint.parent/"resources.json",sampler.finish())
        if not code and reason is None:
            try:
                row=read(checkpoint)
                if not isinstance(row,dict) or row.get("completion") not in {"complete","sample_incomplete"}: raise Unsafe("child_success_incomplete")
                for key in ["attempts","successes","refusals","unexpected_errors","acknowledged","peer_applied"]:
                    if type(row.get(key,0)) is not int or row.get(key,0)<0: raise Unsafe("invalid_result_counts")
            except (OSError,ValueError,TypeError): reason="invalid_result";code=1
        cleanup_result = {"complete": False, "reason": "cleanup_failed"}
        try:
            if _cancellation: _cancellation.cleaning = True
            cleanup_result = cleanup(control, final_deadline)
        except (OSError, ValueError, CommandFailure): reason = "cleanup_failed"; code = 1
        finally:
            if _cancellation: _cancellation.cleaning = False
        atomic(control.parent / "cleanup.json", cleanup_result)
        if reason: recover(checkpoint, reason)
        elif not checkpoint.exists(): recover(checkpoint, "invalid_result"); code = 1
        return code
    finally:
        for sig, handler in previous.items(): signal.signal(sig, handler)

AMBIENT = {"BABY_TRACKER_TEST_DATABASE_URL", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"}
def validate_environment():
    if any(v and (k.startswith("BT_") or k in AMBIENT) for k, v in os.environ.items()): raise Unsafe("ambient_configuration_refused")
    contexts = json.loads(command(["docker", "context", "inspect"], 5))
    if len(contexts) != 1 or not contexts[0]["Endpoints"]["docker"]["Host"].startswith("unix:///"): raise Unsafe("local_docker_required")

def source_identity(source):
    source = pathlib.Path(source)
    if not source.is_absolute() or str(source) != os.path.realpath(source): raise Unsafe("clean_absolute_source_required")
    sha = command(["git", "-C", str(source), "rev-parse", "HEAD"], 5).strip()
    if not re.fullmatch(r"[a-f0-9]{40}", sha) or command(["git", "-C", str(source), "status", "--porcelain", "--untracked-files=all"], 5).strip(): raise Unsafe("clean_source_required")
    return sha

def file_digest(path):
    path=pathlib.Path(path)
    if path.is_symlink() or not path.is_file(): raise Unsafe("input_regular_file_required")
    return hashlib.sha256(path.read_bytes()).hexdigest()

# Include the actual loaded Python helpers, not fixture-construction identity.
LOADED_INPUTS = {str(pathlib.Path(v).resolve()): file_digest(pathlib.Path(v).resolve()) for v in
                 [__file__, sys.modules[Sampler.__module__].__file__, sys.modules[CommandFailure.__module__].__file__]}

def input_identity(root):
    root=pathlib.Path(root); files={}
    for path in sorted((root/"Server").rglob("*")):
        if path.is_file() and (path.suffix in {".go",".sql",".py",".sh",".json",".html"} or path.name in {"go.mod","go.sum","Dockerfile",".dockerignore"}):
            files[str(path.relative_to(root))]=file_digest(path)
    digest=hashlib.sha256(json.dumps(files,sort_keys=True,separators=(",",":")).encode()).hexdigest()
    return dict(input_sha256=digest,inputs=files)

def harness_identity(root):
    identity=dict(revision=source_identity(str(root)),**input_identity(root))
    for path,digest in LOADED_INPUTS.items():
        if file_digest(path)!=digest: raise Unsafe("loaded_harness_changed")
    identity["loaded_python_inputs"]={pathlib.Path(p).name:v for p,v in LOADED_INPUTS.items()}
    return identity

def client_build_controls():
    # Forbid ambient flags/workspaces/overlays and cgo inputs. Keep ordinary host
    # environment for executable/cache discovery, recording the effective toolchain.
    env={k:v for k,v in os.environ.items() if not k.startswith(("GO","CGO"))}
    env.update(GOENV="off",GOWORK="off",GOFLAGS="",GOEXPERIMENT="",GOTOOLCHAIN="local",CGO_ENABLED="0")
    go=shutil.which("go",path=env.get("PATH"))
    if not go: raise Unsafe("go_toolchain_missing")
    controls=dict(go_executable=str(pathlib.Path(go).resolve()),go_executable_sha256=file_digest(pathlib.Path(go).resolve()),
                  python_executable=str(pathlib.Path(sys.executable).resolve()),python_executable_sha256=file_digest(pathlib.Path(sys.executable).resolve()),
                  python_version=sys.version,environment={k:env[k] for k in ("GOENV","GOWORK","GOFLAGS","GOEXPERIMENT","GOTOOLCHAIN","CGO_ENABLED")},
                  effective_go=json.loads(command([go,"env","-json","GOVERSION","GOOS","GOARCH","GOROOT","GOTOOLDIR","CGO_ENABLED","GOFLAGS","GOWORK","GOENV","GOTOOLCHAIN"],5,env=env)))
    return env,controls

def required_anchor(devices,rate,shape):
    # Diagnostics are not anchors. Higher rates/shapes require their population's
    # steady15 anchor; population expansion requires the preceding steady15 anchor.
    if shape!="steady" or rate>15: return (devices,15,"steady",60)
    if devices>32: return ({128:32,512:128}[devices],15,"steady",60)
    return None

def accepted_anchor(row,cell,sha,profile,database_profile):
    devices,rate,shape,seconds=cell
    requested=row.get("requested",{})
    return (cell==(devices,15,"steady",60) and devices//2*rate>=200 and
            row.get("passing") is True and row.get("completion")=="complete" and
            row.get("successes",0)>=200 and row.get("peer_applied",0)>=200 and
            row.get("actual_devices")==devices and row.get("actual_writers")==devices//2 and
            row.get("planned_writes")==devices//2*rate and row.get("measurement_seconds")==seconds and
            row.get("scenario")==shape and row.get("product_sha")==sha and row.get("profile")==profile and row.get("database_profile")==database_profile and
            row.get("refusal_classification")=="quota_429_and_typed_server_busy_v1" and
            all(requested.get(k)==v for k,v in dict(devices=devices,active_writers=devices//2,writes_per_writer_per_minute=rate,measure_ns=seconds*1_000_000_000).items()))

# MCP-enabled matched profiles and their MCP concurrency (dedicated pool and
# global MCP admission maximum; see Go databaseProfile). Lowered limits are
# named profiles so every receipt records the limit actually launched.
MCP_PROFILES={"explicit-mcp":4,"explicit-mcp-3":3,"explicit-mcp-2":2,"explicit-mcp-1":1}

def matched_options(opts):
    if opts.scenario!="matched":
        if opts.measure_seconds is not None or opts.rate is not None or opts.mcp_mode!="none" or opts.mcp_clients or opts.mcp_rate or opts.mcp_abuse_concurrency or opts.database_profile in MCP_PROFILES: raise Unsafe("matched_options_require_matched_scenario")
        return None
    if opts.rate is None: opts.rate=15
    if opts.measure_seconds is None or not 60<=opts.measure_seconds<=900 or not 1<=opts.rate<=60: raise Unsafe("matched_window_or_rate_invalid")
    if (opts.mcp_mode!="none")!=(opts.database_profile in MCP_PROFILES): raise Unsafe("matched_mcp_profile_mismatch")
    if opts.mcp_mode in {"none","idle"} and (opts.mcp_clients or opts.mcp_rate or opts.mcp_abuse_concurrency): raise Unsafe("matched_mcp_options_without_load")
    if opts.mcp_mode=="none": return None
    return dict(mode=opts.mcp_mode,clients=opts.mcp_clients,calls_per_client_per_minute=opts.mcp_rate,abuse_concurrency=opts.mcp_abuse_concurrency,admission_max=MCP_PROFILES[opts.database_profile])

def runner(argv):
    with Cancellation().own() as cancellation:
        return run_owned(argv,cancellation)

def run_owned(argv,cancellation):
    p = argparse.ArgumentParser();p.add_argument("--database-profile",choices=["legacy","explicit",*MCP_PROFILES],default="legacy");p.add_argument("--metrics",choices=["off","on"],default="off");p.add_argument("--profile",choices=["small","standard"],required=True);p.add_argument("--scenario",choices=["pilot","sweep","soak","swift","swift-lifecycle","matched"],required=True);p.add_argument("--source",required=True);p.add_argument("--fixture",required=True);p.add_argument("--output",required=True);p.add_argument("--total-seconds",type=int,required=True)
    # Matched mixed-load cells only: one 32-device steady cell with an explicit
    # window/rate, and an MCP shape the Go workload validates again.
    p.add_argument("--measure-seconds",type=int);p.add_argument("--rate",type=int);p.add_argument("--mcp-mode",choices=["none","idle","read","abusive"],default="none");p.add_argument("--mcp-clients",type=int,default=0);p.add_argument("--mcp-rate",type=int,default=0);p.add_argument("--mcp-abuse-concurrency",type=int,default=0)
    opts=p.parse_args(argv)
    if opts.total_seconds <= 0 or opts.total_seconds > 7200: raise Unsafe("positive_bounded_total_required")
    mcp=matched_options(opts)
    start=time.monotonic();deadline=start+opts.total_seconds;work_deadline=deadline-min(45,opts.total_seconds/3)
    validate_environment();sha=source_identity(opts.source);fixture=safe(opts.fixture,directory=True);output=safe(opts.output,fresh=True);output.mkdir(mode=0o700)
    private=output/".private";private.mkdir(mode=0o700);control=private/"control";control.mkdir(mode=0o700)
    harness=pathlib.Path(__file__).resolve().parents[3]
    cells=[(32,opts.rate,"steady",opts.measure_seconds)] if opts.scenario=="matched" else [(32,15,"steady",60)] if opts.scenario in {"pilot","swift","swift-lifecycle"} else ([(32,15,"steady",900)] if opts.scenario=="soak" else [(d,r,"steady",60) for d in [32,128,512] for r in [1,15,60]]+[(32,15,c,60) for c in ["contention","catch-up","slow-consumer","reconnect"]])
    for i,(devices,rate,shape,seconds) in enumerate(cells):
        cell=output/("cell-%02d"%(i+1));cell.mkdir(mode=0o700)
        atomic(cell/"cell.json",dict(version=1,completion="setup_failed",measurement_seconds=0,successes=0,refusals=0,unexpected_errors=0,latency_seconds=[],passing=False,percentiles=None,scenario=shape,planned_cell=dict(devices=devices,active_writers=2 if shape=="contention" else devices//2,rate=rate,seconds=seconds,shape=shape),measurement_state="unmeasured"))
    atomic(output/"status.json",dict(status=1,completion="setup_failed",elapsed_seconds=time.monotonic()-start))
    metadata=dict(database_profile=opts.database_profile,database_pools=dict(runtime=16,projector=4,maintenance=2,mcp=MCP_PROFILES.get(opts.database_profile,0),server_max=64,reserved=8) if opts.database_profile!="legacy" else None,mcp=mcp,profile=opts.profile,scenario=opts.scenario,product_sha=sha,fixture_manifest=read(fixture/"manifest.json"),total_seconds=opts.total_seconds,load_generator="host process outside app/db/proxy quota budgets; generator current RSS and cumulative CPU sampled separately in each resources.json",load_generator_pid=os.getpid(),host_architecture=platform.machine(),host_cpu_count=os.cpu_count(),host_load_average_start=os.getloadavg())
    if opts.scenario=="matched": metadata.update(measure_seconds=opts.measure_seconds,rate=opts.rate)
    atomic(output/"run.json",metadata)
    status=1;completed=False;failure=None;anchors=set()
    try:
        identity=harness_identity(harness); product_identity=input_identity(pathlib.Path(opts.source))
        metadata.update(harness=identity,product_inputs=product_identity);atomic(output/"run.json",metadata)
        env,controls=client_build_controls();client=private/"capacity-client";source=pathlib.Path(opts.source)
        recipe=[controls["go_executable"],"build","-trimpath","-tags","identitytest","-o",str(client),"./cmd/sync-capacity-client"]
        metadata["client_build"]=dict(controls=controls,argv=recipe,cwd=str(harness/"Server"));atomic(output/"run.json",metadata)
        def validate_inputs(binary=None):
            if harness_identity(harness)!=identity: raise Unsafe("harness_changed_during_run")
            if source_identity(opts.source)!=sha or input_identity(source)!=product_identity: raise Unsafe("source_changed_during_run")
            if file_digest(controls["go_executable"])!=controls["go_executable_sha256"] or file_digest(controls["python_executable"])!=controls["python_executable_sha256"]: raise Unsafe("toolchain_changed_during_run")
            if binary is not None and file_digest(client)!=binary: raise Unsafe("client_binary_changed_during_run")
        validate_inputs()
        command(recipe,min(90,remaining(work_deadline)),cwd=str(harness/"Server"),env=env)
        validate_inputs();client_hash=file_digest(client);metadata["client_binary_sha256"]=client_hash;atomic(output/"run.json",metadata)
        image="bt-capacity-product:"+sha
        command(["docker","build","--build-arg","BT_SOURCE_SHA="+sha,"--tag",image,str(source/"Server")],min(150,remaining(work_deadline)))
        validate_inputs(client_hash)
        image_id=command(["docker","image","inspect","--format","{{.Id}}",image],5).strip()
        image_meta=json.loads(command(["docker","image","inspect",image],5))[0]
        # Keep the historical product input algorithm for cell/fixture compatibility.
        product_hash=hashlib.sha256()
        for path in sorted((source/"Server").rglob("*")):
            if path.is_file() and (path.suffix in {".go",".sql"} or path.name in {"go.mod","go.sum","Dockerfile"}):
                product_hash.update(str(path.relative_to(source/"Server")).encode()+b"\0"+path.read_bytes()+b"\0")
        product_input=product_hash.hexdigest();binary_hash=""
        metadata.update(product_input_sha256=product_input,image_id=image_id,image_arch=image_meta["Architecture"]);atomic(output/"run.json",metadata)
        status=0
        for i,planned in enumerate(cells):
            devices,rate,shape,seconds=planned;cell=output/("cell-%02d"%(i+1));checkpoint=cell/"cell.json"
            cancellation.check()
            anchor=required_anchor(devices,rate,shape) if opts.scenario=="sweep" else None
            if anchor and anchor not in anchors:
                recover(checkpoint,"gated");row=read(checkpoint);row["required_anchor"]=dict(devices=anchor[0],active_writers=anchor[0]//2,rate=anchor[1],shape=anchor[2],seconds=anchor[3]);atomic(checkpoint,row);status=1;continue
            if remaining(work_deadline)<seconds+30+20:
                recover(checkpoint,"deadline");status=1;continue
            validate_inputs(client_hash)
            args=[str(client),"run","--database-profile",opts.database_profile,"--metrics="+str(opts.metrics=="on").lower(),"--fixture",str(fixture),"--output",str(cell),"--image",image_id,"--profile",opts.profile,"--scenario",shape,"--devices",str(devices),"--rate",str(rate),"--measure-seconds",str(seconds),"--product-sha",sha,"--product-input",product_input,"--product-binary",binary_hash,"--cell",shape,"--total-seconds",str(max(1,int(remaining(work_deadline))))]
            if mcp:args += ["--mcp-mode",mcp["mode"],"--mcp-clients",str(mcp["clients"]),"--mcp-rate",str(mcp["calls_per_client_per_minute"]),"--mcp-abuse-concurrency",str(mcp["abuse_concurrency"])]
            if opts.scenario in {"swift","swift-lifecycle"}:args += ["--swift-package",str(source/"Packages/BabyTracker"),"--swift-mode","lifecycle" if opts.scenario=="swift-lifecycle" else "latency"]
            code=supervise(args,control,checkpoint,work_deadline,deadline)
            row=read(checkpoint);clean=read(private/"cleanup.json").get("complete") is True
            if row.get("completion")=="interrupted": cancellation.interrupted[0]=True
            cancellation.check()
            validate_inputs(client_hash)
            if not clean: raise Unsafe("cleanup_failed")
            if code: status=1
            elif accepted_anchor(row,planned,sha,opts.profile,opts.database_profile): anchors.add(planned)
        validate_inputs(client_hash);completed=True
    except (OSError,ValueError,CommandFailure) as err:
        failure=err
        status=1
        # Keep measured history intact but revoke acceptance after input drift.
        if isinstance(err,Unsafe):
            metadata["provenance_failure"]=str(err)
            for checkpoint in sorted(output.glob("cell-*/cell.json")):
                row=read(checkpoint)
                if row.get("measurement_state")!="unmeasured": recover(checkpoint,"invalid_result")
    finally:
        cancellation.cleaning=True
        cleanup_result=dict(complete=False,reason="cleanup_failed")
        try: cleanup_result=cleanup(control,deadline)
        except (OSError,ValueError,CommandFailure): status=1
        atomic(private/"cleanup.json",cleanup_result)
        if not cleanup_result["complete"]: status=1
        for checkpoint in sorted(output.glob("cell-*/cell.json")):
            row=read(checkpoint)
            if row.get("measurement_state")=="unmeasured":
                if cancellation.interrupted[0]: recover(checkpoint,"interrupted")
                elif row.get("completion")=="setup_failed" and completed: recover(checkpoint,"unmeasured")
            if re.fullmatch(r"[a-f0-9]{64}",row.get("product_binary_sha256","")):metadata["binary_sha256"]=row["product_binary_sha256"]
        if cancellation.interrupted[0]:status=1
        metadata["provenance_verified"]=completed and failure is None
        atomic(output/"run.json",metadata)
        atomic(output/"status.json",dict(host_load_average_end=os.getloadavg(),status=status,elapsed_seconds=time.monotonic()-start,completion="interrupted" if cancellation.interrupted[0] else "complete" if status==0 else "incomplete"))
    if failure is not None and not cancellation.interrupted[0]: raise failure
    print("capacity artifacts: "+str(output));return status

def main():
    try:return runner(sys.argv[1:])
    except (ValueError,OSError,CommandFailure):print("capacity_runner_failed; inspect private checkpoints",file=sys.stderr);return 1
if __name__=="__main__":sys.exit(main())
