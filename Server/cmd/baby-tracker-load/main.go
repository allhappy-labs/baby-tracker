//go:build identitytest

// baby-tracker-load drives the Phase 5 load runs against a disposable load
// stack. `seed` provisions families into the stack's database and writes
// the private fleet file; `run` and `audit` (Tasks 10 and 11) use it.
//
// It exists only in the identitytest build: it signs in through the test
// identity issuer, which a release server never trusts, and it refuses any
// stack that does not name itself disposable (a .test origin and a
// com.example Apple audience).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/loadseed"
	"babytracker/server/internal/loadtest"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var err error
	switch os.Args[1] {
	case "seed":
		err = seedCommand(ctx, os.Args[2:], os.Getenv, logger)
	case "audit":
		err = auditCommand(ctx, os.Args[2:], logger)
	case "run":
		err = runCommand(ctx, os.Args[2:], logger)
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	default:
		usage(os.Stderr)
		os.Exit(2)
	}
	if errors.Is(err, errUsage) {
		os.Exit(2)
	}
	if err != nil {
		logger.Error("baby-tracker-load failed", "command", os.Args[1], "error", err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: baby-tracker-load seed --secrets DIR --families N --seed S [--as-of RFC3339] [--history r10k|small]")
	fmt.Fprintln(w, "         [--backlog-hours 24] [--backlog-load 2] [--workers 16] --fleet PATH --report PATH")
	fmt.Fprintln(w, "       baby-tracker-load audit --superuser-url-file FILE --ledger PATH --tracked PATH --out PATH --result PATH")
	fmt.Fprintln(w, "         --host PATH [--verdict PATH] [--thresholds PATH]")
	fmt.Fprintln(w, "       baby-tracker-load run --base URL --host HOST --fleet PATH --ledger PATH --tracked PATH --out PATH")
	fmt.Fprintln(w, "         --scenario S --load L --warmup D --measure D [--seed S] [--thresholds PATH] [--steps L,L,…] [--step-duration D]")
	fmt.Fprintln(w, "         [--storm-at D] [--restart-at D] [--restart-grace D] [--tracked-devices N] [--with-pump=false] [--window-start PATH]")
	fmt.Fprintln(w, "         [--model-override live-only] [--maintenance hot --superuser-url-file FILE [--maintenance-at D]] [--profile-dir DIR]")
}

var errUsage = errors.New("usage")

// maxAsOfAge is how far before now --as-of may be.
const maxAsOfAge = 24 * time.Hour

// seedFlags are the seed command's options.
type seedFlags struct {
	secrets, history, fleet, report, asOf string
	families, workers, backlogHours       int
	seed                                  uint64
	backlogLoad                           float64
	hotFamily                             bool
	memory                                loadseed.SessionMemory
}

func parseSeedFlags(args []string, now time.Time) (seedFlags, loadseed.Spec, error) {
	var f seedFlags
	flags := flag.NewFlagSet("seed", flag.ContinueOnError)
	flags.StringVar(&f.secrets, "secrets", "", "the load stack's private secrets directory (0700)")
	flags.IntVar(&f.families, "families", 0, "families to seed")
	flags.Uint64Var(&f.seed, "seed", 1, "the seed every shape, timestamp and payload follows")
	flags.StringVar(&f.asOf, "as-of", "", "the seed's reference time, RFC 3339 (default: now, to the second)")
	flags.StringVar(&f.history, "history", "r10k", "history profile: r10k or small (7 days)")
	flags.IntVar(&f.backlogHours, "backlog-hours", 24, "hours of consumed sessions before as-of")
	flags.Float64Var(&f.backlogLoad, "backlog-load", 2, "the load factor the consumed-session backlog is sized for")
	flags.IntVar(&f.workers, "workers", 16, "families provisioned or encoded at once")
	flags.StringVar(&f.fleet, "fleet", "", "the private fleet file to write (in a 0700 directory)")
	flags.StringVar(&f.report, "report", "", "the seed report to write (JSON, no credentials)")
	flags.BoolVar(&f.hotFamily, "hot-family", false, "also seed the hot family (5 accounts x 2 devices) the hot-family scenario drives")
	flags.StringVar(&f.memory.WorkMem, "work-mem", "", "work_mem of the superuser seeder sessions (default 256MB)")
	flags.StringVar(&f.memory.MaintenanceWorkMem, "maintenance-work-mem", "", "maintenance_work_mem of the superuser seeder sessions (default 1GB)")
	if err := flags.Parse(args); err != nil {
		return f, loadseed.Spec{}, errUsage
	}
	if !f.memory.Valid() {
		fmt.Fprintln(os.Stderr, "--work-mem and --maintenance-work-mem take a PostgreSQL memory value (e.g. 64MB, 1GB)")
		return f, loadseed.Spec{}, errUsage
	}
	if flags.NArg() != 0 || f.secrets == "" || f.fleet == "" || f.report == "" || f.families < 1 {
		fmt.Fprintln(os.Stderr, "seed needs --secrets, --families (at least 1), --fleet and --report")
		return f, loadseed.Spec{}, errUsage
	}
	asOf := now.UTC().Truncate(time.Second)
	if f.asOf != "" {
		parsed, err := time.Parse(time.RFC3339, f.asOf)
		if err != nil {
			fmt.Fprintln(os.Stderr, "--as-of must be RFC 3339")
			return f, loadseed.Spec{}, errUsage
		}
		asOf = parsed.UTC()
	}
	if asOf.After(now.Add(time.Minute)) {
		fmt.Fprintln(os.Stderr, "--as-of is in the future")
		return f, loadseed.Spec{}, errUsage
	}
	// Sessions are minted now while the history, running timers, receipts
	// and backlog are dated from as-of: the two may differ by a day at most.
	if asOf.Before(now.Add(-maxAsOfAge)) {
		fmt.Fprintf(os.Stderr, "--as-of is more than %s before now\n", maxAsOfAge)
		return f, loadseed.Spec{}, errUsage
	}
	var spec loadseed.Spec
	switch f.history {
	case "r10k":
		spec = loadseed.R10k(f.seed, asOf)
		spec.Families = f.families
	case "small":
		spec = loadseed.Small(f.families, f.seed, asOf)
	default:
		fmt.Fprintln(os.Stderr, "--history must be r10k or small")
		return f, loadseed.Spec{}, errUsage
	}
	spec.BacklogHours, spec.BacklogLoad, spec.Workers, spec.HotFamily = f.backlogHours, f.backlogLoad, f.workers, f.hotFamily
	if err := spec.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return f, loadseed.Spec{}, errUsage
	}
	return f, spec, nil
}

// secretFiles maps the app's file variables to the files the load stack's
// secrets directory holds (Server/scripts/lib/disposable-sync.sh), so the
// seeder loads the very peppers and keys the server loads.
var secretFiles = map[string]string{
	"BT_DATABASE_URL_FILE":                      "runtime_database_url",
	"BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE": "maintenance_database_url",
	"BT_IDENTITY_PEPPER_FILE":                   "identity_pepper",
	"BT_SESSION_TOKEN_PEPPER_FILE":              "session_token_pepper",
	"BT_DEVICE_CONTINUITY_PEPPER_FILE":          "device_continuity_pepper",
	"BT_APPLE_NOTIFICATION_PEPPER_FILE":         "apple_notification_pepper",
	"BT_INVITATION_PEPPER_FILE":                 "invitation_pepper",
	"BT_PAGE_TOKEN_KEY_FILE":                    "page_token_key",
	"BT_ACTOR_ID_PEPPER_FILE":                   "actor_id_pepper",
	"BT_RATE_LIMIT_PEPPER_FILE":                 "rate_limit_pepper",
}

// passedThrough are the app's non-secret variables the seeder reads from
// its environment, with the load node's values (Server/compose.yaml and
// deploy/load) as defaults. The public origin and the Apple audience have
// none: the guard checks what the stack names, never a default. Every other variable is ignored: the seeder
// composes no APNs, MCP, export or pool settings.
var passedThrough = map[string]string{
	"BT_MODE":                                 "hosted",
	"BT_SERVER_INSTANCE_ID":                   "",
	"BT_RECOVERY_EPOCH":                       "1",
	"BT_APPLE_AUDIENCE":                       "",
	"BT_PUBLIC_ORIGIN":                        "",
	"BT_TRUSTED_PROXY_CIDRS":                  "172.31.250.100/32",
	"BT_IDENTITY_KEY_ID":                      "identity.v1",
	"BT_SESSION_TOKEN_KEY_ID":                 "session.v1",
	"BT_DEVICE_CONTINUITY_KEY_ID":             "device.v1",
	"BT_APPLE_NOTIFICATION_KEY_ID":            "notification.v1",
	"BT_INVITATION_KEY_ID":                    "invitation.v1",
	"BT_PAGE_TOKEN_KEY_ID":                    "page.v1",
	"BT_MAX_CAREGIVERS":                       "",
	"BT_MAX_INVITATIONS":                      "",
	"BT_INVITATION_TTL":                       "",
	"BT_MAX_CANONICAL_RECORDS_PER_FAMILY":     "",
	"BT_MAX_CHARGED_STORAGE_BYTES_PER_FAMILY": "",
}

// loadConfig is the node's configuration as the identitytest server loads
// it (cmd/baby-tracker-server loadServerConfig): hosted is loaded as
// self-hosted and then marked hosted, because the hosted adapter is the
// identitytest build's.
func loadConfig(secrets string, getenv func(string) string) (config.Config, error) {
	if err := loadtest.PrivateDirectory(secrets); err != nil {
		return config.Config{}, err
	}
	for _, name := range []string{"BT_PUBLIC_ORIGIN", "BT_APPLE_AUDIENCE", "BT_SERVER_INSTANCE_ID"} {
		if getenv(name) == "" {
			return config.Config{}, fmt.Errorf("refusing to seed: %s is not set; the load stack names its own", name)
		}
	}
	values := map[string]string{}
	for name, fallback := range passedThrough {
		if value := getenv(name); value != "" {
			values[name] = value
		} else if fallback != "" {
			values[name] = fallback
		}
	}
	for name, file := range secretFiles {
		values[name] = filepath.Join(secrets, file)
	}
	hosted := values["BT_MODE"] == string(config.ModeHosted)
	if hosted {
		values["BT_MODE"] = string(config.ModeSelfHosted)
	}
	cfg, err := config.Load(func(name string) string { return values[name] })
	if err != nil {
		return config.Config{}, err
	}
	if hosted {
		cfg.Mode = config.ModeHosted
	}
	if err := disposable(cfg); err != nil {
		return config.Config{}, err
	}
	bundle := getenv("BT_APNS_BUNDLE_ID")
	if bundle == "" {
		bundle = loadseed.DefaultBundleID
	}
	cfg.LiveActivity.Credentials.BundleID = bundle
	return cfg, nil
}

// disposable refuses a configuration that does not name a disposable
// stack: the fleet holds working credentials, so the seeder only ever
// mints them against a throwaway stack's throwaway peppers. It is the
// first of two checks; the second is the target's own: loadseed refuses a
// database not marked as a load stack (loadseed.StackMarker) before it
// writes anything.
func disposable(cfg config.Config) error {
	host := cfg.PublicOrigin.Hostname()
	if !strings.HasSuffix(host, ".test") && host != "localhost" {
		return fmt.Errorf("refusing to seed %s: the load seeder runs only against a disposable .test origin", cfg.PublicOrigin)
	}
	if !strings.HasPrefix(cfg.AppleAudience, "com.example.") {
		return errors.New("refusing to seed: the Apple audience is not a com.example test audience")
	}
	return nil
}

// superuserURL is the bootstrap superuser's URL on the runtime URL's
// server and database: superuser_database_url when the secrets hold one,
// else postgres with bootstrap_password.
func superuserURL(secrets, runtimeURL string) (string, error) {
	if raw, err := os.ReadFile(filepath.Join(secrets, "superuser_database_url")); err == nil {
		return strings.TrimSpace(string(raw)), nil
	}
	raw, err := os.ReadFile(filepath.Join(secrets, "bootstrap_password"))
	if err != nil {
		return "", errors.New("the secrets hold neither superuser_database_url nor bootstrap_password")
	}
	u, err := url.Parse(runtimeURL)
	if err != nil {
		return "", errors.New("the runtime database URL is invalid")
	}
	u.User = url.UserPassword("postgres", strings.TrimSpace(string(raw)))
	return u.String(), nil
}

// seedReport is the report file: the seed's counts and stage times, its
// spec and digest, and what it did not seed. It holds no credential.
type seedReport struct {
	loadseed.Report
	PhaseSeconds   map[string]float64
	TotalSeconds   float64
	Spec           loadseed.Spec
	SeedSpecSHA256 string
	Workers        int
	AsOf           time.Time
	Notes          []string
}

func seedCommand(ctx context.Context, args []string, getenv func(string) string, logger *slog.Logger) error {
	flags, spec, err := parseSeedFlags(args, time.Now())
	if err != nil {
		return err
	}
	if err := loadtest.PrivateDirectory(filepath.Dir(flags.fleet)); err != nil {
		return fmt.Errorf("fleet: %w", err)
	}
	cfg, err := loadConfig(flags.secrets, getenv)
	if err != nil {
		return err
	}
	superuser, err := superuserURL(flags.secrets, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	runtimePool, err := loadseed.OpenPool(ctx, cfg.DatabaseURL, int32(spec.Workers+8), false)
	if err != nil {
		return errors.New("the runtime database could not be opened")
	}
	defer runtimePool.Close()
	superuserPool, err := loadseed.OpenPoolWithMemory(ctx, superuser, 32, true, flags.memory)
	if err != nil {
		return errors.New("the superuser database could not be opened")
	}
	defer superuserPool.Close()

	started := time.Now()
	logger.Info("baby-tracker-load seed", "families", spec.Families, "seed", spec.Seed, "as_of", spec.AsOf, "workers", spec.Workers,
		"backlog_rows", spec.BacklogRows(spec.SeededDevices()), "hot_family", spec.HotFamily)
	report, header, fleet, err := loadseed.Seed(ctx, loadseed.Environment{Config: cfg, Runtime: runtimePool, Superuser: superuserPool, Logger: logger}, spec)
	if err != nil {
		return err
	}
	if err := loadtest.WriteFleet(flags.fleet, header, fleet); err != nil {
		return err
	}
	out := seedReport{Report: report, PhaseSeconds: map[string]float64{}, TotalSeconds: time.Since(started).Seconds(), Spec: spec,
		SeedSpecSHA256: header.SeedSpecSHA256, AsOf: spec.AsOf, Workers: spec.Workers,
		Notes: []string{
			"audit rows come only from the provisioning commands and the refreshes the devices make before their requests (about 15 per family at small scale); the consumed-session backlog writes none (decision D1: routine refresh writes no audit row)",
			"account, device, family and token IDs are random by the product's design; the seed fixes every shape, timestamp and care payload",
			"backlog sessions carry their device's real lineage root and lineage start (the seed time) while they were created and consumed in the backlog hours before as-of: a combination the product never writes; no constraint rejects it, and it is harmless for the retention sweep and the session indexes",
			"PhaseSeconds do not sum to TotalSeconds: the backlog loads alongside the care stage and the care baby key validates alongside the side stage, in no phase (the 'care baby key re-added' log line times it); TotalSeconds, the wall time, is the seed's figure",
		}}
	for name, d := range report.Phases {
		out.PhaseSeconds[name] = d.Seconds()
	}
	if err := writeJSON(flags.report, out); err != nil {
		return err
	}
	logger.Info("baby-tracker-load seed done", "families", report.Families, "devices", report.Devices, "care_entities", report.CareEntities,
		"receipts", report.Receipts, "consumed_sessions", report.ConsumedSessions, "database_bytes", report.DatabaseBytes,
		"seconds", out.TotalSeconds)
	return nil
}

// writeJSON writes value to path atomically: a uniquely named temporary
// file in the same directory, synced, then renamed.
func writeJSON(path string, value any) (resultErr error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}()
	if err := file.Chmod(0o644); err != nil {
		return err
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
