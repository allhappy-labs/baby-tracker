//go:build identitytest

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"babytracker/server/internal/loadseed"
	"babytracker/server/internal/loadtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runDuration reads a duration as the runner writes them (Ns, Nm, Nh, with
// leading zeros allowed) or any Go duration.
func runDuration(name string, target *time.Duration) func(string) error {
	return func(value string) error {
		d, err := parseRunDuration(value)
		if err != nil {
			return fmt.Errorf("--%s: %w", name, err)
		}
		*target = d
		return nil
	}
}

var runnerDuration = regexp.MustCompile(`^([0-9]{1,6})(s|m|h)$`)

func parseRunDuration(value string) (time.Duration, error) {
	if m := runnerDuration.FindStringSubmatch(value); m != nil {
		n, _ := strconv.Atoi(m[1]) // base 10: "08" is 8
		unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour}[m[2]]
		return time.Duration(n) * unit, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q", value)
	}
	return d, nil
}

// parseRunFlags reads the run command's options: RunConfig's fields, as
// Server/scripts/load-node.sh passes them.
func parseRunFlags(args []string) (loadtest.RunConfig, error) {
	cfg, _, err := parseRunFlagsWithSuperuser(args)
	return cfg, err
}

// parseRunFlagsWithSuperuser is parseRunFlags, also returning
// --superuser-url-file (the hot maintenance's database; runCommand opens it).
func parseRunFlagsWithSuperuser(args []string) (loadtest.RunConfig, string, error) {
	var cfg loadtest.RunConfig
	var steps, thresholds, superuserURLFile string
	cfg.WithPump = true
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.StringVar(&cfg.Base, "base", "", "the app's base URL (http://172.31.250.10:8080)")
	flags.StringVar(&cfg.Host, "host", "", "the Host header every request names (sync.load.test)")
	flags.StringVar(&cfg.Fleet, "fleet", "", "the private fleet file (read, then checkpointed)")
	flags.StringVar(&cfg.Ledger, "ledger", "", "the write ledger to create (private)")
	flags.StringVar(&cfg.Tracked, "tracked", "", "the tracked devices file to create (private)")
	flags.StringVar(&cfg.Out, "out", "", "result.json")
	flags.StringVar(&cfg.WindowStart, "window-start", "", "the file written when warm-up ends (default: window-start beside --out)")
	flags.StringVar(&cfg.Scenario, "scenario", "soak", "soak|ramp|storm|restart|mass-snapshot|hot-family|flood|import|export")
	flags.Float64Var(&cfg.Load, "load", 0, "the background load (x the 10k model, sized to the fleet)")
	flags.StringVar(&steps, "steps", "", "ramp loads, comma-separated (0.5,1,1.5,2)")
	flags.Func("warmup", "warm-up (Ns, Nm or a Go duration)", runDuration("warmup", &cfg.Warmup))
	flags.Func("measure", "the measurement window", runDuration("measure", &cfg.Measure))
	flags.Func("step-duration", "each ramp step (default 10m)", runDuration("step-duration", &cfg.StepDuration))
	cfg.Seed, cfg.TrackedDevices = 1, 200
	// Base 10, always: the runner may pass "09" (flag.Uint64Var would read octal).
	flags.Func("seed", "the seed every schedule and draw follows (default 1)", func(v string) (err error) {
		cfg.Seed, err = strconv.ParseUint(v, 10, 64)
		return err
	})
	flags.Func("tracked-devices", "devices whose view of their family is audited (default 200)", func(v string) error {
		n, err := strconv.ParseUint(v, 10, 31)
		cfg.TrackedDevices = int(n)
		return err
	})
	flags.Func("storm-at", "storm: when every live socket closes, from window-start", runDuration("storm-at", &cfg.StormAt))
	flags.Func("restart-at", "restart: the declared window's start, from window-start", runDuration("restart-at", &cfg.RestartAt))
	flags.Func("restart-grace", "restart: the declared window's length (default 60s)", runDuration("restart-grace", &cfg.RestartGrace))
	flags.BoolVar(&cfg.WithPump, "with-pump", true, "write pumping sessions (kind 10); the server does not advertise kinds")
	flags.StringVar(&thresholds, "thresholds", "", "thresholds.json (default: the spec's)")
	flags.StringVar(&cfg.ModelOverride, "model-override", "", "live-only: every class but the live sockets at 0 (soak, ramp, storm, restart)")
	flags.StringVar(&cfg.Maintenance, "maintenance", "", "hot: one family purge and one export inside the soak (needs --superuser-url-file)")
	flags.Func("maintenance-at", "hot maintenance: its start, from window-start (default a third of --measure)", runDuration("maintenance-at", &cfg.MaintenanceAt))
	flags.StringVar(&cfg.Flood, "flood", "", "flood: ip, bearer or both (default both), to measure each attack's cost alone")
	flags.StringVar(&superuserURLFile, "superuser-url-file", "", "the load stack's superuser database URL (--maintenance hot)")
	flags.StringVar(&cfg.ProfileDir, "profile-dir", "", "write the generator's own profiles and lag traces here (relative: beside --out; the runner: --profile-dir profiles)")
	if err := flags.Parse(args); err != nil {
		return cfg, "", errUsage
	}
	bad := func(format string, a ...any) (loadtest.RunConfig, string, error) {
		fmt.Fprintf(os.Stderr, format+"\n", a...)
		return cfg, "", errUsage
	}
	if flags.NArg() != 0 {
		return bad("run: unexpected argument %q", flags.Arg(0))
	}
	if cfg.Base == "" || cfg.Host == "" || cfg.Fleet == "" || cfg.Ledger == "" || cfg.Tracked == "" || cfg.Out == "" {
		return bad("run needs --base, --host, --fleet, --ledger, --tracked and --out")
	}
	if !(cfg.Load > 0) || math.IsInf(cfg.Load, 0) || cfg.Measure <= 0 {
		return bad("run needs a positive --load and --measure")
	}
	if steps != "" {
		for _, s := range strings.Split(steps, ",") {
			load, err := strconv.ParseFloat(s, 64)
			if err != nil || !(load > 0) || math.IsInf(load, 0) {
				return bad("--steps: %q is not a positive load", s)
			}
			cfg.Steps = append(cfg.Steps, load)
		}
	}
	switch cfg.Scenario {
	case "ramp":
		if len(cfg.Steps) == 0 {
			return bad("--scenario ramp needs --steps")
		}
		for i := 1; i < len(cfg.Steps); i++ {
			if cfg.Steps[i] < cfg.Steps[i-1] {
				return bad("--steps must not fall: step %d (%g) is below step %d (%g)", i+1, cfg.Steps[i], i, cfg.Steps[i-1])
			}
		}
	case "storm":
		if cfg.StormAt <= 0 || cfg.StormAt >= cfg.Measure {
			return bad("--scenario storm needs --storm-at inside --measure")
		}
	case "restart":
		if cfg.RestartAt <= 0 || cfg.RestartAt >= cfg.Measure {
			return bad("--scenario restart needs --restart-at inside --measure")
		}
	case "soak", "mass-snapshot", "hot-family", "flood", "import", "export":
	default:
		return bad("--scenario %q is unknown", cfg.Scenario)
	}
	switch cfg.ModelOverride {
	case "":
	case loadtest.ModelLiveOnly:
		if cfg.Scenario != "soak" && cfg.Scenario != "ramp" && cfg.Scenario != "storm" && cfg.Scenario != "restart" {
			return bad("--model-override %s takes only --scenario soak, ramp, storm or restart", loadtest.ModelLiveOnly)
		}
	default:
		return bad("--model-override %q is unknown (want %s)", cfg.ModelOverride, loadtest.ModelLiveOnly)
	}
	switch cfg.Maintenance {
	case "":
		if superuserURLFile != "" || cfg.MaintenanceAt != 0 {
			return bad("--superuser-url-file and --maintenance-at belong to --maintenance %s", loadtest.MaintenanceHot)
		}
	case loadtest.MaintenanceHot:
		if cfg.Scenario != "soak" || cfg.ModelOverride != "" {
			return bad("--maintenance %s runs only in --scenario soak with the full model", loadtest.MaintenanceHot)
		}
		if superuserURLFile == "" {
			return bad("--maintenance %s needs --superuser-url-file", loadtest.MaintenanceHot)
		}
		if cfg.MaintenanceAt >= cfg.Measure {
			return bad("--maintenance-at must fall inside --measure")
		}
	default:
		return bad("--maintenance %q is unknown (want %s)", cfg.Maintenance, loadtest.MaintenanceHot)
	}
	if thresholds != "" {
		t, err := loadtest.LoadThresholds(thresholds)
		if err != nil {
			return bad("%v", err)
		}
		cfg.Thresholds = t
	}
	return cfg, superuserURLFile, nil
}

// openMaintenanceDatabase opens the stack's superuser pool for the hot
// maintenance, refusing any database that is not a disposable load stack.
func openMaintenanceDatabase(ctx context.Context, path string) (*pgxpool.Pool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("the superuser database URL file could not be read")
	}
	cfg, err := pgxpool.ParseConfig(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, errors.New("the superuser database URL is invalid")
	}
	cfg.MaxConns, cfg.MinConns = 2, 0
	cfg.ConnConfig.RuntimeParams["application_name"] = "baby-tracker-load maintenance"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("the superuser database could not be opened")
	}
	if _, err := loadseed.CheckStack(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("refusing the hot maintenance: %w", err)
	}
	return pool, nil
}

// runCommand drives one load run. It fails only when the generator itself
// failed: a run that completed exits 0 whatever its verdict, which is in
// result.json.
func runCommand(ctx context.Context, args []string, logger *slog.Logger) error {
	cfg, superuserURLFile, err := parseRunFlagsWithSuperuser(args)
	if err != nil {
		return err
	}
	if superuserURLFile != "" {
		pool, err := openMaintenanceDatabase(ctx, superuserURLFile)
		if err != nil {
			return err
		}
		defer pool.Close()
		cfg.Superuser = pool
	}
	logger.Info("baby-tracker-load run", "scenario", cfg.Scenario, "load", cfg.Load, "steps", cfg.Steps, "warmup", cfg.Warmup,
		"measure", cfg.Measure, "seed", cfg.Seed)
	summary, err := loadtest.Run(ctx, cfg)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errors.New("the run was interrupted")
		}
		return err
	}
	thresholds := cfg.Thresholds
	if thresholds == (loadtest.Thresholds{}) {
		thresholds = loadtest.SpecThresholds()
	}
	v := loadtest.EvaluateWindow(summary, thresholds)
	logger.Info("baby-tracker-load run done", "generator_gates_pass", v.Pass, "reasons", strings.Join(v.Reasons, "; "),
		"dropped", summary.Dropped, "unexpected_live_losses", summary.UnexpectedLiveLosses)
	return nil
}
