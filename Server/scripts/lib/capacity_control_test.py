import importlib.util, pathlib, unittest, tempfile, os, json, time, subprocess, sys, signal
sys.path.insert(0,str(pathlib.Path(__file__).parent))
import capacity_control as c
REAL_COMMAND=c.command
class Controls(unittest.TestCase):
 def setUp(self):
  self.parent=pathlib.Path(tempfile.mkdtemp(prefix="bt-capacity-control-test-",dir=os.path.realpath(tempfile.gettempdir())))
  self.private=self.parent/".private";self.private.mkdir(mode=0o700);self.control=self.private/"control";self.control.mkdir(mode=0o700)
  self.owner="a"*32;self.root=pathlib.Path(tempfile.mkdtemp(prefix="bt-capacity-"+self.owner+"-",dir=os.path.realpath(tempfile.gettempdir())))
  self.path=self.control/("resources-"+self.owner+".json")
  c.atomic(self.path,dict(version=1,owner=self.owner,root=str(self.root),containers=["bt-capacity-"+self.owner],networks=[]))
 def tearDown(self):
  import shutil
  shutil.rmtree(self.parent,ignore_errors=True);shutil.rmtree(self.root,ignore_errors=True)
 def test_ownership_and_symlinks_refused(self):
  r=c.read(self.path);r["containers"]=["production-db"];c.atomic(self.path,r)
  with self.assertRaises(c.Unsafe):c.cleanup(self.control,time.monotonic()+5,lambda *_:"danger")
  self.path.unlink();self.path.symlink_to(self.parent/"unrelated")
  with self.assertRaises((c.Unsafe,OSError)):c.cleanup(self.control,time.monotonic()+5,lambda *_:"")
 def test_reserved_private_path_symlink_refused(self):
  cell=self.parent/'cell-01';cell.mkdir(mode=0o700)
  external=self.parent/'external';external.mkdir(mode=0o700)
  r=c.read(self.path);r['restore_root']=str(cell/'restore');c.atomic(self.path,r)
  c.receipt(self.path,self.control)
  cell.rmdir();cell.symlink_to(external,target_is_directory=True)
  with self.assertRaises(c.Unsafe):c.cleanup(self.control,time.monotonic()+5,lambda *_:"")
  self.assertTrue(external.exists())
 def test_cleanup_timeout_retains_material(self):
  def timeout(*_):raise c.CommandFailure("cleanup_budget_exhausted")
  with self.assertRaises(c.CommandFailure):c.cleanup(self.control,time.monotonic(),timeout)
  self.assertTrue(self.root.is_dir());self.assertTrue(self.path.is_file())
 def test_torn_result_is_incomplete(self):
  path=self.parent/"cell.json";path.write_text('{"successes":');path.chmod(0o600);c.recover(path,None)
  row=c.read(path);self.assertEqual(row["completion"],"invalid_result");self.assertIsNone(row["percentiles"]);self.assertFalse(row["passing"])

 def test_early_build_failure_keeps_planned_checkpoint(self):
  from unittest.mock import patch
  fixture=self.parent/'fixture';fixture.mkdir(mode=0o700);c.atomic(fixture/'manifest.json',dict(Version=1))
  output=self.parent/'run-output'
  with patch.object(c,'validate_environment'),patch.object(c,'source_identity',return_value='a'*40),patch.object(c,'command',side_effect=c.CommandFailure('command_failed')):
   with self.assertRaises(c.CommandFailure):c.runner(['--profile','standard','--scenario','pilot','--source',str(self.parent),'--fixture',str(fixture),'--output',str(output),'--total-seconds','300'])
  row=c.read(output/'cell-01/cell.json');self.assertEqual(row['completion'],'setup_failed');self.assertEqual(row['measurement_seconds'],0);self.assertFalse(row['passing']);self.assertIsNone(row['percentiles'])
 def test_swift_package_is_selected_source_not_harness(self):
  from unittest.mock import patch
  fixture=self.parent/'fixture';fixture.mkdir(mode=0o700);c.atomic(fixture/'manifest.json',dict(Version=1))
  source=self.parent/'selected-source';(source/'Server').mkdir(parents=True)
  output=self.parent/'swift-run';captured=[]
  def command(args,*_,**__):
   if len(args)>1 and args[1]=='build' and pathlib.Path(args[0]).name=='go':pathlib.Path(args[args.index('-o')+1]).write_bytes(b'controlled-client')
   if args[:4]==['docker','image','inspect','--format']:return 'sha256:'+'b'*64
   if args[:3]==['docker','image','inspect']:return '[{"Architecture":"arm64"}]'
   return '{}'
  def supervise(args,control,checkpoint,*_):
   captured.extend(args);c.atomic(checkpoint,dict(completion='sample_incomplete'));c.atomic(control.parent/'cleanup.json',dict(complete=True));return 0
  with patch.object(c,'validate_environment'),patch.object(c,'source_identity',return_value='a'*40),patch.object(c,'command',side_effect=command),patch.object(c,'supervise',side_effect=supervise):
   self.assertEqual(c.runner(['--profile','standard','--scenario','swift-lifecycle','--source',str(source),'--fixture',str(fixture),'--output',str(output),'--total-seconds','300']),0)
  self.assertEqual(captured[captured.index('--swift-package')+1],str(source/'Packages/BabyTracker'))
 def test_database_profile_is_forwarded_and_recorded(self):
  from unittest.mock import patch
  fixture=self.parent/'fixture';fixture.mkdir(mode=0o700);c.atomic(fixture/'manifest.json',dict(Version=1))
  source=self.parent/'selected-source';(source/'Server').mkdir(parents=True)
  def command(args,*_,**__):
   if len(args)>1 and args[1]=='build' and pathlib.Path(args[0]).name=='go':pathlib.Path(args[args.index('-o')+1]).write_bytes(b'controlled-client')
   if args[:4]==['docker','image','inspect','--format']:return 'sha256:'+'b'*64
   if args[:3]==['docker','image','inspect']:return '[{"Architecture":"arm64"}]'
   return '{}'
  for profile in ['legacy','explicit']:
   output=self.parent/profile;captured=[]
   def supervise(args,control,checkpoint,*_):
    captured.extend(args);c.atomic(checkpoint,dict(completion='sample_incomplete'));c.atomic(control.parent/'cleanup.json',dict(complete=True));return 0
   args=['--profile','standard','--scenario','pilot','--source',str(source),'--fixture',str(fixture),'--output',str(output),'--total-seconds','300']
   if profile=='explicit':args+=['--database-profile','explicit']
   with patch.object(c,'validate_environment'),patch.object(c,'source_identity',return_value='a'*40),patch.object(c,'command',side_effect=command),patch.object(c,'supervise',side_effect=supervise):
    self.assertEqual(c.runner(args),0)
   self.assertIn('--database-profile',captured)
   self.assertEqual(captured[captured.index('--database-profile')+1],profile)
   row=c.read(output/'run.json');self.assertEqual(row['database_profile'],profile)
   self.assertEqual(row['database_pools'],dict(runtime=16,projector=4,maintenance=2,mcp=0,server_max=64,reserved=8) if profile=='explicit' else None)
 def test_killed_child_checkpoint_and_owned_cleanup(self):
  fake=self.parent/"bin";fake.mkdir(mode=0o700);docker=fake/"docker";docker.write_text("#!/bin/sh\nexit 0\n");docker.chmod(0o700)
  checkpoint=self.parent/"cell.json";c.atomic(checkpoint,dict(completion="complete",successes=240,refusals=3,unexpected_errors=2,measurement_seconds=0,passing=True,percentiles={"p95_seconds":0.1}))
  code="import sys,time,pathlib;sys.path.insert(0,sys.argv[1]);import capacity_control as c;sys.exit(c.supervise([sys.executable,'-c','import time; time.sleep(100)'],pathlib.Path(sys.argv[2]),pathlib.Path(sys.argv[3]),time.monotonic()+10,time.monotonic()+15))"
  env=dict(os.environ,PATH=str(fake)+os.pathsep+os.environ['PATH'])
  supervisor=subprocess.Popen([sys.executable,"-c",code,str(pathlib.Path(__file__).parent),str(self.control),str(checkpoint)],env=env,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
  until=time.monotonic()+5
  while not (self.control/"child.json").exists() and time.monotonic()<until:time.sleep(.01)
  pid=c.read(self.control/"child.json")["pid"];os.kill(pid,signal.SIGKILL)
  stdout,stderr=supervisor.communicate(timeout=8);self.assertNotEqual(supervisor.returncode,0,(stdout,stderr))
  row=c.read(checkpoint);self.assertEqual(row["completion"],"interrupted");self.assertEqual(row['refusals'],3);self.assertEqual(row['unexpected_errors'],2);self.assertFalse(row['passing']);self.assertIsNone(row['percentiles']);self.assertFalse(self.root.exists());self.assertTrue(c.read(self.private/"cleanup.json")["complete"])
  with self.assertRaises(ProcessLookupError):os.kill(pid,0)
 def test_normal_child_exit_reaps_owned_descendants(self):
  from unittest.mock import patch
  pidfile=self.parent/'descendant.pid';checkpoint=self.parent/'cell.json'
  c.atomic(checkpoint,dict(version=1,completion='sample_incomplete',successes=0,refusals=0,unexpected_errors=0))
  code="import subprocess,sys,pathlib; p=subprocess.Popen([sys.executable,'-c','import time;time.sleep(30)']);pathlib.Path(sys.argv[1]).write_text(str(p.pid))"
  with patch.object(c,'cleanup',return_value=dict(complete=True)):
   code=c.supervise([sys.executable,'-c',code,str(pidfile)],self.control,checkpoint,time.monotonic()+5,time.monotonic()+10)
  pid=int(pidfile.read_text());alive=True
  try:os.kill(pid,0)
  except ProcessLookupError:alive=False
  if alive:os.kill(pid,signal.SIGKILL)
  self.assertFalse(alive,'owned Swift-like descendant survived exited launcher')

class RunnerControls(unittest.TestCase):
 def setUp(self):
  from unittest.mock import patch
  self.tmp=tempfile.TemporaryDirectory(dir=os.path.realpath(tempfile.gettempdir()));self.root=pathlib.Path(self.tmp.name)
  self.fixture=self.root/'fixture';self.fixture.mkdir(mode=0o700);c.atomic(self.fixture/'manifest.json',dict(source_sha='f'*40))
  self.source=self.root/'source';(self.source/'Server').mkdir(parents=True)
  self.calls=[];self.output=self.root/'output';self.command_calls=[]
  self.patches=[patch.object(c,'validate_environment'),patch.object(c,'source_identity',return_value='a'*40),patch.object(c,'command',side_effect=self.command),patch.object(c,'supervise',side_effect=self.supervise)]
  for p in self.patches:p.start()
 def tearDown(self):
  for p in reversed(self.patches):p.stop()
  self.tmp.cleanup()
 def command(self,args,*_,**__):
  self.command_calls.append(args)
  if len(args)>1 and args[1]=='build' and pathlib.Path(args[0]).name=='go':pathlib.Path(args[args.index('-o')+1]).write_bytes(b'controlled-client')
  if args[:4]==['docker','image','inspect','--format']:return 'sha256:'+'b'*64
  if args[:3]==['docker','image','inspect']:return '[{"Architecture":"arm64"}]'
  return '{}'
 def supervise(self,args,control,checkpoint,*_,**__):
  devices=int(args[args.index('--devices')+1]);rate=int(args[args.index('--rate')+1]);self.calls.append((devices,rate))
  passing=self.anchor_passes and rate==15
  c.atomic(checkpoint,dict(completion='complete' if passing else 'sample_incomplete',passing=passing,successes=devices//2*rate,peer_applied=devices//2*rate,planned_writes=devices//2*rate,actual_devices=devices,actual_writers=devices//2,measurement_seconds=60,requested=dict(devices=devices,active_writers=devices//2,writes_per_writer_per_minute=rate,measure_ns=60_000_000_000),scenario='steady',product_sha='a'*40,profile='standard',database_profile='legacy',refusal_classification='quota_429_and_typed_server_busy_v1'))
  c.atomic(control.parent/'cleanup.json',dict(complete=True));return 0
 def run_sweep(self):
  return c.runner(['--profile','standard','--scenario','sweep','--source',str(self.source),'--fixture',str(self.fixture),'--output',str(self.output),'--total-seconds','1800'])
 def test_failed_anchor_blocks_expansion_and_shapes(self):
  self.anchor_passes=False;self.run_sweep()
  self.assertEqual(self.calls,[(32,1),(32,15)])
  self.assertEqual(c.read(self.output/'cell-04/cell.json')['completion'],'gated')
  self.assertEqual(c.read(self.output/'cell-10/cell.json')['planned_cell']['active_writers'],2)
 def test_accepted_anchor_allows_expansion_after_low_sample_diagnostic(self):
  self.anchor_passes=True;self.run_sweep()
  self.assertIn((128,15),self.calls);self.assertIn((512,15),self.calls)
 def test_cell_interrupt_is_terminal_for_sweep(self):
  for sig in [signal.SIGINT,signal.SIGTERM]:
   with self.subTest(signal=sig):
    self.output=self.root/str(sig);self.calls=[]
    def stop(args,control,checkpoint,*_,**__):
     self.calls.append(args);os.kill(os.getpid(),sig)
     c.atomic(checkpoint,dict(completion='interrupted',passing=False));c.atomic(control.parent/'cleanup.json',dict(complete=True));return 1
    from unittest.mock import patch
    previous=signal.signal(sig,lambda *_:None)
    try:
     with patch.object(c,'supervise',side_effect=stop):self.assertEqual(self.run_sweep(),1)
    finally:signal.signal(sig,previous)
    self.assertEqual(len(self.calls),1,'interrupted sweep started another cell')
    self.assertEqual(c.read(self.output/'cell-02/cell.json')['completion'],'interrupted')
 def test_receipt_attests_harness_separately_from_fixture_and_product(self):
  self.anchor_passes=False;self.run_sweep();row=c.read(self.output/'run.json')
  self.assertIn('harness',row,'active harness missing from reusable receipt')

class ExtraRunnerControls(RunnerControls):
 def test_controlled_build_signal_reaps_child_and_marks_all_rows(self):
  from unittest.mock import patch
  import threading
  for sig in (signal.SIGINT,signal.SIGTERM):
   with self.subTest(signal=sig):
    self.output=self.root/('build-'+str(sig));pidfile=self.root/('pid-'+str(sig));signals=[]
    def interrupt():
     until=time.monotonic()+3
     while not pidfile.exists() and time.monotonic()<until:time.sleep(.01)
     if pidfile.exists():os.kill(os.getpid(),sig);signals.append(sig)
    def controlled(args,timeout,**kw):
     if len(args)>1 and args[1]=='build' and pathlib.Path(args[0]).name=='go':
      return REAL_COMMAND([sys.executable,'-c','import os,pathlib,time,sys;pathlib.Path(sys.argv[1]).write_text(str(os.getpid()));time.sleep(30)',str(pidfile)],3)
     return self.command(args,timeout,**kw)
    thread=threading.Thread(target=interrupt);thread.start()
    with patch.object(c,'command',side_effect=controlled):self.assertEqual(self.run_sweep(),1)
    thread.join(timeout=4);self.assertEqual(signals,[sig]);self.assertFalse(self.calls)
    with self.assertRaises(ProcessLookupError):os.kill(int(pidfile.read_text()),0)
    self.assertEqual(c.read(self.output/'status.json')['completion'],'interrupted')
    for row in self.output.glob('cell-*/cell.json'):self.assertEqual(c.read(row)['completion'],'interrupted')
    self.assertTrue(c.read(self.output/'.private/cleanup.json')['complete'])
 def test_binary_drift_revokes_provenance_before_second_dispatch(self):
  from unittest.mock import patch
  self.anchor_passes=True
  def mutate(args,control,checkpoint,*rest,**kw):
   result=self.supervise(args,control,checkpoint,*rest,**kw);pathlib.Path(args[0]).write_bytes(b'changed');return result
  with patch.object(c,'supervise',side_effect=mutate):
   with self.assertRaisesRegex(c.Unsafe,'client_binary_changed'):self.run_sweep()
  self.assertEqual(len(self.calls),1);self.assertFalse(c.read(self.output/'run.json')['provenance_verified']);self.assertEqual(c.read(self.output/'cell-01/cell.json')['completion'],'invalid_result')
 def test_harness_drift_during_build_blocks_every_cell(self):
  from unittest.mock import patch
  identity={'revision':'b'*40,'input_sha256':'c'*64};changed=dict(identity,input_sha256='d'*64)
  with patch.object(c,'harness_identity',side_effect=[identity,identity,changed]):
   with self.assertRaisesRegex(c.Unsafe,'harness_changed'):self.run_sweep()
  self.assertFalse(self.calls);self.assertFalse(c.read(self.output/'run.json')['provenance_verified'])
 def test_different_clean_product_and_harness_are_retained(self):
  from unittest.mock import patch
  self.anchor_passes=False
  with patch.object(c,'source_identity',side_effect=lambda path:'a'*40 if str(path)==str(self.source) else 'b'*40):self.run_sweep()
  row=c.read(self.output/'run.json');self.assertEqual(row['product_sha'],'a'*40);self.assertEqual(row['harness']['revision'],'b'*40)
  self.assertEqual(row['fixture_manifest']['source_sha'],'f'*40)
  self.assertEqual(row['client_binary_sha256'],c.file_digest(self.output/'.private/capacity-client'))
  self.assertIn('capacity_metrics.py',row['harness']['loaded_python_inputs']);self.assertIn('client_build',row)
 def test_cleanup_failure_stops_expansion(self):
  from unittest.mock import patch
  self.anchor_passes=True
  def failed(args,control,checkpoint,*rest,**kw):
   self.supervise(args,control,checkpoint,*rest,**kw);c.atomic(control.parent/'cleanup.json',dict(complete=False));return 1
  with patch.object(c,'supervise',side_effect=failed):
   with self.assertRaisesRegex(c.Unsafe,'cleanup_failed'):self.run_sweep()
  self.assertEqual(self.calls,[(32,1)])
 def test_mismatched_anchor_does_not_expand(self):
  from unittest.mock import patch
  self.anchor_passes=True
  def mismatch(args,control,checkpoint,*rest,**kw):
   self.supervise(args,control,checkpoint,*rest,**kw);row=c.read(checkpoint);row['requested']['devices']=512;c.atomic(checkpoint,row);return 0
  with patch.object(c,'supervise',side_effect=mismatch):self.run_sweep()
  self.assertEqual(self.calls,[(32,1),(32,15)])

class MatchedMCPControls(RunnerControls):
 def run_matched(self,name,*extra):
  self.output=self.root/name;self.captured=[]
  def capture(args,control,checkpoint,*_,**__):
   self.captured.append(args);c.atomic(checkpoint,dict(completion='complete',passing=False));c.atomic(control.parent/'cleanup.json',dict(complete=True));return 0
  from unittest.mock import patch
  with patch.object(c,'supervise',side_effect=capture):
   return c.runner(['--profile','standard','--scenario','matched','--metrics','on','--source',str(self.source),'--fixture',str(self.fixture),'--output',str(self.output),'--total-seconds','900',*extra])
 def flag(self,args,name):return args[args.index(name)+1]
 def test_matched_cells_forward_one_window_and_mcp_shape(self):
  cells={'sync-only':['--database-profile','explicit','--measure-seconds','300'],
         'mcp-idle':['--database-profile','explicit-mcp','--measure-seconds','300','--mcp-mode','idle'],
         'mcp-read':['--database-profile','explicit-mcp','--measure-seconds','300','--mcp-mode','read','--mcp-clients','4','--mcp-rate','20'],
         'mcp-abuse-2':['--database-profile','explicit-mcp-2','--measure-seconds','300','--mcp-mode','abusive','--mcp-clients','4','--mcp-abuse-concurrency','8']}
  for name,extra in cells.items():
   with self.subTest(cell=name):
    self.assertEqual(self.run_matched(name,*extra),0)
    self.assertEqual(len(self.captured),1,'matched scenario runs exactly one cell');args=self.captured[0]
    self.assertEqual((self.flag(args,'--devices'),self.flag(args,'--rate'),self.flag(args,'--measure-seconds'),self.flag(args,'--cell')),('32','15','300','steady'))
    self.assertEqual(self.flag(args,'--database-profile'),extra[1])
    run=c.read(self.output/'run.json')
    if name=='sync-only':
     self.assertNotIn('--mcp-mode',args);self.assertIsNone(run['mcp'])
     self.assertEqual(run['database_pools']['mcp'],0)
    else:
     self.assertEqual(self.flag(args,'--mcp-mode'),extra[extra.index('--mcp-mode')+1])
     self.assertEqual(run['mcp']['mode'],self.flag(args,'--mcp-mode'))
     expected=2 if name=='mcp-abuse-2' else 4
     self.assertEqual(run['database_pools']['mcp'],expected);self.assertEqual(run['mcp']['admission_max'],expected)
     for flag in ['--mcp-clients','--mcp-rate','--mcp-abuse-concurrency']:self.assertIn(flag,args)
    self.assertEqual(run['measure_seconds'],300);self.assertEqual(run['rate'],15)
 def test_matched_rate_is_forwarded(self):
  self.assertEqual(self.run_matched('rate4','--database-profile','explicit','--measure-seconds','300','--rate','4'),0)
  self.assertEqual(self.flag(self.captured[0],'--rate'),'4')
 def test_mismatched_mcp_options_are_refused_before_any_dispatch(self):
  bad={'mode-without-pool':['--database-profile','explicit','--measure-seconds','300','--mcp-mode','idle'],
       'pool-without-mode':['--database-profile','explicit-mcp','--measure-seconds','300'],
       'clients-without-mode':['--database-profile','explicit','--measure-seconds','300','--mcp-clients','4'],
       'window-too-short':['--database-profile','explicit','--measure-seconds','59'],
       'window-too-long':['--database-profile','explicit','--measure-seconds','901'],
       'missing-window':['--database-profile','explicit'],
       'rate-out-of-range':['--database-profile','explicit','--measure-seconds','300','--rate','61']}
  for name,extra in bad.items():
   with self.subTest(case=name):
    with self.assertRaises(c.Unsafe):self.run_matched(name,*extra)
    self.assertFalse(self.captured);self.assertFalse(self.command_calls,'refused options must not build or dispatch')
 def test_other_scenarios_refuse_matched_only_options(self):
  self.output=self.root/'pilot-mcp'
  with self.assertRaises(c.Unsafe):
   c.runner(['--profile','standard','--scenario','pilot','--database-profile','explicit-mcp','--mcp-mode','idle','--source',str(self.source),'--fixture',str(self.fixture),'--output',str(self.output),'--total-seconds','300'])
  self.output=self.root/'pilot-window'
  with self.assertRaises(c.Unsafe):
   c.runner(['--profile','standard','--scenario','pilot','--measure-seconds','300','--source',str(self.source),'--fixture',str(self.fixture),'--output',str(self.output),'--total-seconds','300'])
  self.assertFalse(self.calls)

class HarnessSourceControls(unittest.TestCase):
 def test_real_clean_revisions_dirty_and_input_changes(self):
  with tempfile.TemporaryDirectory(dir=os.path.realpath(tempfile.gettempdir())) as tmp:
   roots=[pathlib.Path(tmp)/name for name in ['product','harness']];identities=[]
   for n,root in enumerate(roots):
    root.mkdir();(root/'Server/scripts/lib').mkdir(parents=True)
    (root/'Server/scripts/lib/helper.py').write_text('value = '+str(n)+'\n')
    REAL_COMMAND(['git','init','-q',str(root)],5)
    REAL_COMMAND(['git','-C',str(root),'add','.'],5)
    REAL_COMMAND(['git','-C',str(root),'-c','user.name=Control','-c','user.email=control@example.test','-c','commit.gpgsign=false','commit','-qm','test: fixture'],5)
    identities.append(c.harness_identity(root))
   self.assertNotEqual(identities[0]['revision'],identities[1]['revision']);self.assertNotEqual(identities[0]['input_sha256'],identities[1]['input_sha256'])
   helper=roots[1]/'Server/scripts/lib/helper.py';helper.write_text('changed = True\n')
   with self.assertRaisesRegex(c.Unsafe,'clean_source_required'):c.harness_identity(roots[1])
   self.assertNotEqual(c.input_identity(roots[1])['input_sha256'],identities[1]['input_sha256'])


if __name__=="__main__":unittest.main()
