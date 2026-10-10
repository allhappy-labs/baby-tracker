import os,unittest,subprocess,sys
from capacity_metrics import process_sample,container_sample,descendant_sample
class Metrics(unittest.TestCase):
 def test_actual_generator_process_and_missing_process(self):
  actual=process_sample(os.getpid())
  self.assertEqual(actual['status'],'available');self.assertGreater(actual['rss_current_bytes'],0);self.assertGreaterEqual(actual['cpu_total_seconds'],0)
  self.assertEqual(process_sample(2147483647)['status'],'unavailable')
 def test_container_missing_or_malformed_is_not_zero(self):
  self.assertEqual(container_sample('{}')['status'],'unavailable')
  row=container_sample('{"CPUPerc":"0.00%","MemUsage":"0B / 1GiB","PIDs":"1"}')
  self.assertEqual(row['status'],'available');self.assertEqual(row['cpu_percent'],0);self.assertEqual(row['memory_usage_bytes'],0)
 def test_owned_descendants_sampled_separately(self):
  child=subprocess.Popen([sys.executable,'-c','import time;time.sleep(20)'])
  try:
   row=descendant_sample(os.getpid())
   self.assertEqual(row['status'],'available');self.assertGreaterEqual(row['process_count'],1);self.assertGreater(row['rss_process_sum_bytes'],0)
  finally:child.terminate();child.wait()
if __name__=='__main__':unittest.main()
