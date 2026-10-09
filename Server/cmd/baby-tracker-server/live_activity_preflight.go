package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/migrate"
	"babytracker/server/migrations"

	"github.com/jackc/pgx/v5/pgxpool"
)

// live-activity-preflight checks, before push is enabled, everything a key
// arrival can get wrong: the mounted key, the Apple identifiers, the topic,
// outbound HTTP/2 to Apple with the real provider token, and the worker
// database login. It prints one line per check and exits 0 only when every
// selected check passed. It never prints the key, a token or a database URL.

const (
	liveActivityPreflightUsage    = "usage: baby-tracker-server live-activity-preflight [--environment sandbox|production|both] [--skip-apple] [--skip-database]"
	liveActivityPreflightDeadline = 90 * time.Second
)

// preflightTransport is Apple's transport as the preflight uses it. Production
// constructs liveactivity.NewAPNSTransport, whose hosts are fixed; tests
// substitute a scripted answer.
type preflightTransport interface {
	Preflight(context.Context, string) (liveactivity.PreflightResult, error)
	CloseIdleConnections()
}

type liveActivityPreflight struct {
	getenv    func(string) string
	out       io.Writer
	transport func(config.LiveActivityConfig) (preflightTransport, error)
}

func liveActivityPreflightCommand(arguments []string) {
	command := &liveActivityPreflight{getenv: os.Getenv, out: os.Stdout, transport: func(cfg config.LiveActivityConfig) (preflightTransport, error) {
		return liveactivity.NewAPNSTransport(cfg)
	}}
	os.Exit(command.run(context.Background(), arguments))
}

type preflightReport struct {
	out    io.Writer
	failed int
}

func (r *preflightReport) line(status, check, detail string) {
	if status == "FAIL" || status == "UNKNOWN" || status == "RETRY" {
		r.failed++
	}
	fmt.Fprintf(r.out, "[%s] %s: %s\n", status, check, detail)
}

// run returns the process exit code: 0 all selected checks passed, 1 a check
// failed or could not be decided, 2 usage.
func (p *liveActivityPreflight) run(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("live-activity-preflight", flag.ContinueOnError)
	flags.SetOutput(p.out)
	environment := flags.String("environment", "sandbox", "APNs environment to contact: sandbox (development-signed builds), production (TestFlight/App Store builds) or both")
	skipApple := flags.Bool("skip-apple", false, "run only the local and database checks; send nothing to Apple")
	skipDatabase := flags.Bool("skip-database", false, "run only the local and Apple checks; open no database connection (before the worker login has its password)")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(p.out, liveActivityPreflightUsage)
		return 2
	}
	var environments []string
	switch *environment {
	case "sandbox", "production":
		environments = []string{*environment}
	case "both":
		environments = []string{"sandbox", "production"}
	default:
		fmt.Fprintln(p.out, liveActivityPreflightUsage)
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, liveActivityPreflightDeadline)
	defer cancel()
	report := &preflightReport{out: p.out}
	fmt.Fprintf(p.out, "live-activity-preflight: environments=%s\n", strings.Join(environments, ","))

	credentials, err := config.LoadAPNSCredentials(p.getenv)
	if err != nil {
		// Configuration messages name environment variables, never contents.
		report.line("FAIL", "credentials", err.Error())
	} else {
		report.line("PASS", "key file", "regular file, mode 0600, P-256 private key")
		report.line("PASS", "identifiers", fmt.Sprintf("team %s, key ID %s, bundle %s", credentials.TeamID, credentials.KeyID, credentials.BundleID))
		report.line("PASS", "topic", credentials.BundleID+".push-type.liveactivity")
	}
	p.apple(ctx, report, credentials, err == nil, environments, *skipApple)
	if *skipDatabase {
		report.line("SKIP", "worker database", "skipped by --skip-database")
	} else {
		p.database(ctx, report)
	}

	if report.failed == 0 {
		fmt.Fprintln(p.out, "RESULT: PASS")
		return 0
	}
	fmt.Fprintf(p.out, "RESULT: FAIL (%d check(s) failed)\n", report.failed)
	return 1
}

func (p *liveActivityPreflight) apple(ctx context.Context, report *preflightReport, credentials config.LiveActivityConfig, valid bool, selected []string, skip bool) {
	var transport preflightTransport
	skipReason := ""
	switch {
	case skip:
		skipReason = "skipped by --skip-apple"
	case !valid:
		skipReason = "credentials invalid"
	default:
		var err error
		transport, err = p.transport(credentials)
		if err != nil {
			report.line("FAIL", "apple", "transport unavailable - "+err.Error())
			skipReason = "transport unavailable"
		} else {
			defer transport.CloseIdleConnections()
		}
	}
	for _, environment := range []string{"sandbox", "production"} {
		check := "apple " + environment
		switch {
		case !slices.Contains(selected, environment):
			report.line("SKIP", check, "not selected")
		case skipReason != "":
			report.line("SKIP", check, skipReason)
		default:
			result, err := transport.Preflight(ctx, environment)
			if err != nil {
				report.line("FAIL", check, "request not sent - "+err.Error())
				continue
			}
			status, detail := describePreflight(result)
			report.line(status, check, detail)
		}
	}
}

func describePreflight(result liveactivity.PreflightResult) (string, string) {
	answer := fmt.Sprintf("%d %s", result.HTTPStatus, result.Reason)
	if result.Reason == "" {
		answer = fmt.Sprintf("%d", result.HTTPStatus)
	}
	switch result.Outcome {
	case liveactivity.PreflightPass:
		return "PASS", answer + " - key, team ID and key ID accepted; topic not contradicted (a dummy token cannot prove it)"
	case liveactivity.PreflightCredentials:
		return "FAIL", answer + " - key, team ID or key ID rejected"
	case liveactivity.PreflightTopic:
		return "FAIL", answer + " - bundle ID or topic rejected for this key"
	case liveactivity.PreflightRetryLater:
		return "RETRY", answer + " - rate limited, retry later"
	case liveactivity.PreflightConnectivity:
		return "FAIL", "no answer - connectivity (DNS, TCP, TLS or timeout)"
	default:
		return "UNKNOWN", answer + " - not a recognised verdict"
	}
}

// database runs the activation's worker checks when a worker URL file is
// configured: the schema is exactly the embedded version, the login holds the
// exact baby_tracker_live_activity inventory, and (with the runtime URL) a
// shared snapshot proves both logins use one database. Errors are reported by
// safe code only; no pgx message can carry the URL into the output.
func (p *liveActivityPreflight) database(ctx context.Context, report *preflightReport) {
	const check = "worker database"
	if p.getenv("BT_LIVE_ACTIVITY_DATABASE_URL") != "" {
		report.line("FAIL", check, config.ErrLiveActivityInlineDatabaseURL.Error())
		return
	}
	path := p.getenv("BT_LIVE_ACTIVITY_DATABASE_URL_FILE")
	if path == "" {
		report.line("SKIP", check, "BT_LIVE_ACTIVITY_DATABASE_URL_FILE not set")
		return
	}
	workerURL, err := config.ReadLiveActivityDatabaseURL(path)
	if err != nil {
		report.line("FAIL", check, err.Error())
		return
	}
	// The runtime URL is optional for the preflight (worker-only checks then
	// run), but a configured runtime URL that cannot be read is a failure, not
	// a silent downgrade of the same-database proof.
	runtimeURL, runtimeErr := config.DatabaseURL(p.getenv)
	if runtimeErr != nil && (p.getenv("BT_DATABASE_URL") != "" || p.getenv("BT_DATABASE_URL_FILE") != "") {
		report.line("FAIL", check, "runtime "+runtimeErr.Error())
		return
	}
	ctx, cancel := context.WithTimeout(ctx, liveActivityValidationTimeout)
	defer cancel()
	worker, err := openPreflightPool(ctx, workerURL)
	if err != nil {
		report.line("FAIL", check, liveactivity.SafeErrorCode(err))
		return
	}
	defer worker.Close()
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		report.line("FAIL", check, "embedded_compatibility_invalid")
		return
	}
	if runtimeErr != nil {
		// The worker may read schema_compatibility; schema_migrations is not
		// in its inventory, so the version is checked against the embedded
		// exact range rather than the full migration history.
		var version int64
		if err := worker.QueryRow(ctx, "SELECT current_version FROM schema_compatibility WHERE singleton").Scan(&version); err != nil {
			report.line("FAIL", check, liveactivity.SafeErrorCode(err))
			return
		}
		if version < compatibility.Min || version > compatibility.Max || version < 28 {
			report.line("FAIL", check, "live_activity_schema_unavailable")
			return
		}
		if err := database.VerifyLiveActivityRole(ctx, worker, version); err != nil {
			report.line("FAIL", check, liveactivity.SafeErrorCode(err))
			return
		}
		report.line("PASS", check, fmt.Sprintf("schema %d, exact baby_tracker_live_activity inventory; same-database proof skipped (BT_DATABASE_URL or BT_DATABASE_URL_FILE not set)", version))
		return
	}
	runtime, err := openPreflightPool(ctx, runtimeURL)
	if err != nil {
		report.line("FAIL", check, "runtime "+liveactivity.SafeErrorCode(err))
		return
	}
	defer runtime.Close()
	version, err := migrate.New(migrations.Files).CompatibleVersion(ctx, runtime, compatibility)
	if err != nil || version < 28 {
		report.line("FAIL", check, "live_activity_schema_unavailable")
		return
	}
	if err := database.VerifyLiveActivityRoleSeparation(ctx, runtime, worker, version); err != nil {
		report.line("FAIL", check, liveactivity.SafeErrorCode(err))
		return
	}
	report.line("PASS", check, fmt.Sprintf("schema %d, exact baby_tracker_live_activity inventory, same database as runtime", version))
}

// openPreflightPool connects once, like the worker pool at activation: at most
// two connections and nothing opened before the first query.
func openPreflightPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, database.ErrOpen
	}
	poolConfig.MaxConns = int32(liveActivityWorkerConnections)
	poolConfig.MinConns = 0
	poolConfig.MinIdleConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, database.ErrOpen
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// liveActivityShutdownRequested tells a shutdown signal that interrupted the
// boot activation attempt from a configuration failure: the former is a clean
// stop, the latter an exit.
func liveActivityShutdownRequested(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err())
}
