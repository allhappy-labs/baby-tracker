package scripts_test

// Each test builds a fake bin directory whose docker/sysctl/ps/df/orb print
// the canned output named by environment variables and append their
// arguments to $FAKE_LOG, then runs load-node.sh with PATH=fake:/usr/bin:/bin
// under /bin/bash (3.2), so the runner stays portable to the stock shell.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestIdleCheckRefusesForeignHeavyProcess(t *testing.T) {
	onEachHost(t, func(t *testing.T, host string) {
		env := fakeHost(t, on(host, fakeHostOptions{LoadAvg: "{ 0.80 0.90 1.00 }", PS: " 95.0 swift-frontend\n  3.0 Terminal\n", DFAvailGB: 120}))
		out, code := runLoadNode(t, env, "preflight", "--families", "10000")
		if code == 0 || !strings.Contains(out, "machine not idle: swift-frontend") {
			t.Fatalf("exit %d, output %q; want refusal naming swift-frontend", code, out)
		}
	})
}

func TestIdleCheckRefusesHighLoadAverage(t *testing.T) {
	onEachHost(t, func(t *testing.T, host string) {
		env := fakeHost(t, on(host, fakeHostOptions{LoadAvg: "{ 4.50 3.20 2.00 }", PS: "  2.0 Terminal\n", DFAvailGB: 120}))
		out, code := runLoadNode(t, env, "preflight", "--families", "10000")
		if code == 0 || !strings.Contains(out, "load average 4.50 >= 3.00") {
			t.Fatalf("exit %d, output %q", code, out)
		}
		env = fakeHost(t, on(host, fakeHostOptions{LoadAvg: "{ 1.50 4.20 2.00 }", PS: "  2.0 Terminal\n", DFAvailGB: 120}))
		out, code = runLoadNode(t, env, "preflight", "--families", "10000")
		if code == 0 || !strings.Contains(out, "5-minute load average 4.20 >= 4.00") {
			t.Fatalf("5-minute: exit %d, output %q", code, out)
		}
	})
}

func TestPreflightRefusesLowDiskFor10k(t *testing.T) {
	onEachHost(t, func(t *testing.T, host string) {
		env := fakeHost(t, on(host, fakeHostOptions{LoadAvg: "{ 0.5 0.5 0.5 }", PS: "  1.0 Terminal\n", DFAvailGB: 25}))
		out, code := runLoadNode(t, env, "preflight", "--families", "10000")
		if code == 0 || !strings.Contains(out, "free disk 25 GB < 30 GB") {
			t.Fatalf("exit %d, output %q", code, out)
		}
	})
}

func TestLockIsExclusive(t *testing.T) {
	env := fakeHost(t, idleHost())
	lock := filepath.Join(env.TempDir, "bt-load.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lock, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil { // a live pid
		t.Fatal(err)
	}
	out, code := runLoadNode(t, env.With("BT_LOAD_LOCK", lock), "preflight", "--families", "100")
	if code == 0 || !strings.Contains(out, "another load run holds") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func TestCapsVerificationRefusesMismatch(t *testing.T) {
	// docker inspect prints app NanoCpus 2000000000: refuse.
	env := fakeHost(t, idleHost()).With("FAKE_INSPECT_APP", "2000000000 8589934592 8589934592").
		With("FAKE_INSPECT_DB", "4000000000 8589934592 8589934592")
	out, code := runLoadNode(t, env, "verify-caps", "--run", env.Run)
	if code == 0 || !strings.Contains(out, "app NanoCpus 2000000000 != 4000000000") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func TestBurnBoundRejectsUncappedCPU(t *testing.T) {
	env := fakeHost(t, idleHost()).With("FAKE_BURN_CPU", "612.40%")
	out, code := runLoadNode(t, env, "verify-caps", "--run", env.Run)
	if code == 0 || !strings.Contains(out, "burn container used 612.40% > 420%") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func TestHostSamplesFlagContamination(t *testing.T) {
	// 7 samples (70 s) where a foreign process averages 80% CPU: contaminated.
	samples := strings.Repeat(`{"at":"…","load1":2.1,"top":[{"pcpu":80.0,"comm":"swift-frontend"}]}`+"\n", 7)
	out, code := runEvaluateHost(t, samples)
	if code != 0 || !strings.Contains(out, `"contaminated":true`) || !strings.Contains(out, "swift-frontend") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func TestComposeOverlayDeclaresTheReferenceNode(t *testing.T) {
	raw, err := os.ReadFile("../deploy/load/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cpus: 4", "mem_limit: 8g", "memswap_limit: 8g", "shared_preload_libraries=pg_stat_statements",
		"pg_stat_statements.track_planning=on", "track_io_timing=on", "track_wal_io_timing=on", "BT_TRUSTED_PROXY_CIDRS: 172.31.250.100/32", "BT_IDENTITYTEST_APNS_SINK: \"1\"", "subnet: 172.31.250.0/24"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("overlay lacks %q", want)
		}
	}
	if strings.Contains(sectionOf(string(raw), "loadgen"), "cpus:") {
		t.Error("the generator must not be capped")
	}
	// The seeder refuses without the stack's names (cmd/baby-tracker-load
	// loadConfig), so the generator gets the values the app gets.
	for _, want := range []string{"BT_PUBLIC_ORIGIN: ${BT_PUBLIC_ORIGIN:?}", "BT_SERVER_INSTANCE_ID: ${BT_SERVER_INSTANCE_ID:?}",
		"BT_APPLE_AUDIENCE: ${BT_APPLE_AUDIENCE:?}", "BT_RECOVERY_EPOCH: ${BT_RECOVERY_EPOCH:?}"} {
		if !strings.Contains(sectionOf(string(raw), "loadgen"), want) {
			t.Errorf("the generator lacks %q", want)
		}
	}
	// Both capped services carry the caps, not just one of them.
	for _, service := range []string{"app", "db"} {
		section := sectionOf(string(raw), service)
		for _, want := range []string{"cpus: 4", "mem_limit: 8g", "memswap_limit: 8g"} {
			if !strings.Contains(section, want) {
				t.Errorf("%s lacks %q", service, want)
			}
		}
	}
	// The app and the generator read the run's 0600 secrets and write its
	// 0700 directory: they run as the user load-node.sh names (the invoking
	// user on Linux, where bind mounts keep their real owner).
	for _, service := range []string{"app", "loadgen"} {
		if !strings.Contains(sectionOf(string(raw), service), `user: "${BT_LOAD_CONTAINER_USER:-65532:65532}"`) {
			t.Errorf("%s does not run as BT_LOAD_CONTAINER_USER", service)
		}
	}
	for _, capped := range []string{"mem_limit", "memswap_limit", "cpu_quota", "deploy:"} {
		if strings.Contains(sectionOf(string(raw), "loadgen"), capped) {
			t.Errorf("the generator must not be capped (%s)", capped)
		}
	}
}

// Beyond the brief's list: the positive paths and the discipline around them.

func TestPreflightPassesOnAnIdleHostAndWritesIdleJSON(t *testing.T) {
	onEachHost(t, testPreflightPassesOnAnIdleHost)
}

func testPreflightPassesOnAnIdleHost(t *testing.T, host string) {
	env := fakeHost(t, on(host, idleHost())).With("FAKE_DOCKER_NAMES", "buildx_buildkit_other\nbt-load-db-1\n")
	out, code := runLoadNode(t, env, "preflight", "--families", "100")
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	var idle struct {
		Idle            bool              `json:"idle"`
		Families        int               `json:"families"`
		Samples         []json.RawMessage `json:"samples"`
		OtherContainers []string          `json:"other_containers"`
		Disk            struct {
			AvailGB int `json:"avail_gb"`
			NeedGB  int `json:"need_gb"`
		} `json:"disk"`
	}
	readJSON(t, filepath.Join(env.Run, "idle.json"), &idle)
	if !idle.Idle || idle.Families != 100 || len(idle.Samples) != 1 || idle.Disk.NeedGB != 10 || idle.Disk.AvailGB != 120 {
		t.Fatalf("idle.json = %+v", idle)
	}
	if len(idle.OtherContainers) != 1 || idle.OtherContainers[0] != "buildx_buildkit_other" {
		t.Fatalf("other containers = %q; want only the foreign one", idle.OtherContainers)
	}
	if !strings.Contains(out, "other containers running: buildx_buildkit_other") {
		t.Fatalf("output %q does not note the other project's container", out)
	}
	// The lock now names this run and carries a lease beyond this process.
	lock := env.Get("BT_LOAD_LOCK")
	if run, _ := os.ReadFile(filepath.Join(lock, "run")); strings.TrimSpace(string(run)) != env.Run {
		t.Fatalf("lock names run %q, want %q", run, env.Run)
	}
	expires, _ := os.ReadFile(filepath.Join(lock, "expires"))
	if at, err := strconv.ParseInt(strings.TrimSpace(string(expires)), 10, 64); err != nil || at < time.Now().Add(3*time.Hour).Unix() {
		t.Fatalf("lock lease %q: %v", expires, err)
	}
}

func TestIdleCheckRefusesAForeignBuildOrTestCommand(t *testing.T) {
	onEachHost(t, func(t *testing.T, host string) {
		for command, rule := range map[string]string{
			"4242  0.0 /usr/local/go/bin/go test -count=1 ./internal/sync":  "go test",
			"4243  1.2 xcodebuild test -scheme BabyTracker":                 "xcodebuild",
			"4244  0.0 bash Server/scripts/test-db.sh":                      "test-db.sh",
			"4245  0.3 docker compose -p other build app":                   "docker build",
			"4246 88.0 /var/folders/x/T/go-build123/b001/sync.test -test.v": "go-build",
		} {
			env := fakeHost(t, on(host, idleHost())).With("FAKE_PS_ARGS", "    1  0.0 /sbin/launchd\n"+command+"\n")
			out, code := runLoadNode(t, env, "preflight", "--families", "100")
			if code == 0 || !strings.Contains(out, "machine not idle: running "+rule+" (1 processes)") {
				t.Fatalf("%q: exit %d, output %q", command, code, out)
			}
		}
		// The runner's own invocation and its own Compose project are not foreign.
		env := fakeHost(t, on(host, idleHost())).With("FAKE_PS_ARGS",
			"5000 0.1 bash Server/scripts/load-node.sh run --run "+loadRoot(host)+"/x\n"+
				"5001 0.2 docker compose --project-name bt-load -f a run --rm loadgen run --host sync.load.test\n")
		if out, code := runLoadNode(t, env, "preflight", "--families", "100"); code != 0 {
			t.Fatalf("own commands counted as foreign: exit %d, output %q", code, out)
		}
	})
}

// A build process well below the 50% "hot" bar still means another session
// is building: the foreign clause refuses it on its own.
func TestIdleCheckRefusesAModerateForeignProcess(t *testing.T) {
	onEachHost(t, func(t *testing.T, host string) {
		env := fakeHost(t, on(host, fakeHostOptions{LoadAvg: "{ 1.00 1.00 1.00 }", DFAvailGB: 120,
			PS: " 20.0 /Applications/Xcode.app/Contents/Developer/Toolchains/XcodeDefault.xctoolchain/usr/bin/swift-frontend\n  2.0 Terminal\n"}))
		out, code := runLoadNode(t, env, "preflight", "--families", "100")
		if code == 0 || !strings.Contains(out, "machine not idle: swift-frontend") || strings.Contains(out, "above 50% CPU") {
			t.Fatalf("exit %d, output %q", code, out)
		}
	})
}

func TestIdleCheckRefusesABusyForeignContainer(t *testing.T) {
	onEachHost(t, func(t *testing.T, host string) {
		env := fakeHost(t, on(host, idleHost())).With("FAKE_DOCKER_STATS", "other-db-1 180.50%\nbt-load-db-1 90.00%\n")
		out, code := runLoadNode(t, env, "preflight", "--families", "100")
		if code == 0 || !strings.Contains(out, "machine not idle: container other-db-1 at 180.50% CPU") || strings.Contains(out, "bt-load-db-1 at") {
			t.Fatalf("exit %d, output %q", code, out)
		}
	})
}

func TestIdleCheckIgnoresAnIdleSimulatorAndOrbStack(t *testing.T) {
	env := fakeHost(t, fakeHostOptions{LoadAvg: "{ 1.00 1.00 1.00 }", DFAvailGB: 120,
		PS: " 70.0 /Applications/OrbStack.app/Contents/Frameworks/OrbStack Helper.app/Contents/MacOS/OrbStack Helper\n" +
			"  0.4 /Applications/Xcode.app/Contents/Developer/Applications/Simulator.app/Contents/MacOS/Simulator\n"})
	if out, code := runLoadNode(t, env, "preflight", "--families", "100"); code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	busy := fakeHost(t, fakeHostOptions{LoadAvg: "{ 1.00 1.00 1.00 }", DFAvailGB: 120,
		PS: " 97.5 /System/Library/PrivateFrameworks/AppStoreDaemon.framework/Support/appstoreagent\n"})
	out, code := runLoadNode(t, busy, "preflight", "--families", "100")
	if code == 0 || !strings.Contains(out, "machine not idle: appstoreagent above 50% CPU") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

// On Linux the measured stack's processes are host processes. Like the Mac,
// where they hide inside OrbStack's VM, the samples leave them out: a
// process whose cgroup is a Docker container's, the Docker engine, and the
// kernel's softirq and worker threads. Another project's containers are
// still judged through docker stats, and a busy host process still counts.
func TestLinuxIdleCheckLeavesOutContainersTheEngineAndKernelWorkers(t *testing.T) {
	stack := " 99.0 postgres\n 97.0 baby-tracker-se\n 85.0 dockerd\n 70.0 containerd-shim\n 65.0 ksoftirqd/3\n" +
		" 60.0 kworker/u24:1-events_unbound\n 55.0 docker-proxy\n  2.0 bash\n"
	env := fakeHost(t, on("linux", fakeHostOptions{LoadAvg: "{ 1.00 1.00 1.00 }", DFAvailGB: 120, PS: stack})).
		With("FAKE_PS_ARGS", "2001  0.0 /usr/local/go/bin/go test ./x\n")
	inContainer(t, env, 2001) // postgres, and the go test of another project's CI container
	inContainer(t, env, 2002) // the app
	out, code := runLoadNode(t, env, "preflight", "--families", "100")
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	var idle struct {
		Samples []struct {
			Top []struct {
				PCPU float64 `json:"pcpu"`
				Comm string  `json:"comm"`
			} `json:"top"`
			ForeignCommands []json.RawMessage `json:"foreign_commands"`
		} `json:"samples"`
	}
	readJSON(t, filepath.Join(env.Run, "idle.json"), &idle)
	if len(idle.Samples) != 1 || len(idle.Samples[0].ForeignCommands) != 0 {
		t.Fatalf("idle.json samples = %+v", idle.Samples)
	}
	for _, process := range idle.Samples[0].Top {
		if process.Comm == "postgres" || process.Comm == "baby-tracker-se" {
			t.Fatalf("a container's process was sampled: %+v", idle.Samples[0].Top)
		}
	}
	if top := idle.Samples[0].Top; len(top) != 6 || top[0].Comm != "dockerd" || top[0].PCPU != 85 {
		t.Fatalf("top = %+v; want the six host processes, busiest first", top)
	}
	// The same postgres outside a container, and a go test on the host, are
	// another session's.
	env = fakeHost(t, on("linux", fakeHostOptions{LoadAvg: "{ 1.00 1.00 1.00 }", DFAvailGB: 120, PS: stack})).
		With("FAKE_PS_ARGS", "3001  0.0 /usr/local/go/bin/go test ./x\n")
	out, code = runLoadNode(t, env, "preflight", "--families", "100")
	if code == 0 || !strings.Contains(out, "machine not idle: postgres above 50% CPU in 3 of 3 samples") ||
		!strings.Contains(out, "machine not idle: running go test (1 processes)") || strings.Contains(out, "dockerd above") ||
		strings.Contains(out, "kworker") || strings.Contains(out, "ksoftirqd") || strings.Contains(out, "docker-proxy above") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

// On Linux, GNU ps's %cpu is a lifetime average: the process CPU comes from
// top's second frame (the fake's first frame counts nothing), over
// BT_LOAD_CPU_WINDOW seconds (2 by default), and the runner calls none of
// the Mac's tools.
func TestLinuxProcessCPUIsTopsWindowedRate(t *testing.T) {
	hot := " 97.5 node\n  2.0 bash\n"
	env := fakeHost(t, on("linux", fakeHostOptions{LoadAvg: "{ 1.00 1.00 1.00 }", DFAvailGB: 120, PS: hot}))
	out, code := runLoadNode(t, env, "preflight", "--families", "100")
	if code == 0 || !strings.Contains(out, "machine not idle: node above 50% CPU in 3 of 3 samples") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	log, _ := os.ReadFile(env.Get("FAKE_LOG"))
	if got := strings.Count(string(log), "top -b -n 2 -d 2 -w 512\n"); got != 3 {
		t.Fatalf("%d windowed top reads, want 3:\n%s", got, log)
	}
	for _, macOnly := range []string{"sysctl", "ps -Ao pcpu=,comm=", "orb ", "sw_vers", "df -g"} {
		if strings.Contains(string(log), macOnly) {
			t.Fatalf("the Linux runner called %q:\n%s", macOnly, log)
		}
	}
	env = fakeHost(t, on("linux", idleHost())).With("BT_LOAD_CPU_WINDOW", "5")
	if out, code := runLoadNode(t, env, "preflight", "--families", "100"); code != 0 {
		t.Fatalf("window 5: exit %d, output %q", code, out)
	}
	if log, _ := os.ReadFile(env.Get("FAKE_LOG")); !strings.Contains(string(log), "top -b -n 2 -d 5 -w 512\n") {
		t.Fatalf("BT_LOAD_CPU_WINDOW was not top's delay:\n%s", log)
	}
	// A window that is no whole number of seconds is refused (fail-closed).
	env = fakeHost(t, on("linux", idleHost())).With("BT_LOAD_CPU_WINDOW", "0")
	if out, code := runLoadNode(t, env, "preflight", "--families", "100"); code == 0 || !strings.Contains(out, "could not sample the host") {
		t.Fatalf("window 0: exit %d, output %q", code, out)
	}
}

// Controller ruling (2026-10-16): the "any other process above 50%" rule
// fires only when sustained, in 2 or more of 3 process snapshots about 10 s
// apart. A single spike of a desktop app does not block a run.
func TestIdleCheckHotRuleNeedsTwoOfThreeSnapshots(t *testing.T) {
	onEachHost(t, func(t *testing.T, host string) {
		hot := " 97.5 /System/Library/PrivateFrameworks/AppStoreDaemon.framework/Support/appstoreagent\n  2.0 Terminal\n"
		quiet := "  2.0 Terminal\n"
		spike := fakeHost(t, on(host, fakeHostOptions{LoadAvg: "{ 1.00 1.00 1.00 }", DFAvailGB: 120, PS: quiet})).
			With("FAKE_PS_1", hot)
		if out, code := runLoadNode(t, spike, "preflight", "--families", "100"); code != 0 {
			t.Fatalf("a one-snapshot spike blocked the run: exit %d, output %q", code, out)
		}
		sustained := fakeHost(t, on(host, fakeHostOptions{LoadAvg: "{ 1.00 1.00 1.00 }", DFAvailGB: 120, PS: quiet})).
			With("FAKE_PS_1", hot).With("FAKE_PS_3", hot)
		out, code := runLoadNode(t, sustained, "preflight", "--families", "100")
		if code == 0 || !strings.Contains(out, "machine not idle: appstoreagent above 50% CPU in 2 of 3 samples") {
			t.Fatalf("exit %d, output %q", code, out)
		}
	})
}

// I1: a host that cannot be sampled is never idle, on every path that
// checks (preflight, seed, run), even though each calls the check inside ||.
func TestIdleCheckRefusesWhenTheHostCannotBeSampled(t *testing.T) {
	onEachHost(t, func(t *testing.T, host string) {
		cases := map[string]loadEnv{
			"unreadable load average": withoutLoadAverage(t, fakeHost(t, on(host, idleHost()))),
			"comma-decimal loadavg":   fakeHost(t, on(host, fakeHostOptions{LoadAvg: "{ 0,50 0,60 0,70 }", PS: "  2.0 Terminal\n", DFAvailGB: 120})),
			"comma-decimal ps":        fakeHost(t, on(host, fakeHostOptions{LoadAvg: "{ 0.50 0.60 0.70 }", PS: " 95,0 swift-frontend\n", DFAvailGB: 120})),
			"empty process list":      fakeHost(t, on(host, fakeHostOptions{LoadAvg: "{ 0.50 0.60 0.70 }", PS: "", DFAvailGB: 120})),
			"failing docker stats":    fakeHost(t, on(host, idleHost())).With("FAKE_DOCKER_STATS_FAIL", "1"),
			"failing ps":              fakeHost(t, on(host, idleHost())).With("FAKE_PS_FAIL", "1"),
		}
		if host == "linux" {
			cases["failing top"] = fakeHost(t, on(host, idleHost())).With("FAKE_TOP_FAIL", "1")
		}
		for name, env := range cases {
			out, code := runLoadNode(t, env, "preflight", "--families", "100")
			if code == 0 || !strings.Contains(out, "could not sample the host") {
				t.Fatalf("%s: preflight exit %d, output %q", name, code, out)
			}
			var idle struct {
				Idle   bool   `json:"idle"`
				Reason string `json:"reason"`
			}
			readJSON(t, filepath.Join(env.Run, "idle.json"), &idle)
			if idle.Idle || idle.Reason != "could not sample the host" {
				t.Fatalf("%s: idle.json = %+v", name, idle)
			}
		}
		env := withoutLoadAverage(t, fakeHost(t, on(host, idleHost()))).With("FAKE_APP_RUNNING", "true")
		if out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "1", "--warmup", "1s",
			"--minutes", "1"); code == 0 || !strings.Contains(out, "could not sample the host") {
			t.Fatalf("run: exit %d, output %q", code, out)
		}
		if out, code := runLoadNode(t, env, "seed", "--run", env.Run, "--families", "100", "--seed", "1"); code == 0 ||
			!strings.Contains(out, "could not sample the host") {
			t.Fatalf("seed: exit %d, output %q", code, out)
		}
		if log, _ := os.ReadFile(env.Get("FAKE_LOG")); strings.Contains(string(log), "loadgen run") || strings.Contains(string(log), "loadgen seed") {
			t.Fatalf("the generator ran on an unsampled host:\n%s", log)
		}
	})
}

// M7: other sessions' command lines are matched but never recorded: only the
// rule, the executable's base name and its %CPU.
func TestForeignCommandLinesAreNeverRecorded(t *testing.T) {
	onEachHost(t, testForeignCommandLinesAreNeverRecorded)
}

func testForeignCommandLinesAreNeverRecorded(t *testing.T, host string) {
	env := fakeHost(t, on(host, idleHost())).With("FAKE_PS_ARGS",
		"4242  3.5 /usr/local/go/bin/go test ./internal/x -args -dsn=postgres://user:hunter2@db.example/prod\n")
	out, code := runLoadNode(t, env, "preflight", "--families", "100")
	if code == 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	raw, err := os.ReadFile(filepath.Join(env.Run, "idle.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"hunter2", "-dsn", "./internal/x", "/usr/local/go/bin"} {
		if strings.Contains(string(raw), leaked) || strings.Contains(out, leaked) {
			t.Fatalf("%q was recorded: %s / %s", leaked, raw, out)
		}
	}
	var idle struct {
		Samples []struct {
			ForeignCommands []map[string]any `json:"foreign_commands"`
		} `json:"samples"`
	}
	if err := json.Unmarshal(raw, &idle); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"rule": "go test", "exe": "go", "pcpu": 3.5}
	if len(idle.Samples) != 1 || len(idle.Samples[0].ForeignCommands) != 1 || !reflect.DeepEqual(idle.Samples[0].ForeignCommands[0], want) {
		t.Fatalf("idle.json records %+v, want exactly %v", idle.Samples, want)
	}
}

func TestFailedPreflightReleasesTheLockItTook(t *testing.T) {
	env := fakeHost(t, fakeHostOptions{LoadAvg: "{ 0.5 0.5 0.5 }", PS: "  1.0 Terminal\n", DFAvailGB: 25})
	lock := filepath.Join(env.TempDir, "fresh.lock")
	if _, code := runLoadNode(t, env.With("BT_LOAD_LOCK", lock), "preflight", "--families", "10000"); code == 0 {
		t.Fatal("preflight passed with 25 GB free")
	}
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused preflight left its lock behind: %v", err)
	}
}

func TestStaleLockIsReplaced(t *testing.T) {
	env := fakeHost(t, idleHost())
	lock := filepath.Join(env.TempDir, "stale.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	gone := exec.Command("/usr/bin/true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(lock, "pid"), strconv.Itoa(gone.Process.Pid))
	writeFile(t, filepath.Join(lock, "run"), "/private/tmp/bt-load/an-old-run")
	writeFile(t, filepath.Join(lock, "expires"), strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10))
	// M3: the loser of a stale-lock race (its mv finds the lock already
	// moved) refuses and leaves the lock as it was.
	out, code := runLoadNode(t, env.With("BT_LOAD_LOCK", lock).With("FAKE_MV_FAIL", "1"), "preflight", "--families", "100")
	if code == 0 || !strings.Contains(out, "another load run took") {
		t.Fatalf("race loser: exit %d, output %q", code, out)
	}
	if run, _ := os.ReadFile(filepath.Join(lock, "run")); strings.TrimSpace(string(run)) != "/private/tmp/bt-load/an-old-run" {
		t.Fatalf("the loser changed the lock: run %q", run)
	}
	out, code = runLoadNode(t, env.With("BT_LOAD_LOCK", lock), "preflight", "--families", "100")
	if code != 0 || !strings.Contains(out, "removing stale load lock") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if run, _ := os.ReadFile(filepath.Join(lock, "run")); strings.TrimSpace(string(run)) != env.Run {
		t.Fatalf("taken-over lock names run %q", run)
	}
	if aside, _ := filepath.Glob(lock + ".stale.*"); len(aside) != 0 {
		t.Fatalf("the stale lock was moved aside but not removed: %v", aside)
	}
	// A dead pid whose lease is still running holds the lock: the run spans
	// several invocations, each of which exits.
	writeFile(t, filepath.Join(lock, "run"), "/private/tmp/bt-load/a-live-run")
	writeFile(t, filepath.Join(lock, "expires"), strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	writeFile(t, filepath.Join(lock, "pid"), strconv.Itoa(gone.Process.Pid))
	out, code = runLoadNode(t, env.With("BT_LOAD_LOCK", lock), "preflight", "--families", "100")
	if code == 0 || !strings.Contains(out, "another load run holds") {
		t.Fatalf("leased lock not respected: exit %d, output %q", code, out)
	}
}

// R4: if the lock changed hands between the staleness check and the move,
// the mover finds a live lock aside, puts it back and refuses.
func TestStaleTakeoverPutsBackALockThatWentLive(t *testing.T) {
	env := fakeHost(t, idleHost())
	lock := filepath.Join(env.TempDir, "raced.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	gone := exec.Command("/usr/bin/true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(lock, "pid"), strconv.Itoa(gone.Process.Pid))
	writeFile(t, filepath.Join(lock, "run"), "/private/tmp/bt-load/an-old-run")
	writeFile(t, filepath.Join(lock, "expires"), strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10))
	out, code := runLoadNode(t, env.With("BT_LOAD_LOCK", lock).With("FAKE_MV_RACE_PID", strconv.Itoa(os.Getpid())),
		"preflight", "--families", "100")
	if code == 0 || !strings.Contains(out, "another load run took") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if run, _ := os.ReadFile(filepath.Join(lock, "run")); strings.TrimSpace(string(run)) != "/private/tmp/bt-load/the-racer" {
		t.Fatalf("the racer's lock was not put back: run %q", run)
	}
	if aside, _ := filepath.Glob(lock + ".stale.*"); len(aside) != 0 {
		t.Fatalf("a lock was left aside: %v", aside)
	}
}

// R3 (controller ruling): the three process snapshots are 10 s apart by
// default, because macOS %cpu is a decaying average.
func TestHotSnapshotsAreTenSecondsApartByDefault(t *testing.T) {
	onEachHost(t, func(t *testing.T, host string) {
		env := fakeHost(t, on(host, idleHost())).With("BT_LOAD_HOT_GAP", "").With("FAKE_SLEEP_INSTANT", "1")
		if out, code := runLoadNode(t, env, "preflight", "--families", "100"); code != 0 {
			t.Fatalf("exit %d, output %q", code, out)
		}
		log, _ := os.ReadFile(env.Get("FAKE_LOG"))
		if got := strings.Count(string(log), "sleep 10\n"); got != 2 {
			t.Fatalf("%d pauses of 10 s between the 3 snapshots, want 2:\n%s", got, log)
		}
	})
}

// R2: another container whose CPU Docker cannot report ("--") is recorded
// as unknown; it neither fails the sample nor blocks the run.
func TestUnknownContainerCPUIsRecordedNotFatal(t *testing.T) {
	onEachHost(t, testUnknownContainerCPUIsRecordedNotFatal)
}

func testUnknownContainerCPUIsRecordedNotFatal(t *testing.T, host string) {
	env := fakeHost(t, on(host, idleHost())).With("FAKE_DOCKER_STATS", "other-starting-1 --\nother-db-1 3.00%\n")
	out, code := runLoadNode(t, env, "preflight", "--families", "100")
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	var idle struct {
		Samples []struct {
			Containers []struct {
				Name       string   `json:"name"`
				PCPU       *float64 `json:"pcpu"`
				CPUUnknown bool     `json:"cpu_unknown"`
			} `json:"containers"`
		} `json:"samples"`
	}
	readJSON(t, filepath.Join(env.Run, "idle.json"), &idle)
	if len(idle.Samples) != 1 || len(idle.Samples[0].Containers) != 2 {
		t.Fatalf("idle.json samples = %+v", idle.Samples)
	}
	unknown, known := idle.Samples[0].Containers[0], idle.Samples[0].Containers[1]
	if unknown.Name != "other-starting-1" || unknown.PCPU != nil || !unknown.CPUUnknown || known.PCPU == nil || *known.PCPU != 3 {
		t.Fatalf("containers = %+v", idle.Samples[0].Containers)
	}
	// The contamination verdict skips it too.
	sample := `{"at":"x","load1":1.0,"top":[],"foreign_commands":[],"containers":[{"name":"other-starting-1","pcpu":null,"cpu_unknown":true}]}`
	out, code = runEvaluateHost(t, strings.Repeat(sample+"\n", 7))
	if code != 0 || !strings.Contains(out, `"contaminated":false`) {
		t.Fatalf("evaluation: exit %d, output %q", code, out)
	}
}

// R1: a SIGKILLed runner runs no cleanup. Its lease renewer and samplers must
// still stop on their own, or the lock would be renewed forever.
func TestKilledRunnerLeavesNoRenewerOrSamplers(t *testing.T) {
	env := runEnv(t).With("FAKE_GENERATOR_SECONDS", "10").With("BT_LOAD_LOCK_RENEW", "1")
	output, err := os.Create(filepath.Join(env.TempDir, "runner.out"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	runner := exec.Command("/bin/bash", "load-node.sh", "run", "--run", env.Run, "--scenario", "soak", "--load", "1",
		"--warmup", "1s", "--minutes", "1")
	runner.Env = env.environ()
	runner.Stdout, runner.Stderr = output, output
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	expires := func() string {
		raw, _ := os.ReadFile(filepath.Join(env.Get("BT_LOAD_LOCK"), "expires"))
		return strings.TrimSpace(string(raw))
	}
	samples := func() int64 {
		info, err := os.Stat(filepath.Join(env.Run, "host-samples.jsonl"))
		if err != nil {
			return -1
		}
		return info.Size()
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(env.Run, "window-start")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never reached window-start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Alive, the renewer moves the lease forward every second.
	first := expires()
	time.Sleep(2500 * time.Millisecond)
	if renewed := expires(); renewed == first {
		t.Fatalf("the renewer never renewed while the runner lived (%s)", first)
	}
	if err := runner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = runner.Wait()
	time.Sleep(2500 * time.Millisecond) // every loop notices within about a second
	lease, size := expires(), samples()
	time.Sleep(3 * time.Second)
	if later := expires(); later != lease {
		t.Fatalf("the lease is still renewed after the runner was killed: %s → %s", lease, later)
	}
	if later := samples(); later != size {
		t.Fatalf("the host sampler still writes after the runner was killed: %d → %d bytes", size, later)
	}
	// Let the orphaned fake generator finish before the run directory goes.
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(env.Run, "result.json")); err == nil {
			break
		}
	}
	// down clears what a killed runner left: the sentinels and the lock.
	writeFile(t, filepath.Join(env.Run, ".seeding"), "")
	if out, code := runLoadNode(t, env, "down", "--run", env.Run); code != 0 {
		t.Fatalf("down: exit %d, output %q", code, out)
	}
	for _, sentinel := range []string{".sampling", ".seeding"} {
		if _, err := os.Stat(filepath.Join(env.Run, sentinel)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("down left %s: %v", sentinel, err)
		}
	}
}

func TestSubcommandsRequireTheLockAndDownReleasesIt(t *testing.T) {
	env := fakeHost(t, idleHost())
	other := filepath.Join(env.TempDir, "other.lock")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(other, "pid"), strconv.Itoa(os.Getpid()))
	writeFile(t, filepath.Join(other, "run"), "/private/tmp/bt-load/someone-else")
	for _, args := range [][]string{{"verify-caps"}, {"start"}, {"down"}} {
		out, code := runLoadNode(t, env.With("BT_LOAD_LOCK", other), append(args, "--run", env.Run)...)
		if code == 0 || !strings.Contains(out, "does not hold") {
			t.Fatalf("%v under another run's lock: exit %d, output %q", args, code, out)
		}
	}
	if log, _ := os.ReadFile(env.Get("FAKE_LOG")); strings.Contains(string(log), "compose") {
		t.Fatalf("a refused subcommand still ran Compose:\n%s", log)
	}
	out, code := runLoadNode(t, env, "down", "--run", env.Run)
	if code != 0 {
		t.Fatalf("down: exit %d, output %q", code, out)
	}
	if _, err := os.Stat(env.Get("BT_LOAD_LOCK")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("down kept the lock: %v", err)
	}
	log, _ := os.ReadFile(env.Get("FAKE_LOG"))
	if !strings.Contains(string(log), "down --remove-orphans --volumes") {
		t.Fatalf("down did not remove the stack and its volume:\n%s", log)
	}
}

func TestVerifyCapsWritesCapsJSON(t *testing.T) {
	env := fakeHost(t, idleHost())
	out, code := runLoadNode(t, env, "verify-caps", "--run", env.Run)
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	var caps struct {
		OK    bool   `json:"ok"`
		Label string `json:"label"`
		App   struct {
			NanoCpus, Memory, MemorySwap int64
		} `json:"app"`
		DB struct {
			NanoCpus, Memory, MemorySwap int64
			CgroupCPUMax                 string `json:"cgroup_cpu_max"`
			CgroupMemoryMax              string `json:"cgroup_memory_max"`
		} `json:"db"`
		Loadgen struct {
			Inspected      bool `json:"inspected"`
			NanoCpus       int64
			ConfigUncapped bool `json:"config_uncapped"`
		} `json:"loadgen"`
		Burn struct {
			Samples []float64 `json:"samples_pct"`
			Mean    float64   `json:"mean_pct"`
		} `json:"burn"`
		VM struct {
			HostOS          string `json:"host_os"`
			OrbMemoryMiB    int    `json:"orb_memory_mib"`
			DockerMemory    int64  `json:"docker_mem_total"`
			HostMemory      int64  `json:"host_memsize"`
			HostNCPU        int    `json:"host_ncpu"`
			Chip            string `json:"chip"`
			DeclaredBytes   int64  `json:"declared_caps_bytes"`
			MemoryLimit     int64  `json:"memory_limit_bytes"`
			CapsAreCeilings *bool  `json:"caps_are_ceilings"`
		} `json:"vm"`
	}
	readJSON(t, filepath.Join(env.Run, "caps.json"), &caps)
	if !caps.OK || caps.Label != "Apple M4, Docker CPU caps" || caps.App.NanoCpus != 4e9 || caps.DB.Memory != 8589934592 ||
		caps.DB.CgroupCPUMax != "400000 100000" || caps.DB.CgroupMemoryMax != "8589934592" ||
		!caps.Loadgen.Inspected || caps.Loadgen.NanoCpus != 0 || !caps.Loadgen.ConfigUncapped ||
		len(caps.Burn.Samples) != 3 || caps.Burn.Mean != 380 ||
		caps.VM.OrbMemoryMiB != 12288 || caps.VM.DockerMemory != 12599472128 || caps.VM.DeclaredBytes != 2*8589934592 ||
		caps.VM.HostOS != "Darwin" || caps.VM.HostMemory != 25769803776 || caps.VM.HostNCPU != 10 || caps.VM.Chip != "Apple M4" ||
		caps.VM.MemoryLimit != 12288*1048576 || caps.VM.CapsAreCeilings == nil || !*caps.VM.CapsAreCeilings {
		t.Fatalf("caps.json = %+v", caps)
	}
	log, _ := os.ReadFile(env.Get("FAKE_LOG"))
	for _, want := range []string{"docker run --rm -d --name bt-load-burn --cpus 4 --memory 8g", "docker rm -f bt-load-burn",
		"docker exec fake-db cat /sys/fs/cgroup/cpu.max /sys/fs/cgroup/memory.max"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("fake log lacks %q:\n%s", want, log)
		}
	}
	// The app and the generator run as the images' own user on the Mac.
	if user, _ := os.ReadFile(env.Get("FAKE_LOG") + ".user"); strings.TrimSpace(string(user)) != "65532:65532" {
		t.Errorf("compose ran with BT_LOAD_CONTAINER_USER %q, want 65532:65532", user)
	}
}

// On the Linux VM, caps.json records the VM's facts in place of OrbStack's:
// /proc and lscpu for the CPU, MemTotal, the kernel and the OS release,
// Docker's NCPU, MemTotal and cgroup version; the two 8 GiB caps are
// ceilings against MemTotal (as decision D2 B), and the label is computed
// from those facts. Without lscpu, /proc/cpuinfo still names the CPU.
func TestVerifyCapsRecordsTheLinuxVM(t *testing.T) {
	env := fakeHost(t, on("linux", idleHost()))
	out, code := runLoadNode(t, env, "verify-caps", "--run", env.Run)
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	var caps struct {
		OK    bool           `json:"ok"`
		Label string         `json:"label"`
		VM    map[string]any `json:"vm"`
	}
	readJSON(t, filepath.Join(env.Run, "caps.json"), &caps)
	want := map[string]any{
		"host_os": "Linux", "cpu_model": "AMD Ryzen 7 7800X3D 8-Core Processor", "host_ncpu": 12.0, "host_sockets": 1.0,
		"host_cores": 6.0, "host_threads_per_core": 2.0, "host_memsize": float64(linuxMemTotal), "kernel": "7.0.0-1004-azure",
		"os_release": "Ubuntu 24.04.3 LTS", "hypervisor_vendor": "Microsoft", "virtualization": "full", "hypervisor_flag": true,
		"docker_ncpu": 12.0, "docker_mem_total": float64(linuxMemTotal), "docker_cgroup_version": "2",
		"docker_cgroup_driver": "systemd", "docker_server_version": "29.9.0",
		"memory_limit_bytes": float64(linuxMemTotal), "memory_limit_source": "MemTotal",
		"declared_caps_bytes": float64(2 * 8589934592), "caps_are_ceilings": true,
	}
	if !caps.OK || caps.Label != linuxLabel || !reflect.DeepEqual(caps.VM, want) {
		t.Fatalf("caps.json label %q, ok %v, vm:\n got %v\nwant %v", caps.Label, caps.OK, caps.VM, want)
	}
	log, _ := os.ReadFile(env.Get("FAKE_LOG"))
	for _, macOnly := range []string{"sysctl", "orb ", "sw_vers"} {
		if strings.Contains(string(log), macOnly) {
			t.Fatalf("the Linux runner called %q:\n%s", macOnly, log)
		}
	}
	// On Linux the app and the generator run as the invoking user, who owns
	// the run directory's 0600 secrets and 0700 directories.
	if user, _ := os.ReadFile(env.Get("FAKE_LOG") + ".user"); strings.TrimSpace(string(user)) != fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()) {
		t.Errorf("compose ran with BT_LOAD_CONTAINER_USER %q, want %d:%d", user, os.Getuid(), os.Getgid())
	}
	env = fakeHost(t, on("linux", idleHost())).With("FAKE_LSCPU_FAIL", "1")
	if out, code := runLoadNode(t, env, "verify-caps", "--run", env.Run); code != 0 {
		t.Fatalf("without lscpu: exit %d, output %q", code, out)
	}
	readJSON(t, filepath.Join(env.Run, "caps.json"), &caps)
	if caps.Label != "AMD Ryzen 7 7800X3D (VM, 12 vCPU, 16 GiB), Docker CPU caps" || caps.VM["host_cores"] != 6.0 ||
		caps.VM["host_threads_per_core"] != 2.0 || caps.VM["hypervisor_vendor"] != nil {
		t.Fatalf("without lscpu: label %q, vm %v", caps.Label, caps.VM)
	}
}

// The label is computed from the host facts: the CPU model without its
// marketing tail, the hypervisor, the vCPUs and MemTotal rounded up to a
// whole GiB.
func TestNodeLabelFromHostFacts(t *testing.T) {
	for facts, want := range map[string]string{
		`{"host_os":"Darwin","chip":"Apple M4","host_ncpu":10}`:     "Apple M4, Docker CPU caps",
		`{"host_os":"Darwin","chip":"Apple M4 Pro","host_ncpu":14}`: "Apple M4 Pro, Docker CPU caps",
		`{"host_os":"Darwin","chip":null}`:                          "unknown Mac, Docker CPU caps",
		`{"host_os":"Linux","cpu_model":"AMD Ryzen 7 7800X3D 8-Core Processor","host_ncpu":12,"host_memsize":16758202368,"hypervisor_vendor":"Microsoft"}`:   linuxLabel,
		`{"host_os":"Linux","cpu_model":"Intel(R) Xeon(R) Platinum 8370C CPU @ 2.80GHz","host_ncpu":8,"host_memsize":33554432000,"hypervisor_vendor":"KVM"}`: "Intel Xeon Platinum 8370C (KVM VM, 8 vCPU, 32 GiB), Docker CPU caps",
		`{"host_os":"Linux","cpu_model":"AMD EPYC 9654 96-Core Processor","host_ncpu":192,"host_memsize":405874409472,"hypervisor_flag":false}`:              "AMD EPYC 9654 (192 CPU, 378 GiB), Docker CPU caps",
		`{"host_os":"Linux","cpu_model":null,"host_ncpu":null,"host_memsize":null}`:                                                                          "unknown CPU, Docker CPU caps",
	} {
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", `source lib/load_host.sh && bt_load_node_label "$1"`, "label", facts)
		command.Env = []string{"PATH=/usr/bin:/bin"}
		out, err := command.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != want {
			t.Errorf("%s: %q, %v; want %q", facts, out, err, want)
		}
	}
}

// BT_LOAD_NODE_LABEL (a BT_LOAD_* variable the inheritance guard lets
// through) names the node instead: every artifact that carries the label
// takes it. One that is not a single printable line is refused before
// anything runs.
func TestNodeLabelOverrideReachesEveryArtifact(t *testing.T) {
	const label = "Lab box 3, Docker CPU caps"
	env := runEnv(t).With("BT_LOAD_NODE_LABEL", label).With("FAKE_GENERATOR_SECONDS", "0").
		With("FAKE_AUDIT_VERDICT", `{"pass":true,"reasons":[],"gates":{}}`).
		With("FAKE_APP_LOG", `{"msg":"runtime limits","gomaxprocs":4,"cgroup_memory_max":8589934592}`+"\n")
	if out, code := runLoadNode(t, env, "verify-caps", "--run", env.Run); code != 0 {
		t.Fatalf("verify-caps: exit %d, output %q", code, out)
	}
	var caps map[string]any
	readJSON(t, filepath.Join(env.Run, "caps.json"), &caps)
	if caps["label"] != label {
		t.Fatalf("caps.json label = %v, want %q", caps["label"], label)
	}
	// The run labels its own artifacts, whatever an older caps.json says.
	caps["label"] = "an older label"
	raw, err := json.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(env.Run, "caps.json"), string(raw))
	if out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "0.01", "--warmup", "1s", "--minutes", "1"); code != 0 {
		t.Fatalf("run: exit %d, output %q", code, out)
	}
	out, code := runLoadNode(t, env, "audit", "--run", env.Run)
	if code != 0 || !strings.Contains(out, "verdict: PASS on "+label) {
		t.Fatalf("audit: exit %d, output %q", code, out)
	}
	for _, name := range []string{"measurement.json", "resources.json", "verdict.json"} {
		var artifact struct {
			Label string `json:"label"`
		}
		readJSON(t, filepath.Join(env.Run, name), &artifact)
		if artifact.Label != label {
			t.Errorf("%s label = %q, want %q", name, artifact.Label, label)
		}
	}
	for _, bad := range []string{"two\nlines", strings.Repeat("x", 201)} {
		env := fakeHost(t, idleHost()).With("BT_LOAD_NODE_LABEL", bad)
		out, code := runLoadNode(t, env, "verify-caps", "--run", env.Run)
		if code != 2 || !strings.Contains(out, "BT_LOAD_NODE_LABEL must be at most 200 printable characters") {
			t.Fatalf("label %q: exit %d, output %q", bad, code, out)
		}
		if log, _ := os.ReadFile(env.Get("FAKE_LOG")); strings.Contains(string(log), "docker") {
			t.Fatalf("label %q: the refused runner still ran docker:\n%s", bad, log)
		}
	}
}

func TestCapsVerificationRefusesACappedGeneratorAndAWrongCgroup(t *testing.T) {
	env := fakeHost(t, idleHost()).With("FAKE_INSPECT_LOADGEN", "2000000000 0 0")
	out, code := runLoadNode(t, env, "verify-caps", "--run", env.Run)
	if code == 0 || !strings.Contains(out, "loadgen NanoCpus 2000000000 != 0") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	env = fakeHost(t, idleHost()).With("FAKE_CGROUP_CPU", "max 100000")
	out, code = runLoadNode(t, env, "verify-caps", "--run", env.Run)
	if code == 0 || !strings.Contains(out, "db cgroup cpu.max 'max 100000' != '400000 100000'") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	env = fakeHost(t, idleHost()).With("FAKE_BURN_CPU", "120.00%")
	out, code = runLoadNode(t, env, "verify-caps", "--run", env.Run)
	if code == 0 || !strings.Contains(out, "burn mean 120.00% < 350%") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

// start checks the started app as well: its HostConfig (a changed tunable
// recreates the container) and its own view of the limits (Task 2's
// "runtime limits" line).
func TestStartChecksTheRunningAppsLimits(t *testing.T) {
	pprof := `{"time":"t","level":"INFO","msg":"pprof listening","address":"[::]:6060"}` + "\n"
	limits := `{"time":"t","level":"INFO","msg":"runtime limits","gomaxprocs":4,"gomemlimit":7730941132,"cgroup_cpu_quota":4,"cgroup_memory_max":8589934592}` + "\n"
	good := `{"time":"t","level":"INFO","msg":"listening"}` + "\n" + pprof + limits
	env := fakeHost(t, idleHost()).With("FAKE_APP_LOG", good)
	writeFile(t, filepath.Join(env.Run, "caps.json"), `{"ok":true,"mismatches":[],"app":{"running":false}}`)
	out, code := runLoadNode(t, env, "start", "--run", env.Run)
	if code != 0 || !strings.Contains(out, "start: app ready") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	var caps struct {
		OK  bool `json:"ok"`
		App struct {
			NanoCpus      int64
			Running       bool `json:"running"`
			RuntimeLimits struct {
				GOMAXPROCS int `json:"gomaxprocs"`
			} `json:"runtime_limits"`
		} `json:"app"`
	}
	readJSON(t, filepath.Join(env.Run, "caps.json"), &caps)
	if !caps.OK || !caps.App.Running || caps.App.NanoCpus != 4e9 || caps.App.RuntimeLimits.GOMAXPROCS != 4 {
		t.Fatalf("caps.json after start = %+v", caps)
	}
	if _, err := os.Stat(filepath.Join(env.Run, "tunables.json")); err != nil {
		t.Fatal(err)
	}
	wide := strings.Replace(good, `"gomaxprocs":4`, `"gomaxprocs":10`, 1)
	out, code = runLoadNode(t, env.With("FAKE_APP_LOG", wide), "start", "--run", env.Run)
	if code == 0 || !strings.Contains(out, "app runtime limits gomaxprocs=10 != 4") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	out, code = runLoadNode(t, env.With("FAKE_INSPECT_APP", "0 0 0"), "start", "--run", env.Run)
	if code == 0 || !strings.Contains(out, "app NanoCpus 0 != 4000000000") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	// M8: pprof must be listening (Task 2's "pprof listening" line), and a
	// refused address is reported at start, not at the first profile.
	refused := `{"time":"t","level":"ERROR","msg":"pprof address refused","address":"0.0.0.0:6060","error":"only loopback"}` + "\n"
	for log, want := range map[string]string{
		limits:           "app log has no 'pprof listening' line",
		limits + refused: "app pprof did not start: only loopback",
	} {
		out, code = runLoadNode(t, env.With("FAKE_APP_LOG", log).With("BT_LOAD_START_LOG_WAIT", "1"), "start", "--run", env.Run)
		if code == 0 || !strings.Contains(out, want) {
			t.Fatalf("exit %d, output %q; want %q", code, out, want)
		}
	}
}

// The measurement pipeline end to end against the fakes: samplers, the
// generator's invocation, pg_stat_statements reset at window-start and its
// dump, profiles at the offset, and the files every result directory holds.
// A second run archives the first.
func TestRunProducesTheResultDirectory(t *testing.T) {
	onEachHost(t, testRunProducesTheResultDirectory)
}

func testRunProducesTheResultDirectory(t *testing.T, host string) {
	// The app was started with a socket limit of 30,000 and an admission of
	// 64; this run's own environment names neither, but sets a stale
	// runtime pool the app does not run.
	env := fakeHost(t, on(host, idleHost())).With("FAKE_APP_RUNNING", "true").With("FAKE_PROJECT_CONTAINERS", "fake-app\nfake-db\n").
		With("BT_LOAD_HOST_INTERVAL", "1").With("BT_LOAD_DOCKER_INTERVAL", "1").With("BT_LOAD_PG_INTERVAL", "1").
		With("BT_LOAD_RUNTIME_CONNS", "16").
		With("FAKE_APP_ENV", "BT_MODE=hosted\nBT_SOCKET_PROCESS_LIMIT=30000\nBT_DB_RUNTIME_MAX_CONNS=48\nBT_DB_MAINTENANCE_MAX_CONNS=4\n"+
			"BT_ADMISSION_TOTAL=64\nBT_IDENTITYTEST_APNS_SINK_LATENCY_MS=100\nBT_ADMISSION_WAIT=250ms\nBT_DB_MCP_MAX_CONNS=2")
	if host == "linux" {
		writeFile(t, filepath.Join(env.Run, "caps.json"), `{"ok":true,"label":"`+linuxLabel+`","vm":{"host_os":"Linux","memory_limit_bytes":`+
			strconv.Itoa(linuxMemTotal)+`,"caps_are_ceilings":true}}`)
	} else {
		writeFile(t, filepath.Join(env.Run, "caps.json"), `{"ok":true,"label":"Apple M4, Docker CPU caps","vm":{"orb_memory_mib":12288}}`)
	}
	before := time.Now()
	out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "0.01", "--warmup", "1s",
		"--minutes", "1", "--profile-at", "0s", "--steps", "1,2")
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	// M4: the lease is renewed when the run finishes, not only when it
	// starts (the generator ran for 2 s).
	expires, _ := os.ReadFile(filepath.Join(env.Get("BT_LOAD_LOCK"), "expires"))
	if at, err := strconv.ParseInt(strings.TrimSpace(string(expires)), 10, 64); err != nil || at < before.Add(4*time.Hour+2*time.Second).Unix() {
		t.Errorf("lease %q was not renewed at the end of the run (started %d)", expires, before.Unix())
	}
	var measurement struct {
		Label            string            `json:"label"`
		GeneratorAddress string            `json:"generator_address"`
		ProfileFailures  int               `json:"profile_failures"`
		Tunables         map[string]string `json:"tunables"`
		TunablesSource   string            `json:"tunables_source"`
	}
	readJSON(t, filepath.Join(env.Run, "measurement.json"), &measurement)
	if measurement.GeneratorAddress != "172.31.250.100" || measurement.ProfileFailures != 0 || measurement.Label != hostLabel(host) ||
		measurement.TunablesSource != "app container" {
		t.Errorf("measurement.json = %+v", measurement)
	}
	// The tunables are what the running app applied (its environment),
	// under their BT_LOAD_* names, whatever this run's environment says.
	for name, want := range map[string]string{"BT_LOAD_SOCKET_LIMIT": "30000", "BT_LOAD_RUNTIME_CONNS": "48",
		"BT_LOAD_MAINTENANCE_CONNS": "4", "BT_LOAD_ADMISSION": "64", "BT_LOAD_APNS_LATENCY_MS": "100", "BT_LOAD_ADMISSION_WAIT": "250ms"} {
		if got := measurement.Tunables[name]; got != want {
			t.Errorf("measurement.json tunables %s = %q, want %q (%v)", name, got, want, measurement.Tunables)
		}
	}
	if measurement.Tunables["BT_LOAD_HOST_INTERVAL"] != "1" {
		t.Errorf("measurement.json lost the runner's own tunables: %v", measurement.Tunables)
	}
	// The memory note: the caps are ceilings against OrbStack's VM on the
	// Mac, against MemTotal on Linux.
	wantVM := "VM: 12288 MiB (OrbStack), caps app 8192 MiB + db 8192 MiB are ceilings"
	if host == "linux" {
		wantVM = "VM: MemTotal 15981 MiB (Linux), caps app 8192 MiB + db 8192 MiB are ceilings"
	}
	if !strings.Contains(out, wantVM) {
		t.Errorf("output lacks %q:\n%s", wantVM, out)
	}
	log, _ := os.ReadFile(env.Get("FAKE_LOG"))
	for _, want := range []string{
		"run --rm --no-deps -T loadgen run --base http://172.31.250.10:8080 --host sync.load.test --fleet /run/load/private/fleet.jsonl " +
			"--ledger /run/load/private/ledger.jsonl --tracked /run/load/private/tracked.jsonl --out /run/load/result.json " +
			"--scenario soak --load 0.01 --warmup 1s --measure 1m --seed 1 --steps 1,2",
		"pg_stat_statements_reset()",
	} {
		if !strings.Contains(string(log), want) {
			t.Errorf("fake log lacks %q:\n%s", want, log)
		}
	}
	for _, name := range []string{"host-samples.jsonl", "docker-stats.jsonl", "pg-locks.jsonl", "pg-wait-events.jsonl",
		"pg_stat_statements.json", "app.log", "result.json", "host.json", "resources.json", "measurement.json", "run-idle.json",
		"profiles/at-0s-cpu.pb.gz", "profiles/at-0s-heap.pb.gz", "profiles/at-0s-mutex.pb.gz", "profiles/at-0s-block.pb.gz",
		"profiles/at-0s-goroutine.txt"} {
		if info, err := os.Stat(filepath.Join(env.Run, name)); err != nil || (info.Size() == 0 && name != "app.log") {
			t.Errorf("%s: %v (empty: %v)", name, err, err == nil && info.Size() == 0)
		}
	}
	if _, err := os.Stat(filepath.Join(env.Run, ".sampling")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the sampling sentinel survived the run: %v", err)
	}
	var hostJSON struct {
		Contaminated bool `json:"contaminated"`
		Samples      int  `json:"samples"`
	}
	readJSON(t, filepath.Join(env.Run, "host.json"), &hostJSON)
	if hostJSON.Contaminated || hostJSON.Samples < 1 {
		t.Errorf("host.json = %+v", hostJSON)
	}
	var resources struct {
		Measured map[string]struct {
			CPUPeak float64 `json:"cpu_peak_pct"`
			MemPeak int64   `json:"mem_peak_bytes"`
			Samples int     `json:"samples"`
			CPUMean float64 `json:"cpu_mean_pct"`
		} `json:"measured"`
		VM struct {
			OrbMemoryMiB int   `json:"orb_memory_mib"`
			MemoryLimit  int64 `json:"memory_limit_bytes"`
		} `json:"vm"`
		Label string `json:"label"`
		Note  string `json:"note"`
	}
	readJSON(t, filepath.Join(env.Run, "resources.json"), &resources)
	if app := resources.Measured["app"]; app.CPUPeak != 150 || app.MemPeak != 1610612736 || resources.Label != hostLabel(host) {
		t.Errorf("resources.json = %+v", resources)
	}
	if host == "linux" {
		if resources.VM.MemoryLimit != linuxMemTotal || !strings.Contains(resources.Note, "exceed the Linux VM's MemTotal") {
			t.Errorf("resources.json on Linux = %+v", resources)
		}
	} else if resources.VM.OrbMemoryMiB != 12288 || !strings.Contains(resources.Note, "exceed the OrbStack VM memory") {
		t.Errorf("resources.json on the Mac = %+v", resources)
	}
	var statements struct {
		WindowStart string            `json:"window_start"`
		Top         []json.RawMessage `json:"top_by_total_exec_time"`
	}
	readJSON(t, filepath.Join(env.Run, "pg_stat_statements.json"), &statements)
	if statements.WindowStart == "" || len(statements.Top) != 1 {
		t.Errorf("pg_stat_statements.json = %+v", statements)
	}
	out, code = runLoadNode(t, env.With("FAKE_GENERATOR_SECONDS", "0"), "run", "--run", env.Run, "--scenario", "soak",
		"--load", "0.01", "--warmup", "1s", "--minutes", "1")
	if code != 0 {
		t.Fatalf("second run: exit %d, output %q", code, out)
	}
	for _, name := range []string{"runs/1/result.json", "runs/1/profiles/at-0s-cpu.pb.gz", "runs/1/measurement.json", "measurement.json"} {
		if _, err := os.Stat(filepath.Join(env.Run, name)); err != nil {
			t.Errorf("after a second run: %v", err)
		}
	}
	// A failing generator fails the run, and the run still leaves its evidence.
	out, code = runLoadNode(t, env.With("FAKE_GENERATOR_EXIT", "3").With("FAKE_GENERATOR_SECONDS", "0"), "run", "--run", env.Run,
		"--scenario", "soak", "--load", "0.01", "--warmup", "1s", "--minutes", "1")
	if code != 3 || !strings.Contains(out, "generator exit 3") {
		t.Fatalf("failing generator: exit %d, output %q", code, out)
	}
}

// followerGone fails the test when the fake "docker logs --follow" last
// started is still alive: the runner must not leave its log follower behind.
func followerGone(t *testing.T, env loadEnv) {
	t.Helper()
	raw, err := os.ReadFile(env.Get("FAKE_LOG") + ".logspid")
	if err != nil {
		t.Fatalf("no log follower was started: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("the log follower (pid %d) outlived the run", pid)
}

// The app's log for a measurement is streamed from before the generator
// starts (docker's json-file rotation would drop the start of a long run from
// a copy taken at the end): app.log holds what the follower read, the lines
// the app wrote while the runner was stopping included, and the follower is
// gone afterwards. The rotation itself is raised for the load stack.
func TestRunKeepsTheAppsWholeLog(t *testing.T) {
	first := `{"msg":"capacity resource sample","n":1}` + "\n"
	env := runEnv(t).With("BT_LOAD_LOG_DRAIN", "2").With("FAKE_APP_LOG", first).
		With("FAKE_APP_LOG_LATE", `{"msg":"the last line","n":2}`+"\n")
	out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "1", "--warmup", "1s", "--minutes", "1")
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	got, err := os.ReadFile(filepath.Join(env.Run, "app.log"))
	if err != nil || string(got) != first+`{"msg":"the last line","n":2}`+"\n" {
		t.Fatalf("app.log = %q, %v", got, err)
	}
	log, _ := os.ReadFile(env.Get("FAKE_LOG"))
	if !regexp.MustCompile(`docker logs --follow --since 20\d\d-\d\d-\d\dT\d\d:\d\d:\d\dZ fake-app\n`).Match(log) {
		t.Fatalf("the app's log was not followed from the start of the run:\n%s", log)
	}
	if regexp.MustCompile(`docker logs fake-app\n`).Match(log) {
		t.Fatalf("the app's log was also copied at the end:\n%s", log)
	}
	followerGone(t, env)

	// A follow that ends (the app stopped or restarted: the restart scenario)
	// is attached again, and a generator that fails leaves the follower
	// stopped as well.
	// The generator runs until the follower attached twice (at most 20 s),
	// however busy the machine.
	env2 := runEnv(t).With("FAKE_LOGS_FOLLOW_ENDS", "1").With("FAKE_APP_LOG", first).With("FAKE_GENERATOR_EXIT", "3").
		With("FAKE_GENERATOR_FOLLOWS", "2")
	out, code = runLoadNode(t, env2, "run", "--run", env2.Run, "--scenario", "soak", "--load", "1", "--warmup", "1s", "--minutes", "1")
	if code != 3 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	log, _ = os.ReadFile(env2.Get("FAKE_LOG"))
	if n := strings.Count(string(log), "docker logs --follow --since"); n < 2 {
		t.Fatalf("the ended follow was attached %d time(s), want at least 2:\n%s", n, log)
	}
	if got, _ := os.ReadFile(filepath.Join(env2.Run, "app.log")); !strings.HasPrefix(string(got), first) {
		t.Fatalf("app.log = %q", got)
	}
}

// dbCommandSetting is the value the load overlay's db command line gives
// the setting (the text after "name="), or "" when it gives none.
func dbCommandSetting(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../deploy/load/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	db := sectionOf(string(raw), "db")
	match := regexp.MustCompile(`-c, "?`+regexp.QuoteMeta(name)+`=([^,\]"]+)"?[,\]]`).FindAllStringSubmatch(db, -1)
	if len(match) > 1 {
		t.Fatalf("the db command sets %s %d times:\n%s", name, len(match), db)
	}
	if match == nil {
		return ""
	}
	return match[0][1]
}

// The command line outranks ALTER SYSTEM, so a run that wants every
// autovacuum logged (vm-v2-analysis.md: line its actions up with the 503
// seconds) sets BT_LOAD_LOG_AUTOVACUUM_MIN_DURATION; the default stays 1s.
func TestLoadOverlayTakesTheAutovacuumLogThresholdFromTheRun(t *testing.T) {
	if got := dbCommandSetting(t, "log_autovacuum_min_duration"); got != "${BT_LOAD_LOG_AUTOVACUUM_MIN_DURATION:-1s}" {
		t.Errorf("log_autovacuum_min_duration = %q", got)
	}
}

// The background writer may clean 1000 buffers a round, every 50 ms (up to
// 20,000 a second), against 100 every 200 ms (500 a second) by default: at
// 2.5x the db allocates 3.6-4.6k buffers a second, so the capped bgwriter
// (maxwritten_clean 2,894 in 601 s) left backends writing dirty victims
// themselves in 0.5-1 s IO/DataFileWrite storms (vm-v2-analysis.md,
// Follow-up 2). An A/B candidate, in its own commit so it can be reverted.
func TestLoadOverlayLetsTheBackgroundWriterKeepUp(t *testing.T) {
	for name, want := range map[string]string{"bgwriter_lru_maxpages": "1000", "bgwriter_delay": "50ms"} {
		if got := dbCommandSetting(t, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// The app's admission wait is the run's BT_LOAD_ADMISSION_WAIT, default
// 250ms as the code's, so an A/B can turn it off with 0.
func TestLoadOverlayTakesTheAdmissionWaitFromTheRun(t *testing.T) {
	raw, err := os.ReadFile("../deploy/load/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sectionOf(string(raw), "app"), `BT_ADMISSION_WAIT: "${BT_LOAD_ADMISSION_WAIT:-250ms}"`) {
		t.Error("the app's BT_ADMISSION_WAIT is not the run's BT_LOAD_ADMISSION_WAIT")
	}
}

// The load overlay raises the app's json-file rotation far above the
// daemon's default (5 x 20 MB), so a follower that falls behind does not lose
// rotated lines either.
func TestLoadOverlayRaisesTheAppsLogRotation(t *testing.T) {
	raw, err := os.ReadFile("../deploy/load/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	app := string(raw)[strings.Index(string(raw), "\n  app:\n"):]
	app = app[:strings.Index(app, "\n  migrate:\n")]
	match := regexp.MustCompile(`logging: \{driver: json-file, options: \{max-size: "?(\d+)m"?, max-file: "?(\d+)"?\}\}`).FindStringSubmatch(app)
	if match == nil {
		t.Fatalf("the app service has no json-file logging options:\n%s", app)
	}
	size, _ := strconv.Atoi(match[1])
	files, _ := strconv.Atoi(match[2])
	if size*files < 2048 {
		t.Errorf("the app keeps %d x %d MB of log, want at least 2 GB", files, size)
	}
}

func runEnv(t *testing.T) loadEnv {
	t.Helper()
	env := fakeHost(t, idleHost()).With("FAKE_APP_RUNNING", "true").
		With("BT_LOAD_HOST_INTERVAL", "1").With("BT_LOAD_DOCKER_INTERVAL", "1").With("BT_LOAD_PG_INTERVAL", "1")
	writeFile(t, filepath.Join(env.Run, "caps.json"), `{"ok":true,"vm":{"orb_memory_mib":12288}}`)
	return env
}

// When the app's environment cannot be read at the end of a run, the
// tunables are this run's environment only: measurement.json says so
// (tunables_source), and the runner warns.
func TestRunSaysWhenTheAppsTunablesCouldNotBeRead(t *testing.T) {
	env := runEnv(t).With("FAKE_INSPECT_ENV_FAIL", "1").With("BT_LOAD_SOCKET_LIMIT", "30000")
	out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "1", "--warmup", "1s", "--minutes", "1")
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	var measurement struct {
		Tunables       map[string]string `json:"tunables"`
		TunablesSource string            `json:"tunables_source"`
	}
	readJSON(t, filepath.Join(env.Run, "measurement.json"), &measurement)
	if measurement.TunablesSource != "environment only (the app container could not be inspected)" ||
		measurement.Tunables["BT_LOAD_SOCKET_LIMIT"] != "30000" || !strings.Contains(out, "could not read the app's applied tunables") {
		t.Fatalf("measurement %+v, output %q", measurement, out)
	}
}

// Task 19: the flood cell is valid only if the IP flood made the scopes it
// reaches evict. The runner reads the app's limiter samples over the run
// (the flood runs only in the measurement) and records each IP scope's
// evictions in result.json cells.flood, with evictions_ok false, and a
// warning, when an expected scope evicted nothing.
func TestFloodRunRecordsEachIPScopesEvictions(t *testing.T) {
	sample := func(challenge, refresh int) string {
		return `{"msg":"capacity resource sample","sample":{"at":"t","limiter":{"verified_full":0,"ip_evictions":{"auth-challenge-ip":` +
			strconv.Itoa(challenge) + `,"auth-refresh-ip":` + strconv.Itoa(refresh) + `,"live-attempt-ip":0}}}}` + "\n"
	}
	result := `{"routes":{},"cells":{"flood":{"mode":"both","evictions_expected":["auth-challenge-ip","auth-refresh-ip"]}}}`
	type flood struct {
		IPEvictions  map[string]uint64 `json:"ip_evictions"`
		EvictionsOK  *bool             `json:"evictions_ok"`
		InvalidCause string            `json:"evictions_missing"`
		Mode         string            `json:"mode"`
	}
	read := func(env loadEnv) flood {
		var r struct {
			Cells struct {
				Flood flood `json:"flood"`
			} `json:"cells"`
		}
		readJSON(t, filepath.Join(env.Run, "result.json"), &r)
		return r.Cells.Flood
	}
	run := func(log string) (loadEnv, string) {
		env := runEnv(t).With("FAKE_APP_LOG", log).With("FAKE_RESULT", result)
		out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "flood", "--load", "1", "--warmup", "1s", "--minutes", "1")
		if code != 0 {
			t.Fatalf("exit %d, output %q", code, out)
		}
		return env, out
	}
	env, out := run(sample(10, 0) + "not json\n" + sample(400, 120) + sample(510, 300))
	got := read(env)
	if got.EvictionsOK == nil || !*got.EvictionsOK || got.IPEvictions["auth-challenge-ip"] != 500 || got.IPEvictions["auth-refresh-ip"] != 300 ||
		got.Mode != "both" || strings.Contains(out, "flood cell") {
		t.Fatalf("flood cell %+v, output %q", got, out)
	}
	env, out = run(sample(0, 0) + sample(700, 0))
	got = read(env)
	if got.EvictionsOK == nil || *got.EvictionsOK || got.InvalidCause != "auth-refresh-ip" ||
		!strings.Contains(out, "flood cell invalid: no IP evictions in auth-refresh-ip") {
		t.Fatalf("flood cell %+v, output %q", got, out)
	}
	env, out = run("")
	got = read(env)
	if got.EvictionsOK == nil || *got.EvictionsOK || !strings.Contains(out, "flood cell invalid") {
		t.Fatalf("no samples: flood cell %+v, output %q", got, out)
	}
}

// M2: a generator that never ends its warm-up is stopped, container and all,
// when the window-start wait times out.
func TestRunStopsTheGeneratorWhenWarmupNeverEnds(t *testing.T) {
	env := runEnv(t).With("FAKE_GENERATOR_NO_WINDOW", "1").With("BT_LOAD_WINDOW_GRACE", "1")
	started := time.Now()
	out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "1", "--warmup", "1s", "--minutes", "1")
	if code == 0 || !strings.Contains(out, "no window-start within warm-up + 1 s") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	log, _ := os.ReadFile(env.Get("FAKE_LOG"))
	if !strings.Contains(string(log), "docker stop fake-loadgen-run") {
		t.Fatalf("the generator container was not stopped:\n%s", log)
	}
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("the run waited %v for a generator it should have stopped", elapsed)
	}
	// No window, no cluster snapshot: the counters have no baseline.
	if cluster, err := os.ReadFile(filepath.Join(env.Run, "pg-stat-cluster.jsonl")); err != nil || len(cluster) != 0 {
		t.Errorf("pg-stat-cluster.jsonl without a window = %q, %v", cluster, err)
	}
	followerGone(t, env)
}

// Task 21's instrumentation: the statements dump carries plan time, the
// exec-time spread, block I/O time and WAL per statement (with the settings
// that make them non-zero); the cluster's WAL, checkpointer and bgwriter
// counters are snapshotted at the window's start and end, the end line
// carrying the window's delta; and the lock sampler names each waiter's
// blocker, its statement and the lock wait's own duration.
func TestRunCapturesPlanTimeWALAndLockBlockers(t *testing.T) {
	env := runEnv(t)
	out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "1", "--warmup", "1s", "--minutes", "1")
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	raw, _ := os.ReadFile(env.Get("FAKE_LOG"))
	log := string(raw)
	dump := strings.Index(log, "FROM pg_stat_statements")
	if dump < 0 {
		t.Fatalf("no statements dump:\n%s", log)
	}
	statementsSQL := log[strings.LastIndex(log[:dump], "docker exec"):dump]
	for _, column := range []string{"total_exec_time", "mean_exec_time", "min_exec_time", "max_exec_time", "stddev_exec_time",
		"plans", "total_plan_time", "mean_plan_time", "max_plan_time", "shared_blk_read_time", "shared_blk_write_time",
		"shared_blks_dirtied", "shared_blks_written", "wal_records", "wal_fpi", "wal_bytes",
		"current_setting('pg_stat_statements.track_planning', true)", "current_setting('track_io_timing')",
		"current_setting('track_wal_io_timing')"} {
		if !strings.Contains(statementsSQL, column) {
			t.Errorf("the statements dump lacks %s", column)
		}
	}
	var statements struct {
		Settings struct {
			TrackPlanning    string `json:"track_planning"`
			TrackWALIOTiming string `json:"track_wal_io_timing"`
		} `json:"settings"`
		Top []json.RawMessage `json:"top_by_total_exec_time"`
	}
	readJSON(t, filepath.Join(env.Run, "pg_stat_statements.json"), &statements)
	if statements.Settings.TrackPlanning != "on" || statements.Settings.TrackWALIOTiming != "on" || len(statements.Top) != 1 {
		t.Errorf("pg_stat_statements.json = %+v", statements)
	}

	// The baseline follows the statistics reset; the end snapshot follows
	// the statements dump.
	reset, first := strings.Index(log, "pg_stat_statements_reset()"), strings.Index(log, "FROM pg_stat_wal")
	last := strings.LastIndex(log, "FROM pg_stat_wal")
	if reset < 0 || first < reset || first == last || last < dump {
		t.Fatalf("cluster snapshots at %d and %d, reset at %d, dump at %d", first, last, reset, dump)
	}
	for _, view := range []string{"pg_stat_checkpointer", "pg_stat_bgwriter", "current_setting('track_wal_io_timing')"} {
		if !strings.Contains(log[first:], view) {
			t.Errorf("the cluster snapshot lacks %s", view)
		}
	}
	cluster, err := os.ReadFile(filepath.Join(env.Run, "pg-stat-cluster.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(cluster)), "\n")
	if len(lines) != 2 {
		t.Fatalf("pg-stat-cluster.jsonl has %d lines, want 2:\n%s", len(lines), cluster)
	}
	type snapshot struct {
		Phase string `json:"phase"`
		WAL   struct {
			FPI int `json:"wal_fpi"`
		} `json:"wal"`
		Delta *struct {
			Seconds      float64            `json:"seconds"`
			WAL          map[string]float64 `json:"wal"`
			Checkpointer map[string]float64 `json:"checkpointer"`
			BGWriter     map[string]float64 `json:"bgwriter"`
		} `json:"delta"`
	}
	var start, end snapshot
	if err := json.Unmarshal([]byte(lines[0]), &start); err != nil || start.Phase != "window_start" || start.WAL.FPI != 7 || start.Delta != nil {
		t.Errorf("start snapshot %s: %+v, %v", lines[0], start, err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &end); err != nil || end.Phase != "window_end" || end.Delta == nil {
		t.Fatalf("end snapshot %s: %+v, %v", lines[1], end, err)
	}
	wantWAL := map[string]float64{"wal_records": 100, "wal_fpi": 7, "wal_bytes": 8192, "wal_buffers_full": 2}
	if end.Delta.Seconds != 30 || !reflect.DeepEqual(end.Delta.WAL, wantWAL) ||
		!reflect.DeepEqual(end.Delta.Checkpointer, map[string]float64{"num_timed": 1, "write_time": 10}) ||
		!reflect.DeepEqual(end.Delta.BGWriter, map[string]float64{"buffers_alloc": 50}) {
		t.Errorf("window delta = %+v (the counters less the start's; stats_reset is no counter)", *end.Delta)
	}

	// The lock sampler: the blocker's statement, transaction age and wait,
	// and the lock wait's own start.
	locks := strings.Index(log, "FROM pg_locks l")
	if locks < 0 {
		t.Fatalf("no lock sample:\n%s", log)
	}
	locksSQL := log[strings.LastIndex(log[:locks], "docker exec"):locks]
	for _, want := range []string{"'lock_wait_ms', round(extract(epoch FROM clock_timestamp() - l.waitstart) * 1000)",
		"'blocker', CASE WHEN b.pid IS NOT NULL", "'query', left(b.query, 300)", "'xact_ms'", "'wait_event', b.wait_event",
		"'waiting_ms'", "'blocked_by', bp.pids"} {
		if !strings.Contains(locksSQL, want) {
			t.Errorf("the lock sample lacks %s", want)
		}
	}
	if !strings.Contains(log[locks:], "LEFT JOIN pg_stat_activity b ON b.pid = bp.pids[1]") {
		t.Error("the blocker is not the first pid pg_blocking_pids names")
	}
}

// M5: the generator must be at the trusted-proxy address.
func TestRunRefusesAGeneratorAtTheWrongAddress(t *testing.T) {
	for address, want := range map[string]string{
		"172.31.250.7": "generator address on bt-load_load is '172.31.250.7', not 172.31.250.100",
		"":             "generator address on bt-load_load is '', not 172.31.250.100",
	} {
		env := runEnv(t).With("FAKE_LOADGEN_ADDRESS", address)
		out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "1", "--warmup", "1s", "--minutes", "1")
		if code == 0 || !strings.Contains(out, want) || !strings.Contains(out, "refusing the run") {
			t.Fatalf("%q: exit %d, output %q", address, code, out)
		}
		log, _ := os.ReadFile(env.Get("FAKE_LOG"))
		if !strings.Contains(string(log), "docker stop fake-loadgen-run") {
			t.Fatalf("%q: the generator was left running:\n%s", address, log)
		}
		if _, err := os.Stat(filepath.Join(env.Run, ".sampling")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%q: samplers left running: %v", address, err)
		}
	}
}

// M6: numbers are read in base 10 and made canonical before anything runs.
func TestNumbersAreParsedInBaseTen(t *testing.T) {
	env := runEnv(t).With("FAKE_GENERATOR_SECONDS", "0")
	out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "01", "--warmup", "08s", "--minutes", "08")
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	var measurement struct {
		Load    float64 `json:"load"`
		Minutes int     `json:"minutes"`
	}
	readJSON(t, filepath.Join(env.Run, "measurement.json"), &measurement)
	if measurement.Load != 1 || measurement.Minutes != 8 {
		t.Fatalf("measurement.json = %+v", measurement)
	}
	log, _ := os.ReadFile(env.Get("FAKE_LOG"))
	if !strings.Contains(string(log), "--load 1 --warmup 08s --measure 8m") {
		t.Fatalf("generator arguments not canonical:\n%s", log)
	}
	env = env.With("FAKE_APP_RUNNING", "false") // the seed runs before start
	out, code = runLoadNode(t, env, "seed", "--run", env.Run, "--families", "0100", "--seed", "09", "--workers", "08")
	if code != 0 {
		t.Fatalf("seed: exit %d, output %q", code, out)
	}
	log, _ = os.ReadFile(env.Get("FAKE_LOG"))
	if !strings.Contains(string(log), "--families 100 --seed 9") || !strings.Contains(string(log), "--workers 8") ||
		strings.Contains(string(log), "--hot-family") {
		t.Fatalf("seed arguments not canonical:\n%s", log)
	}
	// Task 20's hot family is asked for by a flag without a value.
	out, code = runLoadNode(t, env, "seed", "--run", env.Run, "--families", "100", "--seed", "1", "--hot-family")
	if log, _ := os.ReadFile(env.Get("FAKE_LOG")); code != 0 || !strings.Contains(string(log), "--report /run/load/seed-report.json --hot-family") {
		t.Fatalf("seed --hot-family: exit %d, output %q:\n%s", code, out, log)
	}
	for _, args := range [][]string{
		{"--load", "1e3"}, {"--load", "-1"}, {"--minutes", "8.5"}, {"--minutes", "0x10"},
	} {
		full := append([]string{"run", "--run", env.Run, "--scenario", "soak", "--load", "1", "--warmup", "1s", "--minutes", "1"}, args...)
		if out, code := runLoadNode(t, env, full...); code != 2 {
			t.Errorf("%v: exit %d, output %q", args, code, out)
		}
	}
}

// M9: a failed profile capture is logged and recorded, and the later
// offsets are still taken; the run itself goes on.
func TestFailedProfilesAreRecordedAndLaterOffsetsContinue(t *testing.T) {
	env := runEnv(t).With("FAKE_CURL_EXIT", "1").With("FAKE_GENERATOR_SECONDS", "3")
	out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "1", "--warmup", "1s",
		"--minutes", "1", "--profile-at", "0s,1s")
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	raw, err := os.ReadFile(filepath.Join(env.Run, "profile-failures.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{`"label":"at-0s","kind":"cpu"`, `"label":"at-1s","kind":"goroutine"`} {
		if !strings.Contains(string(raw), label) {
			t.Errorf("profile-failures.jsonl lacks %s:\n%s", label, raw)
		}
	}
	var measurement struct {
		ProfileFailures int `json:"profile_failures"`
	}
	readJSON(t, filepath.Join(env.Run, "measurement.json"), &measurement)
	if measurement.ProfileFailures != 10 {
		t.Errorf("profile_failures = %d, want 10 (5 kinds at 2 offsets)", measurement.ProfileFailures)
	}
	if _, err := os.Stat(filepath.Join(env.Run, "profiles", "at-0s-cpu.pb.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed capture left a partial file: %v", err)
	}
}

// Carry from Task 4's review: up marks the load stack's database so the
// seeder can refuse any other database.
func TestUpMarksTheLoadStackDatabase(t *testing.T) {
	for marker, wantOK := range map[string]bool{"baby-tracker-load-stack": true, "": false} {
		env := fakeHost(t, idleHost()).With("FAKE_DB_MARKER", marker)
		if err := os.RemoveAll(filepath.Join(env.Run, "private")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(env.Run, "source-sha")); err != nil {
			t.Fatal(err)
		}
		out, code := runLoadNode(t, env, "up", "--run", env.Run)
		if (code == 0) != wantOK {
			t.Fatalf("marker %q: exit %d, output %q", marker, code, out)
		}
		log, _ := os.ReadFile(env.Get("FAKE_LOG"))
		text := string(log)
		comment := strings.Index(text, `COMMENT ON DATABASE babytracker IS 'baby-tracker-load-stack'`)
		authority := strings.Index(text, "migrate authority init")
		if comment < 0 || authority < 0 || comment < authority {
			t.Fatalf("marker %q: the COMMENT does not follow migrate and authority init:\n%s", marker, text)
		}
		if !wantOK && !strings.Contains(out, "does not carry the load-stack marker") {
			t.Fatalf("missing marker not refused: %q", out)
		}
		if wantOK && !strings.Contains(text, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements") {
			t.Fatalf("up did not finish:\n%s", text)
		}
	}
}

func TestRunRefusesWithoutTheGeneratorImageOrARunningApp(t *testing.T) {
	env := fakeHost(t, idleHost()).With("FAKE_APP_RUNNING", "true").With("FAKE_IMAGE_MISSING", "1")
	out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "1", "--warmup", "1m", "--minutes", "5")
	if code == 0 || !strings.Contains(out, "generator image bt-load-loadgen:identitytest is not built") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	env = fakeHost(t, idleHost())
	out, code = runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "1", "--warmup", "1m", "--minutes", "5")
	if code == 0 || !strings.Contains(out, "the app is not running") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	for _, args := range [][]string{
		{"--scenario", "nap", "--load", "1", "--warmup", "1m", "--minutes", "5"},
		{"--scenario", "soak", "--load", "1", "--warmup", "1m", "--minutes", "5", "--profile-at", "5m"},
		{"--scenario", "soak", "--load", "1", "--warmup", "soon", "--minutes", "5"},
		{"--scenario", "soak", "--load", "1", "--warmup", "1m", "--minutes", "5", "--steps", "1;rm -rf /"},
	} {
		if out, code := runLoadNode(t, env, append([]string{"run", "--run", env.Run}, args...)...); code != 2 {
			t.Errorf("%v: exit %d, output %q", args, code, out)
		}
	}
}

// Task 24: a ramp profiles relative to its steps. "--profile-at steps"
// takes one profile in each step, its 30 s CPU profile ending 15 s before
// the step does, so the last passing step and the first failing one both
// have one, whichever they turn out to be.
func TestRampProfilesRelativeToItsSteps(t *testing.T) {
	env := fakeHost(t, idleHost())
	out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "ramp", "--load", "1", "--warmup", "1m", "--minutes", "15",
		"--profile-at", "steps,1m,60s,255s", "--steps", "1,2,3", "--step-duration", "5m")
	if code == 0 || !strings.Contains(out, "the app is not running") ||
		!strings.Contains(out, "profile-at steps: 255s (step 1) 555s (step 2) 855s (step 3)") ||
		!strings.Contains(out, "profile schedule: 1m 255s 555s 855s\n") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	// A step past the measurement gets none.
	out, _ = runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "ramp", "--load", "1", "--warmup", "1m", "--minutes", "10",
		"--profile-at", "steps", "--steps", "1,2,3", "--step-duration", "5m")
	if !strings.Contains(out, "profile-at steps: 255s (step 1) 555s (step 2)\n") {
		t.Fatalf("output %q", out)
	}
	for _, args := range [][]string{
		{"--scenario", "soak", "--load", "1", "--warmup", "1m", "--minutes", "5", "--profile-at", "steps"},
		{"--scenario", "ramp", "--load", "1", "--warmup", "1m", "--minutes", "5", "--profile-at", "steps", "--step-duration", "1m"},
		{"--scenario", "ramp", "--load", "1", "--warmup", "1m", "--minutes", "5", "--profile-at", "steps", "--steps", "1,2", "--step-duration", "40s"},
		{"--scenario", "ramp", "--load", "1", "--warmup", "1m", "--minutes", "5", "--profile-at", "steps", "--steps", "1,2", "--step-duration", "2m30s"},
	} {
		if out, code := runLoadNode(t, env, append([]string{"run", "--run", env.Run}, args...)...); code != 2 {
			t.Errorf("%v: exit %d, output %q", args, code, out)
		}
	}
}

// Task 11 (Task 1 carry): the audit takes the run's host.json, so a
// contaminated run fails, and the run's thresholds; it refuses to audit a
// run without a host record and prints the verdict it wrote.
func TestAuditPassesTheHostVerdictAndThresholds(t *testing.T) {
	env := fakeHost(t, idleHost()).With("FAKE_AUDIT_VERDICT", `{"pass":false,"reasons":["host contaminated: xcodebuild"],"gates":{}}`)
	writeFile(t, filepath.Join(env.Run, "result.json"), `{"routes":{}}`)
	out, code := runLoadNode(t, env, "audit", "--run", env.Run)
	if code == 0 || !strings.Contains(out, "no host.json") {
		t.Fatalf("audit without host.json: exit %d, output %q", code, out)
	}
	if log, _ := os.ReadFile(env.Get("FAKE_LOG")); strings.Contains(string(log), "loadgen audit") {
		t.Fatalf("a refused audit still ran the generator:\n%s", log)
	}
	writeFile(t, filepath.Join(env.Run, "host.json"), `{"contaminated":true,"offenders":["xcodebuild"],"details":[],"max_load1":5,"samples":6}`)
	writeFile(t, filepath.Join(env.Run, "run-idle.json"), `{}`)
	writeFile(t, filepath.Join(env.Run, "private", "ledger.jsonl"), "")
	writeFile(t, filepath.Join(env.Run, "private", "tracked.jsonl"), "")
	out, code = runLoadNode(t, env, "audit", "--run", env.Run)
	if code != 0 || !strings.Contains(out, "verdict: FAIL") || !strings.Contains(out, "host contaminated: xcodebuild") {
		t.Fatalf("audit: exit %d, output %q", code, out)
	}
	log, _ := os.ReadFile(env.Get("FAKE_LOG"))
	want := "run --rm --no-deps -T loadgen audit --superuser-url-file /run/load/private/secrets/superuser_database_url " +
		"--ledger /run/load/private/ledger.jsonl --tracked /run/load/private/tracked.jsonl --out /run/load/audit.json " +
		"--result /run/load/result.json --host /run/load/host.json --verdict /run/load/verdict.json\n"
	if !strings.Contains(string(log), want) {
		t.Fatalf("fake log lacks %q:\n%s", want, log)
	}
	writeFile(t, filepath.Join(env.Run, "thresholds.json"), `{}`)
	if out, code = runLoadNode(t, env, "audit", "--run", env.Run); code != 0 {
		t.Fatalf("audit with thresholds: exit %d, output %q", code, out)
	}
	log, _ = os.ReadFile(env.Get("FAKE_LOG"))
	if !strings.Contains(string(log), "--verdict /run/load/verdict.json --thresholds /run/load/thresholds.json") {
		t.Fatalf("the audit was not given the run's thresholds:\n%s", log)
	}
	// An audit that wrote no verdict fails the subcommand.
	if out, code = runLoadNode(t, env.With("FAKE_AUDIT_VERDICT", ""), "audit", "--run", env.Run); code == 0 || !strings.Contains(out, "no verdict.json") {
		t.Fatalf("audit without a verdict: exit %d, output %q", code, out)
	}
}

func TestEmptyHostSamplesAreNeverClean(t *testing.T) {
	out, code := runEvaluateHost(t, "")
	if code != 0 || !strings.Contains(out, `"contaminated":true`) || !strings.Contains(out, "no host samples") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func TestRunnerRefusesInheritedConfigurationAndForeignPaths(t *testing.T) {
	onEachHost(t, testRunnerRefusesInheritedConfigurationAndForeignPaths)
}

func testRunnerRefusesInheritedConfigurationAndForeignPaths(t *testing.T, host string) {
	env := fakeHost(t, on(host, idleHost()))
	if out, code := runLoadNode(t, env.With("BT_DATABASE_URL", "postgres://elsewhere"), "preflight", "--families", "100"); code == 0 ||
		!strings.Contains(out, "refusing inherited BT_DATABASE_URL") {
		t.Fatalf("inherited BT_*: exit %d, output %q", code, out)
	}
	root := loadRoot(host)
	// The other host's root is foreign too: /tmp/bt-load on the Mac (where
	// /tmp is a symlink), /private/tmp/bt-load on Linux.
	other := map[string]string{"darwin": "/tmp/bt-load/x", "linux": "/private/tmp/bt-load/x"}[host]
	for _, run := range []string{other, "/private/tmp/elsewhere/x", root + "/a/b", root + "/../x", "relative"} {
		if out, code := runLoadNode(t, env.With("BT_LOAD_RUN", run), "preflight", "--families", "100"); code == 0 ||
			!strings.Contains(out, "runs live only under "+root+"/") {
			t.Fatalf("run %q: exit %d, output %q", run, code, out)
		}
	}
	target := t.TempDir()
	link := filepath.Join(root, "link-"+strconv.Itoa(os.Getpid()))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(link) })
	if out, code := runLoadNode(t, env.With("BT_LOAD_RUN", link), "preflight", "--families", "100"); code == 0 ||
		!strings.Contains(out, "symlinked output refused") {
		t.Fatalf("symlinked run: exit %d, output %q", code, out)
	}
	if err := os.Symlink(target, filepath.Join(env.Run, "idle.json")); err != nil {
		t.Fatal(err)
	}
	if out, code := runLoadNode(t, env, "preflight", "--families", "100"); code == 0 || !strings.Contains(out, "symlinked output refused") {
		t.Fatalf("symlinked idle.json: exit %d, output %q", code, out)
	}
}

func TestHostEvaluationIgnoresShortBurstsAndCountsForeignCommands(t *testing.T) {
	// A 30-second burst of an ordinary process, OrbStack at 300% all along
	// (the measured stack itself) and a single sighting of a compile: clean.
	var lines []string
	for i := 0; i < 12; i++ {
		top := `{"pcpu":300.0,"comm":"/Applications/OrbStack.app/Contents/Frameworks/OrbStack Helper.app/Contents/MacOS/OrbStack Helper"}`
		if i < 3 {
			top += `,{"pcpu":95.0,"comm":"/usr/libexec/mds_stores"}`
		}
		if i == 5 {
			top += `,{"pcpu":40.0,"comm":"/usr/local/go/pkg/tool/darwin_arm64/compile"}`
		}
		lines = append(lines, `{"at":"x","loadavg":"{ 2.50 2.00 1.00 }","top":[`+top+`],"foreign_commands":[],"containers":[]}`)
	}
	out, code := runEvaluateHost(t, strings.Join(lines, "\n")+"\n")
	if code != 0 || !strings.Contains(out, `"contaminated":false`) || !strings.Contains(out, `"max_load1":2.5`) {
		t.Fatalf("exit %d, output %q", code, out)
	}
	// A go test seen in two samples (different executables: a build's
	// processes change every sample), or another project's container at 60%
	// for a minute, contaminates.
	lines = lines[:0]
	for i := 0; i < 8; i++ {
		foreign := `[]`
		if i == 2 {
			foreign = `[{"rule":"go test","exe":"go","pcpu":0.4}]`
		}
		if i == 6 {
			foreign = `[{"rule":"go test","exe":"sync.test","pcpu":90.0}]`
		}
		lines = append(lines, `{"at":"x","load1":1.0,"top":[],"foreign_commands":`+foreign+`,"containers":[{"name":"other-db-1","pcpu":60.0}]}`)
	}
	out, code = runEvaluateHost(t, strings.Join(lines, "\n")+"\n")
	if code != 0 || !strings.Contains(out, `"contaminated":true`) || !strings.Contains(out, `"name":"go test"`) ||
		!strings.Contains(out, `"example":"go"`) || !strings.Contains(out, "container other-db-1") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	// One sighting of a foreign command is not contamination.
	out, code = runEvaluateHost(t, `{"at":"x","load1":1.0,"top":[],"foreign_commands":[{"rule":"xcodebuild","exe":"xcodebuild","pcpu":1.0}],"containers":[]}`+"\n")
	if code != 0 || !strings.Contains(out, `"contaminated":false`) {
		t.Fatalf("single sighting: exit %d, output %q", code, out)
	}
}

// --- helpers -----------------------------------------------------------------

type fakeHostOptions struct {
	LoadAvg   string // sysctl -n vm.loadavg ("{ 1 5 15 }"); on Linux /proc/loadavg's first three fields
	PS        string // "%CPU command" lines: ps -Ao pcpu=,comm= -r on the Mac, top's second frame on Linux
	DFAvailGB int    // df -Pk / column 4, in GiB
	OS        string // "" or "darwin": the Mac; "linux": the Linux VM (uname -s, /proc, top, lscpu)
}

// The fake hosts every host-touching test runs on: the Mac under OrbStack
// and the Linux load VM.
var hostModes = []string{"darwin", "linux"}

// onEachHost runs fn as a subtest per fake host.
func onEachHost(t *testing.T, fn func(t *testing.T, host string)) {
	t.Helper()
	for _, host := range hostModes {
		t.Run(host, func(t *testing.T) { fn(t, host) })
	}
}

// on returns options for the named host.
func on(host string, options fakeHostOptions) fakeHostOptions {
	options.OS = host
	return options
}

// The fake Linux VM: an AMD Ryzen 7 7800X3D Hyper-V guest, 12 vCPU, 16 GiB.
const (
	linuxLabel    = "AMD Ryzen 7 7800X3D (Hyper-V VM, 12 vCPU, 16 GiB), Docker CPU caps"
	linuxMemTotal = 16365432 * 1024 // /proc/meminfo MemTotal 16365432 kB
	linuxLscpu    = `Architecture:             x86_64
  CPU op-mode(s):         32-bit, 64-bit
CPU(s):                   12
  On-line CPU(s) list:    0-11
Vendor ID:                AuthenticAMD
  Model name:             AMD Ryzen 7 7800X3D 8-Core Processor
    CPU family:           25
    Thread(s) per core:   2
    Core(s) per socket:   6
    Socket(s):            1
Virtualization features:
  Hypervisor vendor:      Microsoft
  Virtualization type:    full
NUMA:
  NUMA node(s):           1
  NUMA node0 CPU(s):      0-11
`
)

// linuxCPUInfo is /proc/cpuinfo for n logical CPUs of the fake VM.
func linuxCPUInfo(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "processor\t: %d\nvendor_id\t: AuthenticAMD\nmodel name\t: AMD Ryzen 7 7800X3D 8-Core Processor\n"+
			"physical id\t: 0\nsiblings\t: 12\ncore id\t\t: %d\ncpu cores\t: 6\nflags\t\t: fpu vme de pse tsc hypervisor lahf_lm\n\n", i, i/2)
	}
	return b.String()
}

// inContainer makes the fake Linux process with that pid (top rows are
// numbered 2000 + their line in PS) a Docker container's.
func inContainer(t *testing.T, env loadEnv, pid int) {
	t.Helper()
	dir := filepath.Join(env.Get("BT_LOAD_PROC_ROOT"), strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "cgroup"), "0::/system.slice/docker-"+strings.Repeat("ab12", 16)+".scope\n")
}

// withoutLoadAverage makes the host's load average unreadable: a failing
// sysctl on the Mac, a missing /proc/loadavg on Linux.
func withoutLoadAverage(t *testing.T, env loadEnv) loadEnv {
	t.Helper()
	if env.OS != "linux" {
		return env.With("FAKE_SYSCTL_FAIL", "1")
	}
	if err := os.Remove(filepath.Join(env.Get("BT_LOAD_PROC_ROOT"), "loadavg")); err != nil {
		t.Fatal(err)
	}
	return env
}

// loadRoot is where the runner keeps runs on the host.
func loadRoot(host string) string {
	if host == "linux" {
		return "/tmp/bt-load"
	}
	return "/private/tmp/bt-load"
}

// hostLabel is the label the runner computes for the fake host.
func hostLabel(host string) string {
	if host == "linux" {
		return linuxLabel
	}
	return "Apple M4, Docker CPU caps"
}

func idleHost() fakeHostOptions {
	return fakeHostOptions{LoadAvg: "{ 0.60 0.70 0.80 }", PS: "  2.0 Terminal\n  1.5 /Applications/OrbStack.app/Contents/MacOS/OrbStack\n", DFAvailGB: 120}
}

type loadEnv struct {
	TempDir string
	Run     string
	OS      string // "darwin" or "linux"
	vars    map[string]string
}

// With returns a copy of the environment with key set to value.
func (e loadEnv) With(key, value string) loadEnv {
	vars := make(map[string]string, len(e.vars)+1)
	for k, v := range e.vars {
		vars[k] = v
	}
	vars[key] = value
	e.vars = vars
	return e
}

func (e loadEnv) Get(key string) string { return e.vars[key] }

func (e loadEnv) environ() []string {
	keys := make([]string, 0, len(e.vars))
	for k := range e.vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	environ := make([]string, 0, len(keys))
	for _, k := range keys {
		environ = append(environ, k+"="+e.vars[k])
	}
	return environ
}

// fakeHost writes the fake tools, a run directory under the host's load root
// (/private/tmp/bt-load on the Mac, /tmp/bt-load on Linux: the only place
// runs may live) that is already "up" (private/secrets and source-sha exist),
// and a lock held for that run by this test process, so subcommands that
// require the lock find it. A Linux host also gets a fake /proc
// (BT_LOAD_PROC_ROOT: loadavg, cpuinfo, meminfo) and os-release.
//
// Every fake tool appends its command line to $FAKE_LOG under a lock file
// (noclobber, a builtin: no fork per try), so the background samplers'
// concurrent lines never interleave mid-line.
func fakeHost(t *testing.T, options fakeHostOptions) loadEnv {
	t.Helper()
	host := options.OS
	if host == "" {
		host = "darwin"
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range fakeTools {
		writeExecutable(t, filepath.Join(bin, name), "#!/bin/bash\n"+
			"set -C; fake_n=0\n"+
			"until { : > \"$FAKE_LOG.lock\"; } 2>/dev/null; do fake_n=$((fake_n + 1)); ((fake_n < 200000)) || break; done\n"+
			"set +C\n"+
			"printf '%s\\n' \""+name+" $*\" >> \"$FAKE_LOG\"\n"+
			"/bin/rm -f \"$FAKE_LOG.lock\"\n"+body)
	}
	root := loadRoot(host)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	run, err := os.MkdirTemp(root, "test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	if err := os.MkdirAll(filepath.Join(run, "private", "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(run, "source-sha"), strings.Repeat("a", 40))
	lock := filepath.Join(dir, "held.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(lock, "pid"), strconv.Itoa(os.Getpid()))
	writeFile(t, filepath.Join(lock, "run"), run)
	env := loadEnv{TempDir: dir, Run: run, OS: host, vars: map[string]string{
		"PATH":                  bin + ":/usr/bin:/bin",
		"HOME":                  dir,
		"TMPDIR":                dir,
		"FAKE_LOG":              filepath.Join(dir, "fake.log"),
		"FAKE_LOADAVG":          options.LoadAvg,
		"FAKE_PS":               options.PS,
		"FAKE_DF_AVAIL":         strconv.Itoa(options.DFAvailGB),
		"BT_LOAD_RUN":           run,
		"BT_LOAD_LOCK":          lock,
		"BT_LOAD_IDLE_SAMPLES":  "1",
		"BT_LOAD_IDLE_INTERVAL": "0",
		"BT_LOAD_BURN_DELAY":    "0",
		"BT_LOAD_BURN_GAP":      "0",
		"BT_LOAD_HOT_GAP":       "0",
		"BT_LOAD_LOG_DRAIN":     "0",
	}}
	if host == "linux" {
		proc := filepath.Join(dir, "proc")
		if err := os.Mkdir(proc, 0o700); err != nil {
			t.Fatal(err)
		}
		loadavg := strings.TrimSpace(strings.NewReplacer("{", "", "}", "").Replace(options.LoadAvg))
		writeFile(t, filepath.Join(proc, "loadavg"), loadavg+" 1/412 98765\n")
		writeFile(t, filepath.Join(proc, "cpuinfo"), linuxCPUInfo(12))
		writeFile(t, filepath.Join(proc, "meminfo"), "MemTotal:       16365432 kB\nMemFree:        12000000 kB\nMemAvailable:   14000000 kB\n")
		writeFile(t, filepath.Join(dir, "os-release"), "PRETTY_NAME=\"Ubuntu 24.04.3 LTS\"\nNAME=\"Ubuntu\"\nVERSION_ID=\"24.04\"\n")
		for key, value := range map[string]string{
			"FAKE_UNAME_S":       "Linux",
			"FAKE_UNAME_R":       "7.0.0-1004-azure",
			"FAKE_LSCPU":         linuxLscpu,
			"FAKE_DOCKER_INFO":   "12 " + strconv.Itoa(linuxMemTotal) + " 2 systemd 29.9.0",
			"BT_LOAD_PROC_ROOT":  proc,
			"BT_LOAD_OS_RELEASE": filepath.Join(dir, "os-release"),
		} {
			env = env.With(key, value)
		}
	}
	return env
}

var fakeTools = map[string]string{
	"docker": `
last=${!#}
case $1 in
  compose)
    # The user the overlay runs the app and the generator as.
    printf '%s\n' "${BT_LOAD_CONTAINER_USER-unset}" > "$FAKE_LOG.user"
    shift
    while [[ $# -gt 0 && $1 == -* ]]; do
      case $1 in --env-file|--project-name|-p|--project-directory|-f|--file|--profile) shift 2 ;; *) shift ;; esac
    done
    case $1 in
      run)
        # The generator: warm-up ends at once, then a short measurement.
        # FAKE_GENERATOR_NO_WINDOW: a generator that never ends its warm-up
        # and runs until "docker stop" reaches its container.
        if [[ " $* " == *" loadgen run "* ]]; then
          if [[ -n ${FAKE_GENERATOR_NO_WINDOW:-} ]]; then
            for _ in $(seq 1 300); do [[ -e $FAKE_LOG.stopped ]] && exit 137; sleep 0.1; done
            exit 0
          fi
          # FAKE_WINDOW_START_FRACTION: window-start as the generator writes
          # it, RFC 3339 with that fraction of the current second.
          if [[ -n ${FAKE_WINDOW_START_FRACTION:-} ]]; then
            printf '%s.%sZ\n' "$(date -u +%Y-%m-%dT%H:%M:%S)" "$FAKE_WINDOW_START_FRACTION" > "$BT_LOAD_RUN/window-start"
          else
            touch "$BT_LOAD_RUN/window-start"
          fi
          # The generator creates its ledger and tracked devices; each
          # holds this generator's pid, so a test can tell measurements apart.
          if [[ -d $BT_LOAD_RUN/private ]]; then
            for f in ledger.jsonl tracked.jsonl; do
              [[ ! -e $BT_LOAD_RUN/private/$f ]] || { echo "fake generator: private/$f exists" >&2; exit 9; }
              printf '%s\n' "$$" > "$BT_LOAD_RUN/private/$f"
            done
          fi
          # FAKE_GENERATOR_APNS_READS: run until the APNs sink was read that
          # many times (at most 20 s), so the samples a test asserts exist
          # however busy the machine; else FAKE_GENERATOR_SECONDS.
          # FAKE_GENERATOR_FOLLOWS: run until the app's log was followed that
          # many times (at most 20 s).
          if [[ -n ${FAKE_GENERATOR_FOLLOWS:-} ]]; then
            for _ in $(seq 1 200); do
              (( $(grep -c 'docker logs --follow' "$FAKE_LOG" 2>/dev/null || echo 0) >= FAKE_GENERATOR_FOLLOWS )) && break
              /bin/sleep 0.1
            done
          elif [[ -n ${FAKE_GENERATOR_APNS_READS:-} ]]; then
            for _ in $(seq 1 200); do
              (( $(cat "$FAKE_LOG.apns" 2>/dev/null || echo 0) >= FAKE_GENERATOR_APNS_READS )) && break
              /bin/sleep 0.1
            done
          else
            sleep "${FAKE_GENERATOR_SECONDS:-2}"
          fi
          if [[ -n ${FAKE_RESULT:-} ]]; then printf '%s\n' "$FAKE_RESULT"; else printf '{"fake":true}\n'; fi > "$BT_LOAD_RUN/result.json"
          exit "${FAKE_GENERATOR_EXIT:-0}"
        fi
        # The seed: FAKE_SEED_EXIT is its exit status.
        if [[ " $* " == *" loadgen seed "* ]]; then
          printf '{"Families":1}\n' > "$BT_LOAD_RUN/seed-report.json"
          exit "${FAKE_SEED_EXIT:-0}"
        fi
        # The audit: FAKE_AUDIT_VERDICT is the verdict.json it writes.
        if [[ " $* " == *" loadgen audit "* && -n ${FAKE_AUDIT_VERDICT:-} ]]; then
          printf '%s\n' "$FAKE_AUDIT_VERDICT" > "$BT_LOAD_RUN/verdict.json"
        fi
        # The migration: FAKE_MIGRATE_EXIT is its exit status; one that
        # succeeds leaves the schema at FAKE_SCHEMA_AFTER (37).
        if [[ " $* " == *" migrate " ]]; then
          if [[ ${FAKE_MIGRATE_EXIT:-0} != 0 ]]; then echo "fake migrate: migration_failed 000037" >&2; exit "$FAKE_MIGRATE_EXIT"; fi
          printf '%s\n' "${FAKE_SCHEMA_AFTER:-37}" > "$FAKE_LOG.schema"
        fi ;;
      # Each build makes new images (see image inspect); FAKE_BUILD_EXIT fails it.
      build)
        [[ ${FAKE_BUILD_EXIT:-0} == 0 ]] || exit "$FAKE_BUILD_EXIT"
        echo $(( $(cat "$FAKE_LOG.built" 2>/dev/null || echo 0) + 1 )) > "$FAKE_LOG.built" ;;
      ps) printf 'fake-%s\n' "$last" ;;
      port) printf '127.0.0.1:1\n' ;;
      restart) [[ ! -f $FAKE_LOG.walpending ]] || cp "$FAKE_LOG.walpending" "$FAKE_LOG.wal" ;;
      # Stopping the app ends its "logs --follow" (the restart scenario).
      # FAKE_COMPOSE_STOP_SECONDS: how long the graceful stop takes.
      stop)
        [[ ! -f $FAKE_LOG.logspid ]] || kill "$(cat "$FAKE_LOG.logspid")" 2>/dev/null || true
        /bin/sleep "${FAKE_COMPOSE_STOP_SECONDS:-0}" ;;
      config)
        if [[ -n ${FAKE_COMPOSE_CONFIG:-} ]]; then printf '%s\n' "$FAKE_COMPOSE_CONFIG"; else printf '{"services":{"loadgen":{"image":"x"}}}\n'; fi ;;
    esac ;;
  inspect)
    case $3 in
      *NanoCpus*)
        case $last in
          *loadgen*) printf '%s\n' "${FAKE_INSPECT_LOADGEN:-0 0 0}" ;;
          *app*) printf '%s\n' "${FAKE_INSPECT_APP:-4000000000 8589934592 8589934592}" ;;
          *db*) printf '%s\n' "${FAKE_INSPECT_DB:-4000000000 8589934592 8589934592}" ;;
        esac ;;
      *State.Running*) printf '%s\n' "${FAKE_APP_RUNNING:-false}" ;;
      # The app's start time: its k-th read is FAKE_STARTED_AT_<k>, else
      # FAKE_STARTED_AT (a restart scenario changes it).
      *State.StartedAt*)
        k=$(( $(cat "$FAKE_LOG.started" 2>/dev/null || echo 0) + 1 ))
        echo "$k" > "$FAKE_LOG.started"
        eval "s=\${FAKE_STARTED_AT_$k-\${FAKE_STARTED_AT:-2026-10-24T10:00:00.000000000Z}}"
        printf '%s\n' "$s" ;;
      *NetworkSettings*) printf '%s\n' "${FAKE_LOADGEN_ADDRESS-172.31.250.100}" ;;
      # The app's environment, one variable a line (FAKE_APP_ENV).
      *Config.Env*) [[ -z ${FAKE_INSPECT_ENV_FAIL:-} ]] || exit 1; printf '%b\n' "${FAKE_APP_ENV:-BT_MODE=hosted}" ;;
      # The db's command line, one argument a line (FAKE_DB_CMD).
      *Config.Cmd*) printf '%b\n' "${FAKE_DB_CMD:-postgres\n-c\nlog_autovacuum_min_duration=1s}" ;;
    esac ;;
  image)
    # An image's ID names the builds made so far: sha256:b<builds>-<name>.
    if [[ " $* " == *".Id"* ]]; then printf 'sha256:b%s-%s\n' "$(cat "$FAKE_LOG.built" 2>/dev/null || echo 0)" "${last%%:*}"; fi
    exit "${FAKE_IMAGE_MISSING:-0}" ;;
  stop) touch "$FAKE_LOG.stopped" ;;
  exec)
    case "$*" in
      # wal_buffers: ALTER SYSTEM takes effect at the next "compose restart".
      *"ALTER SYSTEM SET wal_buffers = '"*)
        v="$*"; v=${v#*wal_buffers = \'}; printf '%s\n' "${v%%\'*}" > "$FAKE_LOG.walpending" ;;
      *"ALTER SYSTEM RESET wal_buffers"*) printf '16MB\n' > "$FAKE_LOG.walpending" ;;
      *"SHOW wal_buffers"*) cat "$FAKE_LOG.wal" 2>/dev/null || printf '16MB\n' ;;
      # FAKE_AUTO_CONF_WAL: postgresql.auto.conf still sets wal_buffers.
      *pg_file_settings*) printf '%s\n' "${FAKE_AUTO_CONF_WAL:-0}" ;;
      *bt-load-burn*) [[ $* == *"burn ${BT_LOAD_BURN_GAP:-3}"* ]] || exit 1; b=${FAKE_BURN_CPU:-380.00%}; printf '%s\n' "${b%\%}" ;;
      *cgroup*) printf '%s\n%s\n' "${FAKE_CGROUP_CPU:-400000 100000}" "${FAKE_CGROUP_MEMORY:-8589934592}" ;;
      *shobj_description*) printf '%s\n' "${FAKE_DB_MARKER-baby-tracker-load-stack}" ;;
      # The cluster counters: the k-th snapshot's are k times the first's,
      # 30 s apart.
      *pg_stat_wal*)
        k=$(( $(cat "$FAKE_LOG.cluster" 2>/dev/null || echo 0) + 1 ))
        echo "$k" > "$FAKE_LOG.cluster"
        printf '{"at":"t%s","epoch":%s.5,"settings":{"track_wal_io_timing":"off"},"wal":{"wal_records":%s,"wal_fpi":%s,"wal_bytes":%s,"wal_buffers_full":%s,"stats_reset":"r"},"checkpointer":{"num_timed":%s,"write_time":%s.25,"stats_reset":"r"},"bgwriter":{"buffers_alloc":%s,"stats_reset":"r"}}\n' \
          "$k" $((1000 + 30 * k)) $((100 * k)) $((7 * k)) $((8192 * k)) $((2 * k)) "$k" $((10 * k)) $((50 * k)) ;;
      *consumed_at*) printf '{"at":"t","due":%s,"ms":3.1}\n' "${FAKE_BACKLOG_DUE:-42}" ;;
      # The schema version: FAKE_SCHEMA_BEFORE (36) until a migration ran.
      *schema_migrations*) cat "$FAKE_LOG.schema" 2>/dev/null || printf '%s\n' "${FAKE_SCHEMA_BEFORE:-36}" ;;
      *"SHOW track_wal_io_timing"*) printf 'on\n' ;;
      *"SHOW log_autovacuum_min_duration"*) printf '%s\n' "${FAKE_DB_AUTOVACUUM_LOG:-1s}" ;;
      *"SHOW bgwriter_lru_maxpages"*) printf '1000\n' ;;
      *"SHOW bgwriter_delay"*) printf '50ms\n' ;;
      *pg_locks*) printf '{"at":"t","blocked":[]}\n{"at":"t","waits":[{"type":"CPU","event":"running","count":3}]}\n' ;;
      *"FROM pg_stat_statements"*) printf '{"settings":{"track_planning":"on","track_io_timing":"on","track_wal_io_timing":"on"},"top":[{"queryid":"1","calls":10,"query":"SELECT 1"}]}\n' ;;
    esac ;;
  stats)
    case "$*" in
      *json*)
        printf '{"Name":"bt-load-app-1","CPUPerc":"150.00%%","MemUsage":"1.5GiB / 8GiB"}\n'
        printf '{"Name":"bt-load-db-1","CPUPerc":"90.00%%","MemUsage":"2GiB / 8GiB"}\n' ;;
      *.Name*) [[ -z ${FAKE_DOCKER_STATS_FAIL:-} ]] || exit 1; printf '%s' "${FAKE_DOCKER_STATS:-}" ;;
      *) printf '%s\n' "${FAKE_BURN_CPU:-380.00%}" ;;
    esac ;;
  info) printf '%s\n' "${FAKE_DOCKER_INFO:-10 12599472128 2 cgroupfs 28.5.2}" ;;
  ps)
    case "$*" in
      *service=loadgen*) printf '%s\n' "${FAKE_LOADGEN_ID-fake-loadgen-run}" ;;
      *.Names*) printf '%s' "${FAKE_DOCKER_NAMES:-}" ;;
      *) printf '%s' "${FAKE_PROJECT_CONTAINERS:-}" ;;
    esac ;;
  logs)
    # "logs --follow" streams until it is killed (FAKE_LOGS_FOLLOW_ENDS: ends
    # at once, as for a stopped container). FAKE_APP_LOG_LATE is a line the
    # app writes after the runner stopped sampling.
    if [[ " $* " == *" --follow "* ]]; then
      printf '%s\n' "$$" > "$FAKE_LOG.logspid"
      printf '%s' "${FAKE_APP_LOG:-}"
      [[ -z ${FAKE_LOGS_FOLLOW_ENDS:-} ]] || exit 0
      if [[ -n ${FAKE_APP_LOG_LATE:-} ]]; then
        while [[ -e $BT_LOAD_RUN/.sampling ]]; do /bin/sleep 0.1; done
        /bin/sleep 0.3
        printf '%s' "$FAKE_APP_LOG_LATE"
      fi
      exec /bin/sleep 30
    fi
    printf '%s' "${FAKE_APP_LOG:-}" ;;
  run) printf 'fakeburn\n' ;;
esac
exit 0
`,
	"sysctl": `
[[ $1 == -n ]] && shift
for key in "$@"; do
  case $key in
    vm.loadavg) [[ -z ${FAKE_SYSCTL_FAIL:-} ]] || exit 1; printf '%s\n' "$FAKE_LOADAVG" ;;
    hw.memsize) printf '25769803776\n' ;;
    hw.ncpu) printf '10\n' ;;
    machdep.cpu.brand_string) printf 'Apple M4\n' ;;
  esac
done
`,
	"ps": `
# The process list: call n prints FAKE_PS_<n> when set, else FAKE_PS (the
# idle check reads it three times for the sustained above-50% rule).
case "$*" in
  *comm=*)
    n=$(( $(cat "$FAKE_LOG.ps" 2>/dev/null || echo 0) + 1 ))
    echo "$n" > "$FAKE_LOG.ps"
    eval "out=\${FAKE_PS_$n-\$FAKE_PS}"
    printf '%s' "$out" ;;
  *args=*) [[ -z ${FAKE_PS_FAIL:-} ]] || exit 1; printf '%s' "${FAKE_PS_ARGS:-}" ;;
esac
`,
	"mv": `
# FAKE_MV_FAIL: another preflight moved the stale lock first.
if [[ -n ${FAKE_MV_FAIL:-} && $1 == *.lock ]]; then exit 1; fi
# FAKE_MV_RACE_PID: between this preflight's staleness check and its move,
# another run took the lock over (a live pid, its own run).
if [[ -n ${FAKE_MV_RACE_PID:-} && $1 == *.lock && $2 == *.stale.* ]]; then
  printf '%s\n' "$FAKE_MV_RACE_PID" > "$1/pid"
  printf '/private/tmp/bt-load/the-racer\n' > "$1/run"
  rm -f "$1/expires"
fi
exec /bin/mv "$@"
`,
	"sleep": `
# FAKE_SLEEP_INSTANT: record the requested pause and return at once.
if [[ -n ${FAKE_SLEEP_INSTANT:-} ]]; then exit 0; fi
exec /bin/sleep "$@"
`,
	"df": `
# POSIX output (df -Pk): 1024-byte blocks, as BSD and GNU df both print it.
[[ " $* " == *" -Pk "* ]] || { echo "fake df: only df -Pk is portable" >&2; exit 1; }
printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\n'
printf '/dev/disk3s1s1 482344960 12582912 %s 12%% /\n' "$((FAKE_DF_AVAIL * 1048576))"
`,
	"uname": `
case $1 in
  -s) printf '%s\n' "${FAKE_UNAME_S:-Darwin}" ;;
  -r) printf '%s\n' "${FAKE_UNAME_R:-25.0.0}" ;;
  *) exec /usr/bin/uname "$@" ;;
esac
`,
	"top": `
# procps top -b -n 2 -d W (Linux): two frames; the first counts nothing (the
# rate since the previous refresh), the second holds the process list: call n
# prints FAKE_PS_<n> when set, else FAKE_PS (the counter ps uses on the Mac),
# one row per "%CPU command" line, pid 2000 + its line number.
[[ -z ${FAKE_TOP_FAIL:-} ]] || exit 1
n=$(( $(cat "$FAKE_LOG.ps" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$FAKE_LOG.ps"
eval "list=\${FAKE_PS_$n-\$FAKE_PS}"
for frame in 1 2; do
  printf 'top - 12:00:00 up 1 day,  1:00,  1 user,  load average: 0.60, 0.70, 0.80\n'
  printf 'Tasks: 300 total,   1 running, 299 sleeping,   0 stopped,   0 zombie\n'
  printf '%%Cpu(s):  1.0 us,  0.5 sy,  0.0 ni, 98.5 id,  0.0 wa,  0.0 hi,  0.0 si,  0.0 st\n'
  printf 'MiB Mem :  15981.9 total,  12000.0 free,   2000.0 used,   1981.9 buff/cache\n'
  printf 'MiB Swap:      0.0 total,      0.0 free,      0.0 used.  13981.9 avail Mem\n\n'
  printf '    PID USER      PR  NI    VIRT    RES    SHR S  %%CPU  %%MEM     TIME+ COMMAND\n'
  i=0
  while IFS= read -r line; do
    [[ -n ${line// /} ]] || continue
    i=$((i + 1))
    read -r pcpu command <<<"$line"
    [[ $frame == 2 ]] || pcpu=0.0
    printf '%7d runner    20   0  100000  10000   1000 S %5s   0.1   0:01.00 %s\n' $((2000 + i)) "$pcpu" "$command"
  done <<<"$list"
  [[ $frame == 2 ]] || printf '\n'
done
`,
	"lscpu": `
[[ -z ${FAKE_LSCPU_FAIL:-} ]] || exit 1
printf '%s' "$FAKE_LSCPU"
`,
	"orb": `
printf 'cpu: 10\nmemory_mib: 12288\nrosetta: true\n'
`,
	"curl": `
# The APNs sink (GET /__identitytest/apns?after=N): its k-th call answers
# $FAKE_APNS_DIR/k.json (else last.json), or FAKE_APNS_JSON, holding the
# recorder's whole ring: the fake keeps the records numbered past N, as the
# recorder does.
body=profile
for arg in "$@"; do
  if [[ $arg == *"/__identitytest/apns?after="* ]]; then
    after=${arg##*after=}
    n=$(( $(cat "$FAKE_LOG.apns" 2>/dev/null || echo 0) + 1 ))
    echo "$n" > "$FAKE_LOG.apns"
    src=${FAKE_APNS_JSON:-'{"requests":[],"total":0,"in_flight_peak":0}'}
    if [[ -n ${FAKE_APNS_DIR:-} ]]; then
      # k.fail: the k-th read fails.
      [[ ! -f $FAKE_APNS_DIR/$n.fail ]] || exit 22
      if [[ -f $FAKE_APNS_DIR/$n.json ]]; then src=$(cat "$FAKE_APNS_DIR/$n.json"); else src=$(cat "$FAKE_APNS_DIR/last.json"); fi
    fi
    body=$(jq -c --argjson after "$after" '.requests |= map(select(.sequence > $after))' <<<"$src")
  fi
done
while [[ $# -gt 0 ]]; do
  if [[ $1 == -o ]]; then printf '%s\n' "$body" > "$2"; shift; fi
  shift
done
exit "${FAKE_CURL_EXIT:-0}"
`,
}

func runLoadNode(t *testing.T, env loadEnv, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/bash", append([]string{"load-node.sh"}, args...)...)
	command.Env = env.environ()
	out, err := command.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	default:
		t.Fatalf("load-node.sh: %v: %s", err, out)
		return "", -1
	}
}

func runEvaluateHost(t *testing.T, samples string) (string, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host-samples.jsonl")
	writeFile(t, path, samples)
	command := exec.CommandContext(t.Context(), "/bin/bash", "-c", `source lib/load_host.sh && bt_load_evaluate_host "$1"`, "evaluate", path)
	command.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := command.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	} else if err != nil {
		t.Fatalf("evaluate host: %v: %s", err, out)
	}
	return string(out), 0
}

// sectionOf returns the YAML block under the first key named name: the key's
// line and every following line indented deeper than it.
func sectionOf(raw, name string) string {
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if trimmed != name+":" && !strings.HasPrefix(trimmed, name+": ") {
			continue
		}
		indent := len(line) - len(trimmed)
		section := []string{line}
		for _, next := range lines[i+1:] {
			rest := strings.TrimLeft(next, " ")
			if rest != "" && len(next)-len(rest) <= indent {
				break
			}
			section = append(section, next)
		}
		return strings.Join(section, "\n")
	}
	return ""
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, value); err != nil {
		t.Fatalf("%s: %v: %s", path, err, raw)
	}
}

// Task 10 carry: each measurement's private ledger and tracked devices are
// archived with it (runs/<n>/private/), so a second measurement in the same
// run starts its own ledger (the generator never replaces one), and the
// audit reads only the current measurement's files.
func TestRunArchivesThePreviousMeasurementsLedgerAndTracked(t *testing.T) {
	env := runEnv(t).With("FAKE_GENERATOR_SECONDS", "0")
	run := func() {
		t.Helper()
		if out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "0.01", "--warmup", "1s", "--minutes", "1"); code != 0 {
			t.Fatalf("run: exit %d, output %q", code, out)
		}
	}
	read := func(path string) string {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	run()
	first := read(filepath.Join(env.Run, "private", "ledger.jsonl"))
	run() // the fake generator refuses an existing ledger (exit 9)
	for _, name := range []string{"ledger.jsonl", "tracked.jsonl"} {
		if got := read(filepath.Join(env.Run, "runs", "1", "private", name)); got != first {
			t.Fatalf("runs/1/private/%s holds %q, want the first measurement's %q", name, got, first)
		}
		if read(filepath.Join(env.Run, "private", name)) == first {
			t.Fatalf("private/%s is still the first measurement's", name)
		}
	}
	if info, err := os.Stat(filepath.Join(env.Run, "runs", "1", "private")); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("runs/1/private: %v %v", info, err)
	}

	// The audit reads the current measurement's files and refuses without them.
	writeFile(t, filepath.Join(env.Run, "host.json"), `{"contaminated":false,"offenders":[],"details":[],"max_load1":1,"samples":6}`)
	audit := env.With("FAKE_AUDIT_VERDICT", `{"pass":true,"reasons":[],"gates":{}}`)
	if out, code := runLoadNode(t, audit, "audit", "--run", env.Run); code != 0 {
		t.Fatalf("audit: exit %d, output %q", code, out)
	}
	if err := os.Remove(filepath.Join(env.Run, "private", "tracked.jsonl")); err != nil {
		t.Fatal(err)
	}
	if out, code := runLoadNode(t, audit, "audit", "--run", env.Run); code == 0 || !strings.Contains(out, "no private/tracked.jsonl") {
		t.Fatalf("audit without tracked.jsonl: exit %d, output %q", code, out)
	}
	writeFile(t, filepath.Join(env.Run, "private", "tracked.jsonl"), "")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(env.Run, "private", "ledger.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	if out, code := runLoadNode(t, audit, "audit", "--run", env.Run); code == 0 || !strings.Contains(out, "older than this measurement") {
		t.Fatalf("audit of an older ledger: exit %d, output %q", code, out)
	}
	// Without the measurement's run-idle.json nothing dates the files: refused.
	now := time.Now()
	if err := os.Chtimes(filepath.Join(env.Run, "private", "ledger.jsonl"), now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(env.Run, "run-idle.json")); err != nil {
		t.Fatal(err)
	}
	if out, code := runLoadNode(t, audit, "audit", "--run", env.Run); code == 0 || !strings.Contains(out, "no run-idle.json") {
		t.Fatalf("audit without run-idle.json: exit %d, output %q", code, out)
	}
}

// Task 23 Step 2, the runner's half of the restart cell: at window-start +
// --restart-at the app is stopped with a 30 s grace and started again, its
// readiness awaited, and the times recorded in restart.json. Stop and start
// keep the container, so the app-log follower, whose follow the stop ended,
// attaches to the same container again. The runner refuses a restart run
// without a usable --restart-at, and no other scenario restarts the app.
//
// The stop is timed in milliseconds from the generator's own window-start
// (here .999 into its second, so a floored clock would stop the app before
// the declared window): never before window-start + restart-at, plus the
// 250 ms margin. The graceful stop (3 s) outlasts the generator (3 s): the
// runner waits for the restart before it ends, so restart.json and the
// start are there when run returns.
func TestRestartScenarioStopsAndStartsTheAppAndRecordsTheTimes(t *testing.T) {
	env := runEnv(t).With("FAKE_GENERATOR_SECONDS", "3").With("FAKE_COMPOSE_STOP_SECONDS", "3").With("FAKE_WINDOW_START_FRACTION", "999000000").
		With("FAKE_APP_LOG", `{"msg":"capacity resource sample"}`+"\n")
	out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "restart", "--load", "1", "--warmup", "1s", "--minutes", "1",
		"--restart-at", "1s", "--restart-grace", "30s")
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	raw, _ := os.ReadFile(env.Get("FAKE_LOG"))
	log := string(raw)
	generator := strings.Index(log, "loadgen run --base")
	stop := strings.Index(log, " stop -t 30 app\n")
	start := strings.Index(log, " start app\n")
	if generator < 0 || stop < generator || start < stop {
		t.Fatalf("want the generator, then stop -t 30 app, then start app:\n%s", log)
	}
	if !strings.Contains(log, "--restart-at 1s --restart-grace 30s") {
		t.Fatalf("the generator did not get the restart options:\n%s", log)
	}
	var restart struct {
		Status         string   `json:"status"`
		RestartAt      int      `json:"restart_at_seconds"`
		Grace          int      `json:"restart_grace_seconds"`
		Declared       int64    `json:"declared_start_unix_ms"`
		Offset         int64    `json:"stop_offset_ms"`
		WithinGrace    bool     `json:"within_grace"`
		Issued         int64    `json:"stop_issued_unix_ms"`
		Stopped        int64    `json:"stopped_unix_ms"`
		Started        int64    `json:"started_unix_ms"`
		Ready          *int64   `json:"ready_unix_ms"`
		ReadyAt        string   `json:"ready_at"`
		Before         string   `json:"container_before"`
		SameContainer  bool     `json:"same_container"`
		WindowStart    int64    `json:"window_start_unix_ms"`
		StopToReadySec *float64 `json:"stop_to_ready_seconds"`
	}
	readJSON(t, filepath.Join(env.Run, "restart.json"), &restart)
	rawStart, _ := os.ReadFile(filepath.Join(env.Run, "window-start"))
	windowStart, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(rawStart)))
	if err != nil {
		t.Fatal(err)
	}
	declared := windowStart.UnixMilli() + 1000
	if restart.Status != "ok" || restart.RestartAt != 1 || restart.Grace != 30 || restart.Declared != declared || restart.WindowStart != windowStart.UnixMilli() ||
		restart.Issued < declared+250 || restart.Offset < 250 || restart.Offset != restart.Issued-declared || !restart.WithinGrace ||
		restart.Before != "fake-app" || !restart.SameContainer || restart.Ready == nil ||
		restart.Issued > restart.Stopped || restart.Stopped-restart.Issued < 3000 || restart.Stopped > restart.Started || restart.Started > *restart.Ready ||
		restart.StopToReadySec == nil ||
		!regexp.MustCompile(`^20\d\d-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`).MatchString(restart.ReadyAt) {
		t.Fatalf("restart.json = %+v", restart)
	}
	// The runner waited for the restart: restart.json was written before the
	// measurement's own record.
	restartInfo, err := os.Stat(filepath.Join(env.Run, "restart.json"))
	if err != nil {
		t.Fatal(err)
	}
	measurementInfo, err := os.Stat(filepath.Join(env.Run, "measurement.json"))
	if err != nil || restartInfo.ModTime().After(measurementInfo.ModTime()) {
		t.Fatalf("restart.json (%v) was written after measurement.json (%v, %v): the runner did not wait for the restart",
			restartInfo.ModTime(), measurementInfo.ModTime(), err)
	}
	// The follow the stop ended was attached again, to the same container.
	if n := strings.Count(log, "docker logs --follow --since"); n < 2 || strings.Count(log, "fake-app\n") < n {
		t.Fatalf("the follower attached %d time(s):\n%s", n, log)
	}
	followerGone(t, env)

	// restart.json is the measurement's: a second run archives it.
	if out, code := runLoadNode(t, env.With("FAKE_GENERATOR_SECONDS", "0"), "run", "--run", env.Run, "--scenario", "soak", "--load", "1",
		"--warmup", "1s", "--minutes", "1"); code != 0 {
		t.Fatalf("second run: exit %d, output %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(env.Run, "runs", "1", "restart.json")); err != nil {
		t.Fatalf("restart.json was not archived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.Run, "restart.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a soak left a restart.json: %v", err)
	}
	if raw, _ := os.ReadFile(env.Get("FAKE_LOG")); strings.Count(string(raw), " stop -t 30 app\n") != 1 {
		t.Fatalf("the soak restarted the app:\n%s", raw)
	}

	// The hot maintenance's generator gets the stack's superuser URL file.
	if out, code := runLoadNode(t, env.With("FAKE_GENERATOR_SECONDS", "0"), "run", "--run", env.Run, "--scenario", "soak", "--load", "2",
		"--warmup", "1s", "--minutes", "1", "--maintenance", "hot", "--maintenance-at", "20s"); code != 0 {
		t.Fatalf("hot maintenance: exit %d, output %q", code, out)
	}
	if raw, _ := os.ReadFile(env.Get("FAKE_LOG")); !strings.Contains(string(raw),
		"--maintenance hot --maintenance-at 20s --superuser-url-file /run/load/private/secrets/superuser_database_url") {
		t.Fatalf("the hot maintenance's generator lacks the superuser URL file:\n%s", raw)
	}

	// A generator that ends before the restart leaves a restart.json that
	// says so.
	if out, code := runLoadNode(t, env.With("FAKE_GENERATOR_SECONDS", "0").With("FAKE_COMPOSE_STOP_SECONDS", "0"), "run", "--run", env.Run,
		"--scenario", "restart", "--load", "1", "--warmup", "1s", "--minutes", "1", "--restart-at", "40s", "--restart-grace", "10s"); code != 0 {
		t.Fatalf("early end: exit %d, output %q", code, out)
	}
	var early struct {
		Status string `json:"status"`
	}
	readJSON(t, filepath.Join(env.Run, "restart.json"), &early)
	if early.Status != "not run: the generator ended first" {
		t.Fatalf("restart.json after an early end: %+v", early)
	}

	for _, bad := range [][]string{{}, {"--restart-at", "90"}, {"--restart-at", "2m"}, {"--restart-at", "10s"},
		{"--restart-at", "30s", "--restart-grace", "45s"}, {"--restart-at", "5s", "--restart-grace", "1m30s"}} {
		args := append([]string{"run", "--run", env.Run, "--scenario", "restart", "--load", "1", "--warmup", "1s", "--minutes", "1"}, bad...)
		if out, code := runLoadNode(t, env, args...); code == 0 || !strings.Contains(out, "--restart-") {
			t.Fatalf("%v: exit %d, output %q", bad, code, out)
		}
	}
}

// Tasks 17 and 18: during a measurement the runner samples the session
// backlog the retention sweep owes into backlog.jsonl, and the APNs sink's
// new records (?after=last) into apns.jsonl with one line per sample in
// apns-samples.jsonl, and summarises the pushes into apns.json. The sink's
// story here, one fixture per read:
//   - read 1: the ring holds 1..3;
//   - read 2: it wrapped (a ring of 3 now holds 8..10, total 10): 4 lost;
//   - read 3: nothing new (the epoch stays);
//   - read 4: a new recorder whose total (2) fell below last (10): a new
//     epoch, re-read from 0 at once (read 5);
//   - read 6: the app started again (its StartedAt changed) and the new
//     recorder already passed last (total 5 >= 2): a new epoch, re-read
//     from 0 (read 7).
//
// Every record is written once per epoch, the samples count the loss and
// the in-flight peak, and the summary judges duplicates by received_at.
func TestRunSamplesTheBacklogAndTheAPNsSink(t *testing.T) {
	type rec struct {
		seq             int
		token, kind, at string
	}
	ring := func(total, peak int, records ...rec) string {
		var parts []string
		for _, r := range records {
			parts = append(parts, fmt.Sprintf(`{"sequence":%d,"token":%q,"received_at":%q,"headers":{"apns-push-type":%q,"apns-topic":"t","apns-id":"%d"},"acknowledged":true}`,
				r.seq, r.token, r.at, r.kind, r.seq))
		}
		return fmt.Sprintf(`{"requests":[%s],"total":%d,"in_flight_peak":%d}`, strings.Join(parts, ","), total, peak)
	}
	bg := func(seq int, token, at string) rec { return rec{seq, token, "background", "2026-10-24T" + at + "Z"} }
	la := func(seq int, token, at string) rec { return rec{seq, token, "liveactivity", "2026-10-24T" + at + "Z"} }
	first := ring(3, 2, bg(1, "aa", "10:00:00.25"), bg(2, "aa", "10:05:00.5"), bg(3, "cc", "10:05:01"))
	wrapped := ring(10, 4, bg(8, "aa", "10:25:30"), la(9, "bb", "10:30:00"), la(10, "bb", "10:31:00"))
	small := ring(2, 1, bg(1, "dd", "10:40:00"), bg(2, "ee", "10:40:01"))
	restarted := ring(5, 9, bg(1, "ff", "10:50:00"), bg(2, "gg", "10:50:01"), bg(3, "hh", "10:50:02"), bg(4, "ii", "10:50:03"),
		bg(5, "jj", "10:50:04"))
	dir := t.TempDir()
	for name, body := range map[string]string{"1": first, "2": wrapped, "3": wrapped, "4": small, "5": small, "6": restarted, "7": restarted,
		"last": restarted} {
		writeFile(t, filepath.Join(dir, name+".json"), body)
	}
	env := runEnv(t).With("BT_LOAD_BACKLOG_INTERVAL", "1").With("BT_LOAD_APNS_INTERVAL", "1").With("FAKE_APNS_DIR", dir).
		With("FAKE_GENERATOR_APNS_READS", "8").
		With("FAKE_STARTED_AT_1", "A").With("FAKE_STARTED_AT_2", "A").With("FAKE_STARTED_AT_3", "A").With("FAKE_STARTED_AT_4", "A").
		With("FAKE_STARTED_AT", "B")
	out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "1", "--warmup", "1s", "--minutes", "1")
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	backlog, err := os.ReadFile(filepath.Join(env.Run, "backlog.jsonl"))
	if err != nil || !strings.Contains(string(backlog), `"due":42`) {
		t.Fatalf("backlog.jsonl = %q, %v", backlog, err)
	}
	raw, _ := os.ReadFile(env.Get("FAKE_LOG"))
	log := string(raw)
	if !strings.Contains(log, "statement_timeout = '10s'") || !strings.Contains(log, "max_parallel_workers_per_gather = 0") ||
		!strings.Contains(log, "consumed_at < now() - interval '24 hours'") {
		t.Fatalf("the backlog query is not the bounded, serial due count:\n%s", log)
	}
	var afters []string
	for _, line := range strings.Split(log, "\n") {
		if i := strings.Index(line, "/__identitytest/apns?after="); i >= 0 {
			afters = append(afters, line[i+len("/__identitytest/apns?after="):])
		}
	}
	if want := []string{"0", "3", "10", "10", "0", "2", "0", "5"}; len(afters) < len(want) || !slices.Equal(afters[:len(want)], want) {
		t.Fatalf("the sink was read with after=%v, want %v first", afters, want)
	}

	type record struct {
		Epoch    int    `json:"recorder_epoch"`
		Sequence int    `json:"sequence"`
		Received string `json:"received_at"`
	}
	var got []string
	seen := map[string]bool{}
	raw, err = os.ReadFile(filepath.Join(env.Run, "apns.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r record
		if err := json.Unmarshal([]byte(line), &r); err != nil || r.Received == "" {
			t.Fatalf("apns.jsonl line %q: %v", line, err)
		}
		key := fmt.Sprintf("%d/%d", r.Epoch, r.Sequence)
		if seen[key] {
			t.Fatalf("record %s written twice:\n%s", key, raw)
		}
		seen[key] = true
		got = append(got, key)
	}
	if want := []string{"1/1", "1/2", "1/3", "1/8", "1/9", "1/10", "2/1", "2/2", "3/1", "3/2", "3/3", "3/4", "3/5"}; !slices.Equal(got, want) {
		t.Fatalf("records %v, want %v", got, want)
	}

	type sample struct {
		Epoch     int  `json:"recorder_epoch"`
		After     int  `json:"after"`
		Total     int  `json:"total"`
		Returned  int  `json:"returned"`
		Lost      int  `json:"lost"`
		Peak      int  `json:"in_flight_peak"`
		Restarted bool `json:"restarted"`
		PrevLast  int  `json:"prev_last"`
	}
	raw, err = os.ReadFile(filepath.Join(env.Run, "apns-samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var samples []sample
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var s sample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatal(err)
		}
		samples = append(samples, s)
	}
	want := []sample{{Epoch: 1, Total: 3, Returned: 3, Peak: 2}, {Epoch: 1, After: 3, Total: 10, Returned: 3, Lost: 4, Peak: 4},
		{Epoch: 1, After: 10, Total: 10, Peak: 4}, {Epoch: 2, Total: 2, Returned: 2, Peak: 1, Restarted: true, PrevLast: 10},
		{Epoch: 3, Total: 5, Returned: 5, Peak: 9, Restarted: true, PrevLast: 2}}
	if len(samples) < len(want) || !slices.Equal(samples[:len(want)], want) {
		t.Fatalf("samples %+v, want %+v first", samples, want)
	}

	var summary struct {
		Records        int            `json:"records"`
		Lost           int            `json:"lost"`
		Coverage       float64        `json:"coverage"`
		Epochs         int            `json:"epochs"`
		Restarts       int            `json:"restarts"`
		InFlightPeak   int            `json:"in_flight_peak"`
		PeakByEpoch    map[string]int `json:"in_flight_peak_by_epoch"`
		Window         int            `json:"duplicate_window_seconds"`
		RecorderCapped *bool          `json:"recorder_capped"`
		Note           string         `json:"note"`
		ByPushType     map[string]struct {
			Records    int `json:"records"`
			Tokens     int `json:"tokens"`
			Duplicates int `json:"duplicates"`
			Repeats    int `json:"by_design_repeats"`
		} `json:"by_push_type"`
	}
	readJSON(t, filepath.Join(env.Run, "apns.json"), &summary)
	bgs, las := summary.ByPushType["background"], summary.ByPushType["liveactivity"]
	// "aa" was pushed at 10:00:00.25 and 10:05:00.5 (a duplicate) and at
	// 10:25:30 (20.5 minutes later: not); "bb"'s two updates are by design.
	if summary.Records != 13 || summary.Lost != 4 || summary.Epochs != 3 || summary.Restarts != 2 || summary.InFlightPeak != 9 ||
		summary.PeakByEpoch["1"] != 4 || summary.Window != 1190 || summary.RecorderCapped != nil || summary.Coverage < 0.76 || summary.Coverage > 0.77 ||
		bgs.Records != 11 || bgs.Duplicates != 1 || las.Records != 2 || las.Repeats != 1 || las.Duplicates != 0 ||
		!strings.Contains(summary.Note, "4 records were lost") {
		t.Fatalf("apns.json = %+v", summary)
	}
	for _, name := range []string{"apns-samples.jsonl", "apns.jsonl", "apns.json"} {
		if info, err := os.Stat(filepath.Join(env.Run, name)); err != nil || info.Size() == 0 {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The samples are archived with their measurement.
	if out, code := runLoadNode(t, env.With("FAKE_GENERATOR_APNS_READS", "").With("FAKE_GENERATOR_SECONDS", "0"), "run", "--run", env.Run,
		"--scenario", "soak", "--load", "1", "--warmup", "1s", "--minutes", "1"); code != 0 {
		t.Fatalf("second run: exit %d, output %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(env.Run, "runs", "1", "apns-samples.jsonl")); err != nil {
		t.Fatalf("apns-samples.jsonl was not archived: %v", err)
	}
}

// Task 13 Step 2 / W1-D review I5: the seed alone runs with wal_buffers
// 256MB. load-node.sh seed sets it (ALTER SYSTEM, a db restart, waited
// healthy, checked), seeds, and resets it (ALTER SYSTEM RESET, a restart,
// checked against the value before), also when the seed fails; seed.log,
// seed-settings.json and seed-report.json say so. A running app is never
// restarted under.
func TestSeedRunsWithLargerWALBuffersAndResetsThem(t *testing.T) {
	env := runEnv(t).With("FAKE_APP_RUNNING", "false")
	out, code := runLoadNode(t, env, "seed", "--run", env.Run, "--families", "100", "--seed", "1")
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	raw, _ := os.ReadFile(env.Get("FAKE_LOG"))
	log := string(raw)
	set := strings.Index(log, "ALTER SYSTEM SET wal_buffers = '256MB'")
	seed := strings.Index(log, "loadgen seed --secrets")
	reset := strings.Index(log, "ALTER SYSTEM RESET wal_buffers")
	if set < 0 || seed < set || reset < seed || strings.Count(log, " restart -t 120 db\n") != 2 || strings.Count(log, " up -d --no-build --wait --wait-timeout 600 db\n") != 2 {
		t.Fatalf("want set, restart, seed, reset, restart:\n%s", log)
	}
	// Each restart follows a CHECKPOINT (the shutdown checkpoint then has
	// nothing to flush) and gives the stop 120 s, not Docker's 10 s.
	restarts := regexp.MustCompile(`(?s)-c CHECKPOINT\n[^\n]*restart -t 120 db\n`).FindAllStringIndex(log, -1)
	if len(restarts) != 2 || restarts[0][0] < set || restarts[0][0] > seed || restarts[1][0] < reset {
		t.Fatalf("want CHECKPOINT right before each restart -t 120 db:\n%s", log)
	}
	if wal, _ := os.ReadFile(env.Get("FAKE_LOG") + ".wal"); strings.TrimSpace(string(wal)) != "16MB" {
		t.Fatalf("wal_buffers after the seed: %q", wal)
	}
	seedLog, _ := os.ReadFile(filepath.Join(env.Run, "seed.log"))
	if !strings.Contains(string(seedLog), "wal_buffers 256MB for the seed (was 16MB)") || !strings.Contains(string(seedLog), "wal_buffers reset to 16MB") {
		t.Fatalf("seed.log = %q", seedLog)
	}
	var settings struct {
		WAL struct {
			During, Before, After, AppliedAt, ResetAt string
		} `json:"wal_buffers"`
	}
	raw, _ = os.ReadFile(filepath.Join(env.Run, "seed-settings.json"))
	if err := json.Unmarshal(bytesReplace(raw), &settings); err != nil || settings.WAL.During != "256MB" || settings.WAL.Before != "16MB" ||
		settings.WAL.After != "16MB" || settings.WAL.AppliedAt == "" || settings.WAL.ResetAt == "" {
		t.Fatalf("seed-settings.json = %s (%v)", raw, err)
	}
	if report, _ := os.ReadFile(filepath.Join(env.Run, "seed-report.json")); !strings.Contains(string(report), `"DatabaseSettings"`) {
		t.Fatalf("seed-report.json = %s", report)
	}

	// A failed seed still resets.
	failed := runEnv(t).With("FAKE_APP_RUNNING", "false").With("FAKE_SEED_EXIT", "5")
	out, code = runLoadNode(t, failed, "seed", "--run", failed.Run, "--families", "100", "--seed", "1")
	raw, _ = os.ReadFile(failed.Get("FAKE_LOG"))
	if code != 5 || !strings.Contains(string(raw), "ALTER SYSTEM RESET wal_buffers") {
		t.Fatalf("failed seed: exit %d, output %q:\n%s", code, out, raw)
	}
	if wal, _ := os.ReadFile(failed.Get("FAKE_LOG") + ".wal"); strings.TrimSpace(string(wal)) != "16MB" {
		t.Fatalf("wal_buffers after a failed seed: %q", wal)
	}
	// A running app is not restarted under.
	running := runEnv(t).With("FAKE_APP_RUNNING", "true")
	out, code = runLoadNode(t, running, "seed", "--run", running.Run, "--families", "100", "--seed", "1")
	raw, _ = os.ReadFile(running.Get("FAKE_LOG"))
	if code == 0 || !strings.Contains(out, "the app is running") || strings.Contains(string(raw), "ALTER SYSTEM") {
		t.Fatalf("running app: exit %d, output %q", code, out)
	}
}

// bytesReplace maps seed-settings.json's snake_case keys to the test
// struct's field names.
func bytesReplace(raw []byte) []byte {
	return []byte(strings.NewReplacer(`"during_seed"`, `"During"`, `"before"`, `"Before"`, `"after"`, `"After"`, `"applied_at"`, `"AppliedAt"`,
		`"reset_at"`, `"ResetAt"`).Replace(string(raw)))
}

// A run records the db's background writer and autovacuum log settings as
// the db reports them, and BT_LOAD_LOG_AUTOVACUUM_MIN_DURATION as the db's
// command line applied it (the run's own environment need not name it).
func TestRunRecordsTheDBTuningSettings(t *testing.T) {
	env := runEnv(t).With("FAKE_GENERATOR_SECONDS", "0").With("FAKE_DB_AUTOVACUUM_LOG", "0").
		With("FAKE_DB_CMD", `postgres\n-c\nbgwriter_delay=50ms\n-c\nlog_autovacuum_min_duration=0\n-c\nlog_min_duration_statement=500ms`)
	if out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "1", "--warmup", "1s", "--minutes", "1"); code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	var measurement struct {
		DB       map[string]string `json:"db_settings"`
		Tunables map[string]string `json:"tunables"`
	}
	readJSON(t, filepath.Join(env.Run, "measurement.json"), &measurement)
	for name, want := range map[string]string{"wal_buffers": "16MB", "log_autovacuum_min_duration": "0", "bgwriter_lru_maxpages": "1000", "bgwriter_delay": "50ms"} {
		if got := measurement.DB[name]; got != want {
			t.Errorf("db_settings %s = %q, want %q (%v)", name, got, want, measurement.DB)
		}
	}
	if got := measurement.Tunables["BT_LOAD_LOG_AUTOVACUUM_MIN_DURATION"]; got != "0" {
		t.Errorf("tunables BT_LOAD_LOG_AUTOVACUUM_MIN_DURATION = %q (%v)", got, measurement.Tunables)
	}
}

// W1-D re-review N3: a db whose wal_buffers a seed left set by ALTER SYSTEM
// (killed between its set and its reset, on a kept volume) is refused by
// start and run; a run records the db's wal_buffers in measurement.json.
func TestStartAndRunRefuseALeftoverSeedSetting(t *testing.T) {
	env := runEnv(t).With("FAKE_AUTO_CONF_WAL", "1")
	for _, args := range [][]string{{"start", "--run", env.Run},
		{"run", "--run", env.Run, "--scenario", "soak", "--load", "1", "--warmup", "1s", "--minutes", "1"}} {
		out, code := runLoadNode(t, env, args...)
		if code == 0 || !strings.Contains(out, "wal_buffers is still set by ALTER SYSTEM") {
			t.Fatalf("%s: exit %d, output %q", args[0], code, out)
		}
	}
	if raw, _ := os.ReadFile(env.Get("FAKE_LOG")); strings.Contains(string(raw), "loadgen run") || strings.Contains(string(raw), " up -d --no-build --wait --wait-timeout 300 app") {
		t.Fatalf("a refused stack was started or measured:\n%s", raw)
	}
	clean := runEnv(t).With("FAKE_GENERATOR_SECONDS", "0")
	if out, code := runLoadNode(t, clean, "run", "--run", clean.Run, "--scenario", "soak", "--load", "1", "--warmup", "1s", "--minutes", "1"); code != 0 {
		t.Fatalf("clean run: exit %d, output %q", code, out)
	}
	var measurement struct {
		DB struct {
			WALBuffers string `json:"wal_buffers"`
		} `json:"db_settings"`
	}
	readJSON(t, filepath.Join(clean.Run, "measurement.json"), &measurement)
	if measurement.DB.WALBuffers != "16MB" {
		t.Fatalf("measurement.json db_settings = %+v", measurement.DB)
	}
}

// W1-D re-review N4(a): a restart whose re-read from 0 fails is carried to
// the next sample written, so the summary still counts the restart.
func TestAPNsRestartWithAFailedReReadIsStillRecorded(t *testing.T) {
	dir := t.TempDir()
	first := `{"requests":[{"sequence":1,"token":"aa","received_at":"2026-10-24T10:00:00Z","headers":{"apns-push-type":"background"}},` +
		`{"sequence":2,"token":"bb","received_at":"2026-10-24T10:00:01Z","headers":{"apns-push-type":"background"}},` +
		`{"sequence":3,"token":"cc","received_at":"2026-10-24T10:00:02Z","headers":{"apns-push-type":"background"}}],"total":3,"in_flight_peak":1}`
	restarted := `{"requests":[{"sequence":1,"token":"dd","received_at":"2026-10-24T10:10:00Z","headers":{"apns-push-type":"background"}},` +
		`{"sequence":2,"token":"ee","received_at":"2026-10-24T10:10:01Z","headers":{"apns-push-type":"background"}}],"total":2,"in_flight_peak":1}`
	writeFile(t, filepath.Join(dir, "1.json"), first)
	writeFile(t, filepath.Join(dir, "2.json"), restarted)
	writeFile(t, filepath.Join(dir, "3.fail"), "")
	writeFile(t, filepath.Join(dir, "last.json"), restarted)
	env := runEnv(t).With("BT_LOAD_APNS_INTERVAL", "1").With("FAKE_APNS_DIR", dir).With("FAKE_GENERATOR_APNS_READS", "5")
	if out, code := runLoadNode(t, env, "run", "--run", env.Run, "--scenario", "soak", "--load", "1", "--warmup", "1s", "--minutes", "1"); code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	raw, err := os.ReadFile(filepath.Join(env.Run, "apns-samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 || !strings.Contains(lines[1], `"restarted":true`) || !strings.Contains(lines[1], `"prev_last":3`) ||
		!strings.Contains(lines[1], `"recorder_epoch":2`) {
		t.Fatalf("samples:\n%s", raw)
	}
	var summary struct {
		Restarts int  `json:"restarts"`
		Unknown  bool `json:"lost_unknown_at_restart"`
		Records  int  `json:"records"`
	}
	readJSON(t, filepath.Join(env.Run, "apns.json"), &summary)
	if summary.Restarts != 1 || !summary.Unknown || summary.Records != 5 {
		t.Fatalf("apns.json = %+v", summary)
	}
}

// upgradeEnv is an up, seeded stack between measurements: the app running
// and starting cleanly, no generator container.
func upgradeEnv(t *testing.T) loadEnv {
	t.Helper()
	good := `{"time":"t","level":"INFO","msg":"pprof listening","address":"[::]:6060"}` + "\n" +
		`{"time":"t","level":"INFO","msg":"runtime limits","gomaxprocs":4,"cgroup_memory_max":8589934592}` + "\n"
	env := fakeHost(t, idleHost()).With("FAKE_APP_RUNNING", "true").With("FAKE_LOADGEN_ID", "").With("FAKE_APP_LOG", good).
		With("BT_LOAD_START_LOG_WAIT", "1")
	writeFile(t, filepath.Join(env.Run, "caps.json"), `{"ok":true,"mismatches":[],"app":{"running":true}}`)
	writeFile(t, filepath.Join(env.Run, "source.json"), `{"source_sha":"`+strings.Repeat("a", 40)+`","server_tree_dirty":false}`)
	return env
}

type upgradeRecord struct {
	Status     string `json:"status"`
	Stage      string `json:"stage"`
	Message    string `json:"message"`
	RecreateDB bool   `json:"recreate_db"`
	From, To   struct {
		SourceSHA     string `json:"source_sha"`
		ServerImage   string `json:"server_image"`
		LoadgenImage  string `json:"loadgen_image"`
		SchemaVersion *int   `json:"schema_version"`
	}
	Migration *struct {
		Before     *int `json:"schema_before"`
		After      *int `json:"schema_after"`
		DurationMS *int `json:"duration_ms"`
		OK         bool `json:"ok"`
	} `json:"migration"`
	DBSettings map[string]string `json:"db_settings"`
}

func upgradeHistory(t *testing.T, env loadEnv) []upgradeRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(env.Run, "upgrades.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var records []upgradeRecord
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r upgradeRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("upgrades.jsonl line %q: %v", line, err)
		}
		records = append(records, r)
	}
	return records
}

// inOrder fails unless every want appears in log, each after the one before.
func inOrder(t *testing.T, log string, wants ...string) {
	t.Helper()
	at := 0
	for _, want := range wants {
		i := strings.Index(log[at:], want)
		if i < 0 {
			t.Fatalf("%q missing or out of order (after %q):\n%s", want, log[:at], log)
		}
		at += i + len(want)
	}
}

// Task 21's A/B on one seeded volume: upgrade builds HEAD, stops the app,
// migrates the existing database (timed, schema before and after), checks
// the marker and starts the app as start does, recording the old and new
// SHAs and images; the db container is kept, and the output says what that
// means for the overlay's db settings.
func TestUpgradeMovesTheSeededStackToHEAD(t *testing.T) {
	head, err := exec.Command("/usr/bin/git", "-C", "..", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(head))
	env := upgradeEnv(t)
	out, code := runLoadNode(t, env, "upgrade", "--run", env.Run)
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	for _, want := range []string{"upgrade: " + strings.Repeat("a", 40) + " -> " + sha + "; schema 36 -> 37", "start: app ready",
		"the db container is kept: a changed db command line in the overlay", "use --recreate-db"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	raw, _ := os.ReadFile(env.Get("FAKE_LOG"))
	log := string(raw)
	inOrder(t, log, "shobj_description", "schema_migrations", "build app migrate loadgen", "stop -t 30 app",
		"pg_file_settings", "run --rm --no-deps -T migrate", "schema_migrations", "shobj_description",
		"up -d --no-build --wait --wait-timeout 300 app", "/readyz")
	if strings.Contains(log, "--force-recreate") || strings.Contains(log, "CHECKPOINT") {
		t.Errorf("the db was recreated without --recreate-db:\n%s", log)
	}
	var record upgradeRecord
	readJSON(t, filepath.Join(env.Run, "upgrade.json"), &record)
	if record.Status != "ok" || record.RecreateDB || record.From.SourceSHA != strings.Repeat("a", 40) || record.To.SourceSHA != sha ||
		record.From.ServerImage != "sha256:b0-bt-load-server" || record.To.ServerImage != "sha256:b1-bt-load-server" ||
		record.From.LoadgenImage != "sha256:b0-bt-load-loadgen" || record.To.LoadgenImage != "sha256:b1-bt-load-loadgen" ||
		record.From.SchemaVersion == nil || *record.From.SchemaVersion != 36 || record.Migration == nil || !record.Migration.OK ||
		*record.Migration.Before != 36 || *record.Migration.After != 37 || record.Migration.DurationMS == nil || *record.Migration.DurationMS < 0 ||
		record.DBSettings["track_wal_io_timing"] != "on" || record.DBSettings["log_autovacuum_min_duration"] != "1s" ||
		record.DBSettings["bgwriter_lru_maxpages"] != "1000" || record.DBSettings["bgwriter_delay"] != "50ms" {
		t.Errorf("upgrade.json = %+v", record)
	}
	if got, _ := os.ReadFile(filepath.Join(env.Run, "source-sha")); strings.TrimSpace(string(got)) != sha {
		t.Errorf("source-sha = %q, want HEAD %s", got, sha)
	}
	var source struct {
		SHA          string   `json:"source_sha"`
		UpgradedFrom []string `json:"upgraded_from"`
	}
	readJSON(t, filepath.Join(env.Run, "source.json"), &source)
	if source.SHA != sha || !slices.Equal(source.UpgradedFrom, []string{strings.Repeat("a", 40)}) {
		t.Errorf("source.json = %+v", source)
	}
	// A second upgrade starts from the first's result; the history keeps both.
	if out, code := runLoadNode(t, env, "upgrade", "--run", env.Run); code != 0 {
		t.Fatalf("second upgrade: exit %d, output %q", code, out)
	}
	history := upgradeHistory(t, env)
	if len(history) != 2 || history[1].From.SourceSHA != sha || history[1].From.SchemaVersion == nil || *history[1].From.SchemaVersion != 37 ||
		history[1].From.ServerImage != "sha256:b1-bt-load-server" || history[1].To.ServerImage != "sha256:b2-bt-load-server" {
		t.Errorf("upgrades.jsonl = %+v", history)
	}
}

// upgrade never runs under a measurement or a seed: a generator container
// or the run's sampling sentinel refuses it before anything is built or
// stopped.
func TestUpgradeRefusesWhileGenerating(t *testing.T) {
	for _, name := range []string{"generator", "sampling"} {
		t.Run(name, func(t *testing.T) {
			env := upgradeEnv(t)
			want := "a measurement or seed is in progress"
			if name == "generator" {
				env, want = env.With("FAKE_LOADGEN_ID", "fake-loadgen-run"), "a generator is running (fake-loadgen-run"
			} else {
				writeFile(t, filepath.Join(env.Run, ".sampling"), "")
			}
			out, code := runLoadNode(t, env, "upgrade", "--run", env.Run)
			if code == 0 || !strings.Contains(out, want) {
				t.Fatalf("exit %d, output %q; want %q", code, out, want)
			}
			raw, _ := os.ReadFile(env.Get("FAKE_LOG"))
			for _, never := range []string{" build ", "stop -t 30 app", "migrate"} {
				if strings.Contains(string(raw), never) {
					t.Errorf("a refused upgrade ran %q:\n%s", never, raw)
				}
			}
			if _, err := os.Stat(filepath.Join(env.Run, "upgrades.jsonl")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused upgrade wrote history: %v", err)
			}
		})
	}
}

// --recreate-db: after the app stops and before the migration, a
// CHECKPOINT, then the db recreated (120 s stop, healthy wait), its caps and
// marker checked again.
func TestUpgradeRecreatesTheDBBeforeTheMigration(t *testing.T) {
	env := upgradeEnv(t)
	out, code := runLoadNode(t, env, "upgrade", "--run", env.Run, "--recreate-db")
	if code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if strings.Contains(out, "the db container is kept") || !strings.Contains(out, "db recreated") {
		t.Errorf("output %q", out)
	}
	raw, _ := os.ReadFile(env.Get("FAKE_LOG"))
	inOrder(t, string(raw), "build app migrate loadgen", "stop -t 30 app", "-c CHECKPOINT",
		"up -d --no-build --force-recreate --timeout 120 --wait --wait-timeout 600 db", "NanoCpus", "shobj_description",
		"pg_file_settings", "run --rm --no-deps -T migrate", "up -d --no-build --wait --wait-timeout 300 app")
	var record upgradeRecord
	readJSON(t, filepath.Join(env.Run, "upgrade.json"), &record)
	if record.Status != "ok" || !record.RecreateDB {
		t.Errorf("upgrade.json = %+v", record)
	}
	// A recreated db that is not capped stops the upgrade before the migration.
	out, code = runLoadNode(t, env.With("FAKE_INSPECT_DB", "0 0 0"), "upgrade", "--run", env.Run, "--recreate-db")
	if code == 0 || !strings.Contains(out, "the recreated db is not capped as declared") {
		t.Fatalf("uncapped db: exit %d, output %q", code, out)
	}
}

// A failed migration leaves the app stopped and says so: upgrade.json and
// the history record the failure, the schema versions and the migrate log;
// nothing starts the app. A failed build stops nothing.
func TestUpgradeMigrationFailureLeavesTheAppStopped(t *testing.T) {
	env := upgradeEnv(t)
	out, code := runLoadNode(t, env.With("FAKE_MIGRATE_EXIT", "3"), "upgrade", "--run", env.Run)
	if code == 0 || !strings.Contains(out, "the migration failed after") ||
		!strings.Contains(out, "the app is stopped (schema version 36 before, 36 now; see upgrade-migrate.log)") ||
		!strings.Contains(out, "migrate: fake migrate: migration_failed 000037") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	raw, _ := os.ReadFile(env.Get("FAKE_LOG"))
	log := string(raw)
	inOrder(t, log, "stop -t 30 app", "run --rm --no-deps -T migrate")
	if strings.Contains(log, "wait-timeout 300 app") {
		t.Errorf("the app was started after a failed migration:\n%s", log)
	}
	var record upgradeRecord
	readJSON(t, filepath.Join(env.Run, "upgrade.json"), &record)
	if record.Status != "failed" || record.Stage != "migrate" || record.Migration == nil || record.Migration.OK ||
		record.Migration.Before == nil || *record.Migration.Before != 36 || record.Migration.After == nil || *record.Migration.After != 36 {
		t.Errorf("upgrade.json = %+v", record)
	}
	if history := upgradeHistory(t, env); len(history) != 1 || history[0].Status != "failed" {
		t.Errorf("upgrades.jsonl = %+v", history)
	}
	if migrateLog, _ := os.ReadFile(filepath.Join(env.Run, "upgrade-migrate.log")); !strings.Contains(string(migrateLog), "migration_failed") {
		t.Errorf("upgrade-migrate.log = %q", migrateLog)
	}

	// A failed build: the app is never stopped.
	env = upgradeEnv(t)
	out, code = runLoadNode(t, env.With("FAKE_BUILD_EXIT", "1"), "upgrade", "--run", env.Run)
	if code == 0 || !strings.Contains(out, "the build failed; the app was not stopped") {
		t.Fatalf("failed build: exit %d, output %q", code, out)
	}
	raw, _ = os.ReadFile(env.Get("FAKE_LOG"))
	if strings.Contains(string(raw), "stop -t 30 app") {
		t.Errorf("a failed build stopped the app:\n%s", raw)
	}
	if got, _ := os.ReadFile(filepath.Join(env.Run, "source-sha")); strings.TrimSpace(string(got)) != strings.Repeat("a", 40) {
		t.Errorf("a failed build moved source-sha to %q", got)
	}
	if history := upgradeHistory(t, env); len(history) != 1 || history[0].Status != "failed" || history[0].Stage != "build" {
		t.Errorf("upgrades.jsonl after a failed build = %+v", history)
	}
}
