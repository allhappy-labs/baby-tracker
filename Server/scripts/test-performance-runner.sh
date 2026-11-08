#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
runner=Server/scripts/sync-performance.sh
[[ -f $runner ]] || { echo "missing performance runner" >&2; exit 1; }
root=$(mktemp -d /private/tmp/baby-sync-perf-tests.XXXXXXXX)
trap 'rm -rf "$root"' EXIT
if [[ ${1:-} == --revision-controls-only ]]; then
  python3 - "$PWD/Server/scripts/lib" "$root" <<'REVISION_CONTROLS'
import sys,pathlib,json,time,subprocess,os,contextlib,io
sys.path.insert(0,sys.argv[1])
import performance_control as pc
root=pathlib.Path(sys.argv[2])
# Baseline mode changes product cwd after sourcing a RELATIVE current runner.
anchored=subprocess.run(['bash','-c','source Server/scripts/sync-performance.sh; cd "$1"; run_matrix_child "$1" "$2" -c "$3"','anchor',str(root),sys.executable,'print("anchored")'],cwd=pathlib.Path(sys.argv[1]).parents[2],capture_output=True,text=True,timeout=5)
assert anchored.returncode==0 and 'anchored' in anchored.stdout, 'helper followed product cwd instead of frozen harness source'
row=dict(cell=1,families=1,concurrency=1,requested_events=248,actual_events=17,completion='not_started',elapsed_seconds=.1,measurement_seconds=.05,pairs=[dict(operation=1,durable_seconds=.01,observable_seconds=.02)],request_seconds=[],attempted=dict(empty_pull=0,caught_up_read=0,save_apply=1),successful=dict(empty_pull=0,caught_up_read=0,save_apply=1))
assert pc.validate_row(row)['pairs']==row['pairs']
normal=root/'normal.log';normal.write_text('capacity-matrix cell: '+json.dumps(row)+'\n')
normal_rows=pc.aggregate(normal,root/'normal.json','swift_matrix',{})
assert normal_rows[0]['successful_qps']==20 and normal_rows[0]['durable_peer']['count']==1
normal.write_text(normal.read_text()*2)
assert pc.aggregate(normal,root/'duplicate.json','swift_matrix',{})[0]['completion']=='invalid_runtime_artifact'
# N1: a malformed duplicate must invalidate its own cell, not the last valid one.
first=json.loads(json.dumps(row));first['completion']='measurement_complete'
second=json.loads(json.dumps(first));second.update(cell=2,concurrency=16)
bad_first=json.loads(json.dumps(first));bad_first['pairs'][0]['durable_seconds']=-1
cross=root/'cross-cell.log'
cross.write_text(''.join('capacity-matrix cell: '+json.dumps(r)+'\n' for r in [first,second,bad_first]))
cross_rows=pc.aggregate(cross,root/'cross-cell.json','swift_matrix',{})
assert cross_rows[0]['completion']=='invalid_runtime_artifact' and cross_rows[0]['durable_peer'] is None
assert cross_rows[1]['completion']=='measurement_complete' and cross_rows[1]['durable_peer']['count']==1
cross.write_text('capacity-matrix cell: '+json.dumps(first)+'\ncapacity-matrix cell: '+json.dumps({'cell':'unidentified'})+'\n')
unknown_rows=pc.aggregate(cross,root/'unidentified.json','swift_matrix',{})
assert all(r['completion']=='global_artifact_failure' and r['durable_peer'] is None and r['successful_qps'] is None for r in unknown_rows)
print('N1 proof: cross-cell malformed duplicate attributed correctly; unidentified input fails globally')
# V1: a matrix whose cells measured nothing is not a passing run. Each cell is
# judged coherent only when it measured every requested axis value with exact
# accounting; the CLI exits non-zero unless all 27 are.
def measured(cell,request=.01):
 o=cell-1;families=[1,32,128][o%9//3];events=[248,512,2048][o//9]
 return dict(cell=cell,families=families,actual_families=families,concurrency=[1,16,64][o%3],requested_events=events,actual_events=events+3,
  completion='measurement_complete',failure_stage='measurement',failure_code='none',elapsed_seconds=60,measurement_seconds=30,
  pairs=[dict(operation=5,durable_seconds=.1,observable_seconds=.2)],request_seconds=[request]*4,
  attempted=dict(empty_pull=2,caught_up_read=2,save_apply=1),successful=dict(empty_pull=2,caught_up_read=2,save_apply=1),errors=0,rate_limits=0,timeouts=0)
def matrix(rows,name):
 log=root/(name+'.log');log.write_text(''.join('capacity-matrix cell: '+json.dumps(r)+'\n' for r in rows))
 out=root/(name+'-cells');out.mkdir()
 code=subprocess.run([sys.executable,pc.__file__,'aggregate',str(log),str(out/'cells.json'),'complete',str(root/'absent-build.json'),str(root/'absent-evidence.jsonl'),str(root/'absent-checkpoints')]).returncode
 return code,json.loads((out/'cells.json').read_text()),json.loads((out/'matrix-verdict.json').read_text())
code,cells,verdict=matrix([measured(c) for c in range(1,28)],'coherent')
assert code==0 and verdict['coherent_cells']==27 and verdict['passed'] is True, verdict
assert all(c['coherent'] is True and c['incoherence']==[] for c in cells)
assert all(r['within_20_percent'] is True for r in verdict['events_axis'] if r['distribution']=='request'), verdict['events_axis']
assert len([r for r in verdict['events_axis'] if r['distribution']=='request'])==9
# The 2026-10-25 run: three paced setup stops and 24 sign-in 429s, nothing measured.
paced=[dict(cell=c,families=1,actual_families=1,concurrency=[1,16,64][c-1],requested_events=248,actual_events=2*c,completion='paced_setup_budget_insufficient',failure_stage='authority_aging',failure_code='none',elapsed_seconds=.6,measurement_seconds=0,pairs=[],request_seconds=[],attempted=dict(empty_pull=0,caught_up_read=0,save_apply=0),successful=dict(empty_pull=0,caught_up_read=0,save_apply=0),errors=0,rate_limits=0,timeouts=0) for c in range(1,4)]
refused=[]
for c in range(4,28):
 r=measured(c);r.update(actual_families=0,completion='setup_or_measurement_refused',failure_stage='owner_sign_in',failure_code='rate_limit',measurement_seconds=0,pairs=[],request_seconds=[],rate_limits=1,attempted=dict(empty_pull=0,caught_up_read=0,save_apply=0),successful=dict(empty_pull=0,caught_up_read=0,save_apply=0));refused.append(r)
code,cells,verdict=matrix(paced+refused,'unmeasured')
assert code!=0 and verdict['coherent_cells']==0 and verdict['passed'] is False, (code,verdict)
assert all(c['coherent'] is False and c['incoherence'] for c in cells)
assert 'not_measured' in cells[0]['incoherence'] and 'rate_limited' in cells[3]['incoherence']
for name,mutate,reason in [
 ('short_history',lambda r:r.update(actual_events=100),'events_below_requested'),
 ('missing_family',lambda r:r.update(actual_families=0),'families_below_requested'),
 ('unaccounted',lambda r:r['attempted'].update(empty_pull=3),'accounting_mismatch'),
 ('lost_pair',lambda r:r.update(pairs=[]),'accounting_mismatch'),
 ('refusal',lambda r:r.update(rate_limits=1),'rate_limited'),
 ('error',lambda r:r.update(errors=1),'errors_or_timeouts'),
]:
 rows=[measured(c) for c in range(1,28)];mutate(rows[4])
 code,cells,verdict=matrix(rows,'mutated-'+name)
 assert code!=0 and verdict['coherent_cells']==26 and reason in cells[4]['incoherence'], (name,cells[4]['incoherence'])
# Latency growth with history is reported per equal families/concurrency pair.
rows=[measured(c,request=.05 if c>18 else .01) for c in range(1,28)]
code,cells,verdict=matrix(rows,'growth')
assert code==0 and not any(r['within_20_percent'] for r in verdict['events_axis'] if r['distribution']=='request')
print('V1 proof: an unmeasured matrix fails; coherence needs full families, history and exact accounting; events-axis p95 growth is reported')
# Exercise the runtime metric merger, including real-valued zero deltas.
samples=root/'metric-window';samples.mkdir()
(samples/'resource-samples.jsonl').write_text('\n'.join(json.dumps(dict(time=t,sample_finished=t+.1,resources=dict(db_connections=14,network_counter_bytes=n,filesystem_counter_bytes=n*2))) for t,n in [(10,100),(11,125)]))
(root/'server-phases.jsonl').write_text(json.dumps(dict(time=10.5,phase='security_fence',elapsed_ns=7000000))+'\n')
(root/'server-resources.jsonl').write_text('\n'.join(json.dumps(dict(time=t,alloc_total_bytes=n,cpu_total_seconds=t,runtime_pool_wait_ns=0,rss_peak_bytes=1000)) for t,n in [(10,100),(11,150)]))
measured=pc.metric_window(samples,dict(wall_started=9,wall_ended=12))
assert measured['resources']['security_fence_wait_seconds']==.007
assert measured['resources']['allocations_bytes']==50 and measured['resources']['pool_acquire_wait_seconds']==0
assert measured['resources']['network_bytes']==25 and measured['resources']['filesystem_bytes']==50
assert measured['resources']['rss_bytes'] is None and measured['unavailable']['rss_bytes']=='getrusage_exposes_peak_rss_only'

# N2: exercise both actual consumers, including an interior reset/time fault.
failures=[]
for variant,values,timestamps in [
 ('interior_reset',[100,10,120],[10,11,12]),
 ('out_of_order',[100,110,120],[10,9.5,12]),
 ('nonfinite_nan',[100,110,120],[10,float('nan'),12]),
 ('nonfinite_infinity',[100,110,120],[10,float('inf'),12]),
 ('measured_zero',[100,100,100],[10,11,12]),
]:
 path=samples/'resource-samples.jsonl'
 path.write_text('\n'.join(json.dumps(dict(time=t,sample_finished=t+.1,resources=dict(network_counter_bytes=n,filesystem_counter_bytes=n))) for t,n in zip(timestamps,values)))
 window=pc.metric_window(samples,dict(wall_started=9,wall_ended=13))
 capture=io.StringIO()
 with contextlib.redirect_stdout(capture):pc.sample_summary(path)
 summary=json.loads(capture.getvalue().split(': ',1)[1])
 for key,counter in [('network_bytes','network_counter_bytes'),('filesystem_bytes','filesystem_counter_bytes')]:
  if variant=='measured_zero':
   if window['resources'][key]!=0 or key in window['unavailable']:failures.append(variant+':metric_window:'+key)
   if summary[counter+'_delta']!=0 or counter+'_unavailable_reason' in summary:failures.append(variant+':sample_summary:'+key)
  else:
   if window['resources'][key] is not None or not window['unavailable'].get(key):failures.append(variant+':metric_window:'+key)
   if summary[counter+'_delta'] is not None or not summary.get(counter+'_unavailable_reason'):failures.append(variant+':sample_summary:'+key)
assert not failures, 'N2 regressions: '+', '.join(failures)
print('N2 proof: both consumers reject interior resets, out-of-order/nonfinite timestamps and preserve measured zero')

for mutation in ['duplicate','incomplete','invalid','missing']:
 bad=json.loads(json.dumps(row))
 if mutation=='duplicate':bad['pairs']*=2
 if mutation=='incomplete':bad['pairs'][0]['complete']=False
 if mutation=='invalid':bad['pairs'][0]['durable_seconds']=-1
 if mutation=='missing':del bad['pairs'][0]['observable_seconds']
 try:pc.validate_row(bad)
 except ValueError:pass
 else:raise AssertionError(mutation+' accepted')
control=root/'control.jsonl';control.write_text('');os.chmod(control,0o600)
ack=root/'control.ack';evidence=root/'evidence.jsonl';log=root/'log'
code="""import pathlib,time,json,sys
root=pathlib.Path(sys.argv[1]);row=json.loads(sys.argv[2])
with (root/'control.jsonl').open('a') as f:f.write(json.dumps(dict(kind='start',cell=1))+chr(10))
while not (root/'control.ack').exists():time.sleep(.005)
(root/'checkpoint-1.json').write_text(json.dumps(row));time.sleep(60)
"""
env=dict(os.environ,BABY_TRACKER_MATRIX_CONTROL=str(control),BABY_TRACKER_MATRIX_ACK=str(ack),BABY_TRACKER_MATRIX_EVIDENCE=str(evidence),BT_SYNC_PERF_TEST_FAULT='matrix_cell_timeout')
with log.open('w') as f:
 result=subprocess.run([sys.executable,pc.__file__,'supervise',str(root),sys.executable,'-c',code,str(root),json.dumps(row)],env=env,stdout=f,timeout=5)
assert result.returncode==124
rows=pc.aggregate(log,root/'cells.json','swift_matrix',{},evidence,root)
assert rows[0]['actual_events']==17 and len(rows[0]['pairs'])==1 and rows[0]['elapsed_seconds']>=1
assert rows[0]['measurement_seconds']==.05 and rows[0]['measurement_interval_complete'] is False
assert rows[0]['successful_qps'] is None and rows[0]['completion']=='cell_forced_timeout'
assert rows[1]['elapsed_seconds'] is None and rows[1]['actual_events'] is None
row['actual_events']=0
(root/'checkpoint-1.json').write_text(json.dumps(row))
assert pc.aggregate(log,root/'unknown-head.json','swift_matrix',{},evidence,root)[0]['actual_events'] is None

# M1: the resource sampler stops at its deadline, not after a fixed count.
sampler_bin=root/'sampler-bin';sampler_bin.mkdir();stub=sampler_bin/'docker';stub.write_text('#!/bin/sh\nexit 1\n');stub.chmod(0o700)
sampler_env=dict(os.environ,PATH=str(sampler_bin)+os.pathsep+os.environ['PATH'])
past=root/'sampler-past.jsonl'
subprocess.run([sys.executable,pc.__file__,'sample-loop','app','db',str(past),str(time.time()-1)],env=sampler_env,timeout=10,check=True)
assert not past.exists(), 'sampler ran after its deadline'
bounded=root/'sampler-bounded.jsonl';began=time.monotonic()
subprocess.run([sys.executable,pc.__file__,'sample-loop','app','db',str(bounded),str(time.time()+2.5)],env=sampler_env,timeout=10,check=True)
spent=time.monotonic()-began
assert spent<6 and 1<=len(bounded.read_text().splitlines())<=4, ('sampler not bounded by time',spent,len(bounded.read_text().splitlines()))
# M2: an image that was never built is not a cleanup failure.
absent=root/'absent-image';absent.mkdir();stub=absent/'docker';stub.write_text('#!/bin/sh\ncase "$1 $2" in "image inspect") exit 1 ;; "image rm") exit 9 ;; esac\nexit 0\n');stub.chmod(0o700)
absent_secrets=root/'baby-tracker-protocol.absent';absent_secrets.mkdir();(absent_secrets/'compose.env').write_text('')
absent_config=root/'absent-cleanup.json';absent_config.write_text(json.dumps(dict(project='baby-tracker-protocol-absent',secrets=str(absent_secrets),override=str(absent_secrets/'override'),image='baby-tracker-protocol-absent:identitytest',source=str(root))))
absent_out=root/'absent-out';absent_out.mkdir()
saved_path=os.environ['PATH'];os.environ['PATH']=str(absent)+os.pathsep+saved_path
try: assert pc.cleanup_worker(absent_config,absent_out,20)==0, 'cleanup failed on an image that was never built'
finally: os.environ['PATH']=saved_path
assert json.loads((absent_out/'cleanup.json').read_text())['secrets_removed'] is True and not absent_secrets.exists()
print('M1/M2 proof: sampler bounded by its deadline; absent image skipped by cleanup')

started=time.monotonic()
try:pc.command([sys.executable,'-c','import time;time.sleep(60)'],.05)
except pc.CommandFailure as e:assert e.code=='command_timeout'
else:raise AssertionError('cleanup command did not time out')
assert time.monotonic()-started<1
# The whole cleanup worker is bounded too, retaining exact private recovery pointers.
secrets=root/'baby-tracker-protocol.fixture';secrets.mkdir()
config=root/'cleanup-input.json';config.write_text(json.dumps(dict(project='baby-tracker-protocol-fixture',secrets=str(secrets),override=str(secrets/'override'),image='baby-tracker-protocol-fixture:identitytest',source=str(root))))
fake=root/'bin';fake.mkdir();docker=fake/'docker';docker.write_text('#!'+sys.executable+'\nimport time;time.sleep(60)\n');docker.chmod(0o700)
os.environ['PATH']=str(fake)+os.pathsep+os.environ['PATH']
assert pc.bounded_cleanup(config,root,.3)==1
assert secrets.exists() and (root/'cleanup-recovery.json').exists()
assert json.loads((root/'cleanup.json').read_text())['cleanup_complete'] is False
print('revision controls: malformed runtime pairs rejected; partial sample/head survive forced timeout with actual elapsed; never-started unknown; slow cleanup group killed/reaped and private recovery information retained')
REVISION_CONTROLS
  exit 0
fi
if [[ ${1:-} == --matrix-preflight-only ]]; then
  python3 - "$PWD/$runner" "$PWD/Packages/BabyTracker" "$root" <<'MATRIX_PREFLIGHT'
import os,pathlib,subprocess,sys
runner,package,root=sys.argv[1],sys.argv[2],pathlib.Path(sys.argv[3])
base={k:v for k,v in os.environ.items() if not k.startswith(('BABY_TRACKER_MATRIX','BABY_TRACKER_E2E'))}
base['BABY_TRACKER_CAPACITY_MATRIX']='1'
command=['bash','-c','source "$1"; run_matrix_child "$2" swift test --filter CapacityMatrixE2ETests/capacityMatrix','preflight',runner,package]
for variant in ['missing_configuration','expired_deadline']:
    env=dict(base)
    if variant=='expired_deadline':
        cert=root/'cert'; cert.write_text('')
        env.update(BABY_TRACKER_MATRIX_PRIVATE=str(root),BABY_TRACKER_E2E_BASE='https://localhost:9',BABY_TRACKER_E2E_INSTANCE='70000000-0000-4000-8000-000000000001',BABY_TRACKER_E2E_CERT=str(cert),BABY_TRACKER_MATRIX_DEADLINE='0',BABY_TRACKER_MATRIX_ROTATION_IP_SECONDS='0.006',BABY_TRACKER_MATRIX_ROTATION_DEVICE_SECONDS='0.06')
    result=subprocess.run(command,env=env,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True,timeout=60)
    if variant=='missing_configuration':
        assert result.returncode!=0
        assert 'capacity-matrix failure: missing_configuration' in result.stdout
        assert 'capacity-matrix start: ' not in result.stdout
    else:
        assert result.returncode==0
        assert result.stdout.count('capacity-matrix cell: ')==27
        assert result.stdout.count('"completion":"global_budget_exhausted"')==27
    print('matrix preflight proof: '+variant+' classified with no network/cell operations')
MATRIX_PREFLIGHT
  exit 0
fi
# Resolve the real compose template only inside an owned synthetic project.
# Config evaluation starts no containers and reads no user dotenv/key files.
config_root="$root/config"
mkdir -p "$config_root/Server" "$config_root/owned/keyrings" "$config_root/owned/mcp"
cp Server/compose.yaml "$config_root/Server/compose.yaml"
printf 'services: {}\n' > "$config_root/owned/override.yaml"
: > "$config_root/owned/compose.env"
cat > "$config_root/.env" <<'HOSTILE_ENV'
BT_MAX_CAREGIVERS=1
BT_MAX_INVITATIONS=2
BT_INVITATION_TTL=1h
BT_MAX_CANONICAL_RECORDS_PER_FAMILY=3
BT_MAX_CHARGED_STORAGE_BYTES_PER_FAMILY=4
BT_IDENTITY_PREVIOUS_KEYS_FILE=/run/keyrings/unrequested.json
BT_SESSION_TOKEN_PREVIOUS_KEYS_FILE=/run/keyrings/unrequested.json
BT_DEVICE_CONTINUITY_PREVIOUS_KEYS_FILE=/run/keyrings/unrequested.json
BT_APPLE_NOTIFICATION_PREVIOUS_KEYS_FILE=/run/keyrings/unrequested.json
BT_INVITATION_PREVIOUS_KEYS_FILE=/run/keyrings/unrequested.json
BT_PAGE_TOKEN_PREVIOUS_KEYS_FILE=/run/keyrings/unrequested.json
BT_KEYRING_DIR=/unrequested/keyrings
BT_SECRETS_DIR=/unrequested/secrets
HOSTILE_ENV
cp "$config_root/.env" "$config_root/Server/.env"
cp "$config_root/.env" "$config_root/inherited.env"
helper="$PWD/Server/scripts/lib/disposable-sync.sh"
for dotenv in default inherited; do
  (
    for name in $(compgen -v BT_); do unset "$name"; done
    unset COMPOSE_ENV_FILES COMPOSE_DISABLE_ENV_FILE
    export BT_DISPOSABLE_PERFORMANCE=1
    export BT_SECRETS_DIR="$config_root/owned" BT_KEYRING_DIR="$config_root/owned/keyrings" BT_MCP_SECRETS_DIR="$config_root/owned/mcp"
    export BT_SERVER_INSTANCE_ID=70000000-0000-4000-8000-000000000001 BT_APPLE_AUDIENCE=synthetic.config.only
    [[ $dotenv != inherited ]] || export COMPOSE_ENV_FILES="$config_root/inherited.env"
    project=baby-sync-perf-config-only
    override="$config_root/owned/override.yaml"
    source "$helper"
    cd "$config_root"
    compose config --format json > "$root/config-$dotenv.json"
  )
  python3 - "$root/config-$dotenv.json" "$config_root/owned" <<'VERIFY_CONFIG'
import json, pathlib, sys
config=json.load(open(sys.argv[1])); app=config['services']['app']; env=app['environment']
expected={'BT_MAX_CAREGIVERS':'10','BT_MAX_INVITATIONS':'10','BT_INVITATION_TTL':'168h',
          'BT_MAX_CANONICAL_RECORDS_PER_FAMILY':'100000','BT_MAX_CHARGED_STORAGE_BYTES_PER_FAMILY':'268435456'}
for key,value in expected.items():
    assert env[key]==value, f'{key}: dotenv replaced default ({env[key]})'
for key in ('IDENTITY','SESSION_TOKEN','DEVICE_CONTINUITY','APPLE_NOTIFICATION','INVITATION','PAGE_TOKEN'):
    assert env[f'BT_{key}_PREVIOUS_KEYS_FILE']=='', f'{key}: unrequested previous-key path'
owned=pathlib.Path(sys.argv[2]).resolve()
for mount in app['volumes']:
    if mount['type']=='bind': assert pathlib.Path(mount['source']).resolve().is_relative_to(owned), 'unowned app bind'
for secret in config['secrets'].values():
    assert pathlib.Path(secret['file']).resolve().is_relative_to(owned), 'unowned secret source'
VERIFY_CONFIG
  echo "performance config proof: $dotenv dotenv ignored; defaults, empty previous-key paths and owned binds preserved"
done
# Exercise the production artifact validator directly, without Docker or a
# duplicate validator. Fixtures contain only synthetic times/counts.
source "$runner"
python3 - "$root" <<'FOREGROUND_FIXTURES'
import json,pathlib,sys
root=pathlib.Path(sys.argv[1])
def fixture(mode):
    sizes={'hint_only_override_120s':3,'offline_backlog':1,'access_renewal':1,'background_engine_pull':1}
    if mode=='all': sizes.update(healthy=200,unavailable_live_fallback=1)
    proofs=['healthy','negative','recovery'] if mode=='all' else ['recovery']
    rows=[];pairs=[]
    for distribution,count in sizes.items():
        duration=20 if distribution=='unavailable_live_fallback' else .4
        rows.append(dict(distribution=distribution,count=count,expected=count,elapsed_seconds=110,
            durable_p95_seconds=duration,observable_p95_seconds=duration,publication_p95_seconds=duration))
        pairs.extend(dict(distribution=distribution,ordinal=i+1,durable_seconds=duration,
            observable_seconds=duration,publication_seconds=duration) for i in range(count))
    return proofs,rows,pairs
for variant in ['valid_all','valid_recovery','missing_marker','wrong_count','lying_p95','slow_p95','wrong_ordinal','wrong_scope']:
    proofs,rows,pairs=fixture('recovery' if variant in ['valid_recovery','wrong_scope'] else 'all')
    if variant=='missing_marker': proofs.remove('healthy')
    if variant=='wrong_count': pairs.pop()
    if variant=='wrong_ordinal': pairs[-1]['ordinal']=2
    if variant=='lying_p95': rows[0]['durable_p95_seconds']=.1
    if variant=='slow_p95':
        for row in rows:
            if row['distribution']=='healthy': row['durable_p95_seconds']=1.1
        for pair in pairs:
            if pair['distribution']=='healthy': pair['durable_seconds']=1.1
    lines=['foreground-latency proof: '+proof+' complete' for proof in proofs]
    lines+=['foreground-latency sample: '+json.dumps(row) for row in rows]
    lines+=['foreground-latency pair: '+json.dumps(pair) for pair in pairs]
    (root/(variant+'.txt')).write_text('\n'.join(lines)+'\n')
FOREGROUND_FIXTURES
for valid in all recovery; do
  validate_foreground_artifacts "$root/valid_$valid.txt" "$root/pairs-$valid.jsonl" "$valid" > "$root/marker-$valid.txt"
  marker='foreground-latency proof: complete'
  [[ $valid == all ]] || marker='foreground-latency proof: recovery-only complete'
  [[ $(cat "$root/marker-$valid.txt") == "$marker" ]] || { echo "wrong scope marker" >&2; exit 1; }
done
for invalid in missing_marker wrong_count lying_p95 slow_p95 wrong_ordinal wrong_scope; do
  if validate_foreground_artifacts "$root/$invalid.txt" "$root/invalid-pairs.jsonl" all > "$root/invalid.log" 2>&1; then
    echo "foreground validator accepted $invalid" >&2; exit 1
  fi
done
for invalid in case mode scope; do
  case $invalid in
    case) arguments=(--mode foreground --foreground-case invalid); expected='invalid foreground case' ;;
    mode) arguments=(--mode invalid); expected='invalid mode' ;;
    scope) arguments=(--mode socket --foreground-case recovery); expected='invalid foreground case' ;;
  esac
  if BT_SYNC_PERF_PROBE=1 bash "$runner" --output "$root/invalid-$invalid" "${arguments[@]}" > "$root/invalid-$invalid.log" 2>&1; then
    echo "accepted invalid mode/case" >&2; exit 1
  fi
  grep -qx "$expected" "$root/invalid-$invalid.log"
  [[ ! -e "$root/invalid-$invalid" ]] || { echo "invalid case created output" >&2; exit 1; }
done
echo "foreground validator proof: exact full/scoped markers; missing proof, count, ordinal, p95 and mode/case failures rejected"
# The runner's matrix report fails a matrix that measured nothing (the
# 2026-10-25 run reported status 0 with zero measured cells).
unmeasured="$root/unmeasured-output"
mkdir -p "$unmeasured/cell-evidence"
for cell in $(seq 1 27); do
  printf 'capacity-matrix cell: {"cell":%d,"families":%d,"concurrency":%d,"requested_events":%d,"actual_events":2,"completion":"setup_or_measurement_refused","elapsed_seconds":0.3,"measurement_seconds":0,"pairs":[],"request_seconds":[],"rate_limits":1,"errors":0,"timeouts":0}\n' \
    "$cell" "$(( (cell - 1) % 9 / 3 == 0 ? 1 : ((cell - 1) % 9 / 3 == 1 ? 32 : 128) ))" "$(( (cell - 1) % 3 == 0 ? 1 : ((cell - 1) % 3 == 1 ? 16 : 64) ))" "$(( (cell - 1) / 9 == 0 ? 248 : ((cell - 1) / 9 == 1 ? 512 : 2048) ))"
done > "$unmeasured/matrix.log"
if report_matrix "$unmeasured" complete > "$root/unmeasured-report.log" 2>&1; then
  echo "runner reported an unmeasured matrix as a success" >&2; exit 1
fi
jq -e '.passed == false and .coherent_cells == 0' "$unmeasured/matrix-verdict.json" >/dev/null || { echo "unmeasured matrix verdict missing" >&2; exit 1; }
echo "matrix report proof: an unmeasured matrix fails the runner"
# The matrix's quotas reach the disposable app's environment, and only its.
printf '%s\n' 'services:' '  db:' '    ports: ["127.0.0.1:0:5432"]' '  app:' '    volumes:' '      - /owned/recovery_database_url:/run/secrets/recovery_database_url:ro' '    environment:' '      BT_IDENTITYTEST_HOSTED_ADAPTER: "0"' '  migrate:' '    build: {}' > "$root/matrix-override.yaml"
apply_matrix_quotas "$root/matrix-override.yaml"
python3 - "$root/matrix-override.yaml" "$(matrix_quota_overrides)" <<'MATRIX_QUOTAS'
import re,sys
# No YAML library: split the override's services by their two-space keys.
services={};current=None
for line in open(sys.argv[1]).read().splitlines():
    match=re.fullmatch(r'  ([a-z]+):',line)
    if match: current=match[1]; services[current]=[]
    elif current: services[current].append(line)
wanted=dict(line.split('=',1) for line in sys.argv[2].splitlines())
app=services['app']
assert '    environment:' in app, app
env=dict(re.fullmatch(r'      ([A-Z_]+): "(.*)"',line).groups() for line in app[app.index('    environment:')+1:])
assert len(wanted)>=10 and all(env.get(k)==v for k,v in wanted.items()), env
assert env['BT_IDENTITYTEST_HOSTED_ADAPTER']=='0'
assert not any('BT_RATE_LIMIT' in line for name in ['db','migrate'] for line in services[name])
MATRIX_QUOTAS
echo "matrix quota proof: the stated quotas reach the disposable app only"
# Task 7: the runner reads the PostgreSQL authority, not the retired
# projection head, the retired cleanup-worker marker or its retired receipts.
# security_audit_events is listed too: with routine refresh writing no audit row
# (decision D1c) it no longer grows per rotation, so it cannot be the head.
retired_status=0
retired=$(grep -n 'security_projection_head\|identitytest recovery cleanup completed\|cleanup_refresh_recovery_receipts\|security_audit_events' "$runner") || retired_status=$?
[[ $retired_status -le 1 ]] || { echo "retired-name guard could not read $runner (grep exit $retired_status)" >&2; exit 1; }
[[ -z $retired ]] || { echo "runner still names a retired authority object: $retired" >&2; exit 1; }
# Both head readers (the matrix reader and the artifact) count consumed sessions.
[[ $(grep -c "FROM sessions WHERE status = 'consumed'" "$runner") == 2 ]] || { echo "runner head is not the consumed-session count" >&2; exit 1; }
# The matrix needs about 55 minutes for 27 cells; the other modes keep 20.
# A relative --output stops the runner after argument validation and before
# it creates anything, so only the budget check decides these.
for budget in matrix_3600_accepted matrix_3601_refused matrix_59_refused socket_3600_refused foreground_3600_refused socket_1200_accepted; do
  case $budget in
    matrix_3600_accepted) arguments=(--mode matrix --total-seconds 3600); want=accepted ;;
    matrix_3601_refused) arguments=(--mode matrix --total-seconds 3601); want=refused ;;
    matrix_59_refused) arguments=(--mode matrix --total-seconds 59); want=refused ;;
    socket_3600_refused) arguments=(--mode socket --total-seconds 3600); want=refused ;;
    foreground_3600_refused) arguments=(--mode foreground --total-seconds 3600); want=refused ;;
    socket_1200_accepted) arguments=(--mode socket --total-seconds 1200); want=accepted ;;
  esac
  if BT_SYNC_PERF_PROBE=1 bash "$runner" --output relative-budget-check "${arguments[@]}" > "$root/$budget.log" 2>&1; then
    echo "budget case $budget got past output validation" >&2; exit 1
  fi
  if [[ $want == refused ]]; then
    grep -qx 'invalid time budget' "$root/$budget.log" || { echo "budget case $budget was not refused" >&2; exit 1; }
  else
    ! grep -qx 'invalid time budget' "$root/$budget.log" || { echo "budget case $budget was refused" >&2; exit 1; }
    grep -q 'fresh absolute output required' "$root/$budget.log" || { echo "budget case $budget did not reach output validation" >&2; exit 1; }
  fi
done
echo "performance budget proof: matrix accepts 60-3600 s, other modes 60-1200 s; no retired authority object in the runner"
python3 - "$runner" "$root" <<'SUPERVISOR_TEST'
import os,pathlib,signal,subprocess,sys,time,json
runner=str(pathlib.Path(sys.argv[1]).resolve()); root=pathlib.Path(sys.argv[2])
for sig in [signal.SIGTERM,signal.SIGINT]:
    pidfile=root/('child-'+str(sig)+'.pid')
    code='import os,time,pathlib; pathlib.Path('+repr(str(pidfile))+').write_text(str(os.getpid())); time.sleep(60)'
    owner=subprocess.Popen(['bash','-c','source "$1"; run_matrix_child "$2" "$3" -c "$4"','owned-supervisor',runner,str(root),sys.executable,code])
    deadline=time.monotonic()+5
    while not pidfile.exists():
        assert time.monotonic()<deadline, 'child did not start'
        time.sleep(.01)
    child=int(pidfile.read_text())
    owner.send_signal(sig)
    assert owner.wait(timeout=5)==124, 'supervisor did not classify interrupt'
    try: os.kill(child,0)
    except ProcessLookupError: pass
    else: raise AssertionError('owned child leaked')
for variant in ['delayed_partial','out_of_order','stale_finish','cleanup_timeout','missing_finish']:
    control=root/(variant+'.control'); control.write_bytes(b''); os.chmod(control,0o600)
    ack=root/(variant+'.ack'); pidfile=root/(variant+'.pid'); log=root/(variant+'.log')
    code="""import json,os,pathlib,time,sys
control,ack,pidfile,variant=map(str,sys.argv[1:])
pathlib.Path(pidfile).write_text(str(os.getpid()))
def send(kind,cell):
 with open(control,'ab',buffering=0) as f:f.write((json.dumps(dict(kind=kind,cell=cell))+'\\n').encode())
def acknowledged(cell):
 end=time.monotonic()+2
 while time.monotonic()<end:
  try:
   if json.loads(pathlib.Path(ack).read_text()).get('cell')==cell:return
  except (FileNotFoundError,ValueError):pass
  time.sleep(.005)
 raise RuntimeError('missing ACK')
if variant=='delayed_partial':
 time.sleep(.1)
 with open(control,'ab',buffering=0) as f:
  f.write(b'{"kind":"start",');time.sleep(.1);f.write(b'"cell":1}\\n')
 acknowledged(1);send('finish',1)
elif variant=='out_of_order':send('start',2);time.sleep(60)
else:
 send('start',1);acknowledged(1)
 if variant=='missing_finish':sys.exit(0)
 send('finish',1)
 print('capacity-matrix cell: '+json.dumps(dict(cell=1,completion='fixture_complete')),flush=True)
 send('start',2);acknowledged(2)
 if variant=='stale_finish':send('finish',1)
 time.sleep(60)
"""
    env=dict(os.environ,BABY_TRACKER_MATRIX_CONTROL=str(control),BABY_TRACKER_MATRIX_ACK=str(ack),BT_SYNC_PERF_TEST_FAULT='matrix_cell_timeout')
    with open(log,'w') as output:
        owner=subprocess.Popen(['bash','-c','source "$1"; run_matrix_child "$2" "$3" -c "$4" "$5" "$6" "$7" "$8"','owned-supervisor',runner,str(root),sys.executable,code,str(control),str(ack),str(pidfile),variant],env=env,stdout=output,stderr=subprocess.STDOUT)
        status=owner.wait(timeout=5)
    assert status==(0 if variant=='delayed_partial' else 1 if variant=='missing_finish' else 124), (variant,status,log.read_text())
    child=int(pidfile.read_text())
    try: os.kill(child,0)
    except ProcessLookupError: pass
    else: raise AssertionError('owned protocol child leaked')
    if variant in ['stale_finish','cleanup_timeout']:assert 'fixture_complete' in log.read_text(), 'completed row lost'
print('matrix supervisor proof: SIGTERM/SIGINT, delayed partial controls, out-of-order/stale finish, missing finish, cleanup timeout and row preservation passed')
SUPERVISOR_TEST
[[ ${1:-} != --config-only ]] || exit 0
# These failures must precede any owned Docker resources or output creation.
for kind in optin external origin destination existing symlink; do
  destination="$root/$kind"
  mkdir -p "$root/existing"
  ln -s "$root/existing" "$root/symlink" 2>/dev/null || true
  case $kind in
    optin) prefix=(env -u BT_SYNC_PERF_PROBE) ;;
    external) prefix=(env BT_SYNC_PERF_PROBE=1 BT_DATABASE_URL=postgres://external/forbidden) ;;
    origin) prefix=(env BT_SYNC_PERF_PROBE=1 BT_PUBLIC_ORIGIN=https://external.invalid) ;;
    destination) destination=https://external/forbidden; prefix=(env BT_SYNC_PERF_PROBE=1) ;;
    *) prefix=(env BT_SYNC_PERF_PROBE=1) ;;
  esac
  if "${prefix[@]}" bash "$runner" --output "$destination" >"$root/$kind.log" 2>&1; then
    echo "preflight accepted $kind" >&2; exit 1
  fi
  case $kind in optin|external|origin) [[ ! -e $destination ]] || { echo "preflight created output" >&2; exit 1; } ;; esac
done
for fault in start_failure interrupt timeout; do
  output="$root/result-$fault"
  if BT_SYNC_PERF_PROBE=1 BT_SYNC_PERF_TEST_FAULT="$fault" bash "$runner" --output "$output" --duration 30 --total-seconds 600 >"$root/$fault.log" 2>&1; then
    echo "failure injection succeeded: $fault" >&2; exit 1
  fi
  fault_failed() { echo "fault $fault: $1 failed" >&2; cat "$output/status.json" "$output/cleanup.json" >&2 || true; exit 1; }
  case $fault in
    interrupt) jq -e '.status == 130' "$output/status.json" >/dev/null || fault_failed status ;;
    timeout) jq -e '.status == 124' "$output/status.json" >/dev/null || fault_failed status ;;
    start_failure) jq -e '.status != 0 and .phase == "app_start"' "$output/status.json" >/dev/null || fault_failed status ;;
  esac
  jq -e '.containers == 0 and .volumes == 0 and .secrets_removed == true' "$output/cleanup.json" >/dev/null || fault_failed cleanup
  [[ ! -s "$output/measurements.jsonl" ]] || { echo "failure emitted measurements" >&2; exit 1; }
  if rg -i 'postgres://|Bearer |identity_token|access_token|refresh_token|device_credential|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' "$output"; then
    echo "unsafe retained artifact" >&2; exit 1
  fi
done
echo "performance runner proof: preflight refusals and actual start/interrupt/timeout cleanup passed"
