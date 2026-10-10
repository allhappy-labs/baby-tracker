//go:build identitytest

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"babytracker/server/internal/capacity"
)

func main() { os.Exit(command()) }
func command() int {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "capacity phase required: fixture, run, report")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "fixture":
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		f := flag.NewFlagSet("fixture", flag.ContinueOnError)
		dest := f.String("destination", "", "new private fixture directory")
		families := f.Int("families", 1, "synthetic families")
		devices := f.Int("devices-per-family", 2, "devices per family")
		events := f.Int("authority-events", 248, "minimum real authority events")
		care := f.Int("care-entries-per-baby", 0, "diaper history count")
		grants := f.Int("mcp-grants-per-family", 0, "private MCP grants issued per family (same fixture for all matched cells)")
		if f.Parse(os.Args[2:]) != nil {
			return 2
		}
		m, err := capacity.BuildFixture(ctx, capacity.FixtureSpec{Families: *families, DevicesPerFamily: *devices, AuthorityEvents: *events, CareEntriesPerBaby: *care, MCPGrantsPerFamily: *grants}, *dest)
		if err != nil {
			fmt.Fprintln(os.Stderr, "capacity_fixture_failed")
			return 1
		}
		if json.NewEncoder(os.Stdout).Encode(m) != nil {
			return 1
		}
		return 0
	case "report":
		f := flag.NewFlagSet("report", flag.ContinueOnError)
		input := f.String("input", "", "private checkpoint")
		if f.Parse(os.Args[2:]) != nil {
			return 2
		}
		r, err := capacity.ReadResult(*input)
		if err != nil {
			fmt.Fprintln(os.Stderr, "capacity_result_invalid")
			return 1
		}
		if json.NewEncoder(os.Stdout).Encode(r) != nil {
			return 1
		}
		return 0
	case "run":
		f := flag.NewFlagSet("run", flag.ContinueOnError)
		swiftMode := f.String("swift-mode", "latency", "Swift diagnostic kind")
		swiftPackage := f.String("swift-package", "", "separately measured Swift peer package")
		metrics := f.Bool("metrics", false, "bounded diagnostics")
		databaseProfile := f.String("database-profile", "legacy", "legacy or complete explicit laboratory database profile")
		fixture := f.String("fixture", "", "sealed fixture")
		output := f.String("output", "", "private output")
		image := f.String("image", "", "owned release image")
		profile := f.String("profile", "standard", "resource profile")
		scenario := f.String("scenario", "pilot", "scenario")
		devices := f.Int("devices", 32, "connected devices")
		rate := f.Int("rate", 15, "writes/writer/minute")
		seconds := f.Int("measure-seconds", 60, "steady window")
		sha := f.String("product-sha", "", "source SHA")
		input := f.String("product-input", "", "source hash")
		binary := f.String("product-binary", "", "binary hash")
		cell := f.String("cell", "steady", "workload shape")
		timeout := f.Int("total-seconds", 300, "positive bounded child budget")
		mcpMode := f.String("mcp-mode", "", "matched MCP cell: empty (disabled), idle, read or abusive; requires --database-profile explicit-mcp (4) or explicit-mcp-1..3 (lowered MCP concurrency)")
		mcpClients := f.Int("mcp-clients", 0, "MCP credentials offering load (read/abusive)")
		mcpRate := f.Int("mcp-rate", 0, "paced MCP calls per client per minute (read, 1..30)")
		mcpAbuse := f.Int("mcp-abuse-concurrency", 0, "unpaced concurrent workers per credential (abusive, 2..16)")
		if f.Parse(os.Args[2:]) != nil {
			return 2
		}
		if *timeout < 1 || *timeout > 7200 {
			return 2
		}
		ctx, cancel := context.WithTimeout(ctx, time.Duration(*timeout)*time.Second)
		defer cancel()
		var swiftArtifact capacity.SwiftArtifact
		if *swiftPackage != "" {
			var buildErr error
			swiftArtifact, buildErr = capacity.BuildSwiftArtifact(ctx, *swiftPackage, *sha, *output)
			if buildErr != nil {
				fmt.Fprintln(os.Stderr, "capacity_swift_build_failed", buildErr)
				return 1
			}
		}

		setupStarted := time.Now()
		r, err := capacity.RestoreFixture(ctx, *fixture, filepath.Join(*output, "restore"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "capacity_restore_failed")
			return 1
		}
		w := capacity.Workload{DatabaseProfile: *databaseProfile, Diagnostics: *metrics, Devices: *devices, ActiveWriters: *devices / 2, EmptyPullInterval: 300 * time.Second, WritesPerWriterPerMinute: *rate, Warmup: 30 * time.Second, Measure: time.Duration(*seconds) * time.Second,
			MCPMode: *mcpMode, MCPClients: *mcpClients, MCPCallsPerClientPerMinute: *mcpRate, MCPAbuseConcurrency: *mcpAbuse}
		switch *cell {
		case "steady":
		case "contention":
			w.Contention = true
			w.ActiveWriters = 2
		case "catch-up":
			w.BacklogChanges = 100
		case "slow-consumer":
			w.SlowConsumer = true
		case "reconnect":
			w.Reconnect = true
		default:
			_ = r.Close()
			return 2
		}
		w, launchErr := r.PrepareWorkload(ctx, w, *image, *profile, *output)
		result := capacity.CellResult{Version: 1, Requested: w, Completion: capacity.SetupFailed}
		if launchErr == nil {
			w = capacity.AttachProductIdentity(w, *sha, *input, *binary, *scenario)
			w = capacity.AttachSetupDuration(w, time.Since(setupStarted).Seconds())
			if *swiftPackage != "" {
				if err := r.SwiftCell(ctx, w, swiftArtifact, *output, *swiftMode); err != nil {
					return 1
				}
				return 0
			}
			result, err = capacity.Run(ctx, r.Manifest, w)
		} else {
			err = launchErr
		}
		r.CollectMetrics(ctx, w, &result)
		closeErr := r.Close()
		if closeErr != nil {
			result.Completion = capacity.CleanupFailed
			err = closeErr
		}
		if err != nil && strings.HasPrefix(err.Error(), "capacity_") {
			result.FailureCode = err.Error()
		}
		result.Finalize()
		// Final replacement remains private and survives ordinary failure. Supervisor
		// owns atomic recovery when a child dies before this point.
		if e := capacity.WriteCheckpoint(*output, result); e != nil {
			return 1
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "capacity_cell_incomplete")
			return 1
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "capacity_phase_invalid")
		return 2
	}
}
