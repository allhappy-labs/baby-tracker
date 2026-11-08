//go:build identitytest

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"babytracker/server/internal/loadseed"
	"babytracker/server/internal/loadtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// auditFlags are the audit command's options.
type auditFlags struct {
	superuserURLFile, ledger, tracked, out, result, host, verdict, thresholds string
}

func parseAuditFlags(args []string) (auditFlags, error) {
	var f auditFlags
	flags := flag.NewFlagSet("audit", flag.ContinueOnError)
	flags.StringVar(&f.superuserURLFile, "superuser-url-file", "", "a file holding the load stack's superuser database URL")
	flags.StringVar(&f.ledger, "ledger", "", "the run's write ledger (JSON lines)")
	flags.StringVar(&f.tracked, "tracked", "", "the run's tracked devices (JSON lines)")
	flags.StringVar(&f.out, "out", "", "the audit report to write (JSON)")
	flags.StringVar(&f.result, "result", "", "the run's result.json: the audit and host verdict are folded into it")
	flags.StringVar(&f.host, "host", "", "the runner's host.json (machine state over the run)")
	flags.StringVar(&f.verdict, "verdict", "", "the verdict to write (default: verdict.json beside --out)")
	flags.StringVar(&f.thresholds, "thresholds", "", "the thresholds the run is judged by (default: the spec's)")
	if err := flags.Parse(args); err != nil {
		return f, errUsage
	}
	if flags.NArg() != 0 || f.superuserURLFile == "" || f.ledger == "" || f.tracked == "" || f.out == "" || f.result == "" || f.host == "" {
		fmt.Fprintln(os.Stderr, "audit needs --superuser-url-file, --ledger, --tracked, --out, --result and --host")
		return f, errUsage
	}
	if f.verdict == "" {
		f.verdict = filepath.Join(filepath.Dir(f.out), "verdict.json")
	}
	return f, nil
}

// auditReport is audit.json: the counts, the first findings and the
// verdict they lead to.
type auditReport struct {
	Audit    loadtest.AuditResult    `json:"audit"`
	Findings []loadtest.AuditFinding `json:"findings"`
	Host     loadtest.HostVerdict    `json:"host"`
	Verdict  loadtest.Verdict        `json:"verdict"`
}

func auditCommand(ctx context.Context, args []string, logger *slog.Logger) error {
	flags, err := parseAuditFlags(args)
	if err != nil {
		return err
	}
	// Everything the verdict needs is read before the database is touched:
	// a malformed input fails the audit, never passes it.
	resultRaw, err := os.ReadFile(flags.result)
	if err != nil {
		return fmt.Errorf("result: %w", err)
	}
	host, err := readHost(flags.host)
	if err != nil {
		return err
	}
	thresholds := loadtest.SpecThresholds()
	if flags.thresholds != "" {
		if thresholds, err = loadtest.LoadThresholds(flags.thresholds); err != nil {
			return err
		}
	}
	ledger, writes, err := loadtest.ReadLedgerCounted(flags.ledger)
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	tracked, err := loadtest.ReadTracked(flags.tracked)
	if err != nil {
		return fmt.Errorf("tracked: %w", err)
	}
	if _, _, err := foldAudit(resultRaw, loadtest.AuditResult{}, host, thresholds); err != nil {
		return err
	}

	raw, err := os.ReadFile(flags.superuserURLFile)
	if err != nil {
		return errors.New("the superuser database URL file could not be read")
	}
	cfg, err := pgxpool.ParseConfig(strings.TrimSpace(string(raw)))
	if err != nil {
		return errors.New("the superuser database URL is invalid")
	}
	cfg.MaxConns, cfg.MinConns = 2, 0
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.ConnConfig.RuntimeParams["application_name"] = "baby-tracker-load audit"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return errors.New("the superuser database could not be opened")
	}
	defer pool.Close()
	// The database names itself before anything is read: only a disposable
	// load stack is audited, whatever URL the file holds.
	if _, err := loadseed.CheckStack(ctx, pool); err != nil {
		return fmt.Errorf("refusing to audit: %w", err)
	}

	logger.Info("baby-tracker-load audit", "acknowledged_entities", len(ledger), "acknowledged_writes", writes, "tracked_devices", len(tracked))
	result, findings, err := loadtest.AuditWithFindings(ctx, pool, ledger, tracked)
	if err != nil {
		return err
	}
	result.AcknowledgedWrites = writes
	folded, verdict, err := foldAudit(resultRaw, result, host, thresholds)
	if err != nil {
		return err
	}
	if findings == nil {
		findings = []loadtest.AuditFinding{}
	}
	if err := writeJSON(flags.out, auditReport{Audit: result, Findings: findings, Host: host, Verdict: verdict}); err != nil {
		return err
	}
	if err := writeRaw(flags.result, folded); err != nil {
		return err
	}
	if err := writeJSON(flags.verdict, verdict); err != nil {
		return err
	}
	logger.Info("baby-tracker-load audit done", "pass", verdict.Pass, "acknowledged_entities", result.Acknowledged,
		"acknowledged_writes", result.AcknowledgedWrites, "lost", result.Lost, "cross_family", result.CrossFamily, "diverged", result.Diverged,
		"tracked_devices", result.TrackedDevices, "tracked_diverged", result.TrackedDiverged, "tracked_skipped", result.TrackedSkipped,
		"host_contaminated", host.Contaminated, "reasons", strings.Join(verdict.Reasons, "; "))
	return nil
}

// readHost reads the runner's host.json. "contaminated" must be present:
// a record that does not say the machine was clean proves nothing.
func readHost(path string) (loadtest.HostVerdict, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return loadtest.HostVerdict{}, fmt.Errorf("host: %w", err)
	}
	var present struct {
		Contaminated *bool `json:"contaminated"`
	}
	if err := json.Unmarshal(raw, &present); err != nil {
		return loadtest.HostVerdict{}, fmt.Errorf("host %s: %w", path, err)
	}
	if present.Contaminated == nil {
		return loadtest.HostVerdict{}, fmt.Errorf("host %s: no \"contaminated\" verdict", path)
	}
	var host loadtest.HostVerdict
	if err := json.Unmarshal(raw, &host); err != nil {
		return loadtest.HostVerdict{}, fmt.Errorf("host %s: %w", path, err)
	}
	return host, nil
}

// foldAudit sets the audit and the host verdict in the generator's
// result.json and evaluates the run. result.json is the RunSummary's fields
// at its top level plus whatever else the generator writes (per-step and
// per-window summaries); every other key is kept as it was.
func foldAudit(resultRaw []byte, audit loadtest.AuditResult, host loadtest.HostVerdict, t loadtest.Thresholds) ([]byte, loadtest.Verdict, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(resultRaw, &fields); err != nil || fields == nil {
		return nil, loadtest.Verdict{}, fmt.Errorf("result: not a JSON object: %v", err)
	}
	if routes, ok := fields["routes"]; !ok || bytes.Equal(bytes.TrimSpace(routes), []byte("null")) {
		return nil, loadtest.Verdict{}, errors.New("result: no \"routes\": not a run summary")
	}
	var summary loadtest.RunSummary
	if err := json.Unmarshal(resultRaw, &summary); err != nil {
		return nil, loadtest.Verdict{}, fmt.Errorf("result: %w", err)
	}
	summary.Audit, summary.Host = audit, host
	for key, value := range map[string]any{"audit": audit, "host": host} {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, loadtest.Verdict{}, err
		}
		fields[key] = raw
	}
	folded, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return nil, loadtest.Verdict{}, err
	}
	return folded, loadtest.Evaluate(summary, t), nil
}

// writeRaw is writeJSON for an already encoded document.
func writeRaw(path string, raw []byte) error {
	return writeJSON(path, json.RawMessage(raw))
}
