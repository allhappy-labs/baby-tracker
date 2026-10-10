"""Real disposable fixture/run SIGKILL regression. Inputs are private local artifacts."""
import argparse, pathlib, os, sys, signal, subprocess, time, json
sys.path.insert(0,str(pathlib.Path(__file__).parent))
import capacity_control as c
p=argparse.ArgumentParser();p.add_argument('--client',required=True);p.add_argument('--output',required=True);p.add_argument('--fixture');p.add_argument('--image');p.add_argument('--cell',choices=['steady','contention','catch-up','slow-consumer','reconnect'],default='steady');p.add_argument('--only-run',action='store_true');p.add_argument('--early-restore',action='store_true');p.add_argument('--signal',choices=['kill','term'],default='kill');args=p.parse_args()
base=c.safe(args.output,fresh=True);base.mkdir(mode=0o700);private=base/'.private';private.mkdir(mode=0o700);control=private/'control';control.mkdir(mode=0o700)
def killed_child(command,cell,wait_pair=False):
 cell.mkdir(mode=0o700);checkpoint=cell/'cell.json';c.atomic(checkpoint,dict(version=1,completion='setup_failed',measurement_seconds=0,successes=0,passing=False))
 script="import pathlib,sys,time,json;sys.path.insert(0,sys.argv[1]);import capacity_control as c;sys.exit(c.supervise(json.loads(sys.argv[2]),pathlib.Path(sys.argv[3]),pathlib.Path(sys.argv[4]),time.monotonic()+100,time.monotonic()+135))"
 if args.early_restore:os.environ['BT_CAPACITY_TEST_PAUSE_BEFORE_POSTGRES']='1'
 supervisor=subprocess.Popen([sys.executable,'-c',script,str(pathlib.Path(__file__).parent),json.dumps(command),str(control),str(checkpoint)],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 until=time.monotonic()+80;owner=None
 while time.monotonic()<until:
  if supervisor.poll() is not None:raise AssertionError('real child stopped before kill point')
  try:
   child=c.read(control/'child.json')['pid']
   receipts=list(control.glob('resources-*.json'))
   for path in receipts:
    r=c.receipt(path,control)
    if args.early_restore and wait_pair:
     if (cell/'restore').is_dir() and (pathlib.Path(r['root'])/'postgres-password').is_file():owner=r['owner'];break
    elif wait_pair:
     row=c.read(checkpoint)
     if row.get('proof_peer_observed')==1:owner=r['owner'];break
    else:
     existing=c.command(['docker','ps','--all','--quiet','--filter','label='+c.LABEL+'='+r['owner']],5).strip()
     if existing:owner=r['owner'];break
   if owner:os.kill(child,signal.SIGKILL if args.signal=='kill' else signal.SIGTERM);break
  except (OSError,ValueError,KeyError,c.CommandFailure):pass
  time.sleep(.03)
 if not owner:
  supervisor.terminate();supervisor.wait(timeout=40);raise AssertionError('owned kill point not reached')
 status=supervisor.wait(timeout=40)
 if args.early_restore:assert not (cell/'restore').exists(),'early restore material survived while cleanup claimed success'
 assert status!=0,'killed child was promoted to success'
 row=c.read(checkpoint);assert row['completion']=='interrupted' and not row['passing'] and row['percentiles'] is None,row
 assert c.read(private/'cleanup.json')['complete']
 for path in control.glob('resources-*.json'):
  r=c.receipt(path,control)
  assert not pathlib.Path(r['root']).exists()
  if r.get('restore_root'):assert not pathlib.Path(r['restore_root']).exists()
  if r.get('fixture_root') and not r.get('fixture_sealed'):assert not pathlib.Path(r['fixture_root']).exists()
  for kind in ['container','volume','network']:
   cmd=['docker','ps','--all','--quiet'] if kind=='container' else ['docker',kind,'ls','--quiet']
   assert not c.command(cmd+['--filter','label='+c.LABEL+'='+r['owner']],5).strip()
 if wait_pair and not args.early_restore:
  if args.cell=='contention':assert row.get('contention_proof_pairs')==2,row
  if args.cell=='catch-up':assert row.get('backlog_acknowledged')==100 and row.get('catchup_observed')==100,row
  if args.cell=='slow-consumer':assert row.get('slow_consumer_devices')==32,row
  if args.cell=='reconnect':assert row.get('refresh',{}).get('successes')==2 and row.get('refresh_reconnect',{}).get('successes')==2 and row.get('reconnect_attempts')==2,row
 try:os.kill(child,0)
 except ProcessLookupError:pass
 else:raise AssertionError('child not reaped')
 return dict(child_reaped=True,completion=row['completion'],percentiles=None,private_material_removed=True,owned_resources_absent=True,actual_devices=row.get('actual_devices'),proof_peer_observed=row.get('proof_peer_observed'),fixture_receipt_before_creation=True,shape=args.cell,shape_evidence={k:row.get(k) for k in ['contention_proof_pairs','backlog_acknowledged','catchup_observed','slow_consumer_devices','reconnect_attempts','refresh','refresh_reconnect']})
if not args.only_run:
 fixture_destination=base/'fixture-killed'
 proof=killed_child([args.client,'fixture','--destination',str(fixture_destination),'--families','512','--authority-events','248'],base/'fixture-child')
 assert not fixture_destination.exists();c.atomic(base/'fixture-kill-proof.json',proof);print('real fixture child SIGKILL: owned cleanup and incomplete checkpoint verified')
if args.fixture and args.image:
 proof=killed_child([args.client,'run','--fixture',args.fixture,'--output',str(base/'run-child'),'--image',args.image,'--cell',args.cell],base/'run-child',True)
 c.atomic(base/'run-kill-proof.json',proof);print('real run child SIG'+args.signal.upper()+': '+('early restore boundary' if args.early_restore else args.cell+' production pair')+', owned cleanup and incomplete checkpoint verified')
