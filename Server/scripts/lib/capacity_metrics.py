"""Five-second external observations. Missing data is never synthesized as zero."""
import json,math,re,subprocess,time,threading

def process_sample(pid):
 try:
  row=subprocess.check_output(['ps','-p',str(pid),'-o','rss=,time='],text=True,stderr=subprocess.DEVNULL,timeout=2).split()
  if len(row)!=2:raise ValueError()
  parts=row[1].split(':');seconds=sum(float(v)*60**i for i,v in enumerate(reversed(parts)))
  return dict(status='available',source='host ps: capacity client process; outside container budgets',rss_current_bytes=int(row[0])*1024,cpu_total_seconds=seconds)
 except (OSError,ValueError,subprocess.SubprocessError):return dict(status='unavailable',source='host ps: capacity client process')

def descendant_sample(pid):
 try:
  rows=[line.split() for line in subprocess.check_output(['ps','-axo','pid=,ppid=,rss=,time='],text=True,stderr=subprocess.DEVNULL,timeout=2).splitlines()]
  owned={str(pid)}
  while True:
   expanded=owned|{r[0] for r in rows if r[1] in owned}
   if expanded==owned:break
   owned=expanded
  selected=[r for r in rows if r[0] in owned and r[0]!=str(pid)]
  if not selected:return dict(status='unavailable',reason='no live descendants')
  cpu=sum(sum(float(v)*60**i for i,v in enumerate(reversed(r[3].split(':')))) for r in selected)
  return dict(status='available',source='host ps: currently live owned descendants; sums may drop when a child exits; RSS shares may double count',process_count=len(selected),cpu_live_process_sum_seconds=cpu,rss_process_sum_bytes=sum(int(r[2])*1024 for r in selected))
 except (OSError,ValueError,IndexError,subprocess.SubprocessError):return dict(status='unavailable')

def container_sample(raw):
 try:
  row=json.loads(raw);cpu=float(row['CPUPerc'].removesuffix('%'));usage=row['MemUsage'].split('/')[0].strip()
  m=re.fullmatch(r'([0-9.]+)(B|KiB|MiB|GiB|kB|MB|GB)',usage)
  factors={'B':1,'KiB':1024,'MiB':1024**2,'GiB':1024**3,'kB':1000,'MB':1000**2,'GB':1000**3}
  memory=float(m[1])*factors[m[2]]
  if not math.isfinite(cpu) or cpu<0 or not math.isfinite(memory) or memory<0:raise ValueError()
  return dict(status='available',source='Docker stats; memory usage is Docker working-set semantics',cpu_percent=cpu,memory_usage_bytes=int(memory))
 except (KeyError,ValueError,TypeError,AttributeError):return dict(status='unavailable',source='Docker stats')

class Sampler:
 def __init__(self,pid,containers):
  self.pid=pid;self.containers=containers;self.rows=[];self.stop=threading.Event();self.thread=threading.Thread(target=self.run)
 def run(self):
  while not self.stop.is_set() and len(self.rows)<1442:
   row=dict(at_unix=time.time(),generator=process_sample(self.pid),descendants=descendant_sample(self.pid),containers={})
   started=time.monotonic()
   try:
    targets=self.containers()
    if targets:
     raw=subprocess.check_output(['docker','stats','--no-stream','--format','{{json .}}',*[name for _,name in targets]],text=True,stderr=subprocess.DEVNULL,timeout=3)
     observed={json.loads(line)['Name']:line for line in raw.splitlines()}
     row['containers']={role:container_sample(observed.get(name,'{}')) for role,name in targets}
   except (OSError,ValueError,KeyError,subprocess.SubprocessError):row['container_status']='unavailable'
   self.rows.append(row)
   self.stop.wait(max(0,5-(time.monotonic()-started)))
 def start(self):self.thread.start()
 def finish(self):self.stop.set();self.thread.join();return dict(interval_seconds=5,semantics='external sampling across setup, warmup, measurement and teardown; timestamps required for window comparison',samples=self.rows)
