package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

const overlapAccountID = "93000000-0000-4000-8000-000000000001"
const overlapProfileCommandID = "93000000-0000-4000-8000-000000000002"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "drain-sync-http" {
		flags := flag.NewFlagSet("drain-sync-http", flag.ContinueOnError)
		origin := flags.String("origin", "", "loopback feature origin")
		owner := flags.String("owner-curl-config", "", "private owner credential")
		cursor := flags.String("cursor-file", "", "private overlap cursor")
		if err := flags.Parse(os.Args[2:]); err != nil || *origin == "" || *owner == "" || *cursor == "" || flags.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "usage: baby-tracker-overlap-check drain-sync-http --origin URL --owner-curl-config PATH --cursor-file PATH")
			os.Exit(2)
		}
		pages, changes, err := drainSyncHTTP(context.Background(), *origin, *owner, *cursor)
		if err != nil {
			fmt.Fprintf(os.Stderr, "drain predecessor sync failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("overlap_sync_drain=verified pages=%d changes=%d\n", pages, changes)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "assert-delivery-feature" {
		flags := flag.NewFlagSet("assert-delivery-feature", flag.ContinueOnError)
		statePath := flags.String("protocol-state-file", "", "private protocol client state file")
		if err := flags.Parse(os.Args[2:]); err != nil || *statePath == "" || flags.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "usage: baby-tracker-overlap-check assert-delivery-feature --protocol-state-file PATH")
			os.Exit(2)
		}
		if err := assertDeliveryFeature(context.Background(), *statePath, os.Getenv); err != nil {
			fmt.Fprintf(os.Stderr, "assert delivery feature failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("overlap_delivery=result_feature_verified")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "probe-import-http" {
		flags := flag.NewFlagSet("probe-import-http", flag.ContinueOnError)
		origin := flags.String("origin", "", "loopback feature origin")
		owner := flags.String("owner-curl-config", "", "private owner credential")
		caregiver := flags.String("caregiver-curl-config", "", "private caregiver credential")
		phase := flags.String("phase", "", "overlap phase")
		cursorFile := flags.String("cursor-file", "", "private overlap cursor output")
		if e := flags.Parse(os.Args[2:]); e != nil || *origin == "" || *owner == "" || *caregiver == "" || *phase == "" || *cursorFile == "" || flags.NArg() != 0 {
			os.Exit(2)
		}
		if e := probeImportHTTP(context.Background(), *origin, *owner, *caregiver, *phase, func(cursor protocol.Cursor) error {
			return writeOverlapCursor(*cursorFile, cursor)
		}); e != nil {
			fmt.Fprintf(os.Stderr, "import feature HTTP probe failed: %v\n", e)
			os.Exit(1)
		}
		fmt.Println("overlap_http_import=result_verified")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "assert-import-unused" {
		flags := flag.NewFlagSet("assert-import-unused", flag.ContinueOnError)
		accountIDPath := flags.String("account-id-file", "", "private authenticated account ID file")
		familyIDPath := flags.String("family-id-file", "", "private HTTP family ID file")
		expectedVersion := flags.Int64("expected-version", 0, "expected schema version")
		if err := flags.Parse(os.Args[2:]); err != nil || *accountIDPath == "" || *familyIDPath == "" || *expectedVersion <= 0 || flags.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "usage: baby-tracker-overlap-check assert-import-unused --account-id-file PATH --family-id-file PATH --expected-version N")
			os.Exit(2)
		}
		if err := assertHTTPImportUnused(context.Background(), *accountIDPath, *familyIDPath, *expectedVersion, os.Getenv); err != nil {
			fmt.Fprintf(os.Stderr, "assert HTTP import predecessor unused failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("overlap_http_import=result_unused")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "assert-import-preserved" {
		flags := flag.NewFlagSet("assert-import-preserved", flag.ContinueOnError)
		accountIDPath := flags.String("account-id-file", "", "private authenticated account ID file")
		familyIDPath := flags.String("family-id-file", "", "private HTTP family ID file")
		expectedVersion := flags.Int64("expected-version", 0, "expected schema version")
		phase := flags.String("phase", "", "import overlap phase")
		if err := flags.Parse(os.Args[2:]); err != nil || *accountIDPath == "" || *familyIDPath == "" || *expectedVersion <= 0 || *phase == "" || flags.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "usage: baby-tracker-overlap-check assert-import-preserved --account-id-file PATH --family-id-file PATH --expected-version N --phase PHASE")
			os.Exit(2)
		}
		if err := assertHTTPImportPreserved(context.Background(), *accountIDPath, *familyIDPath, *expectedVersion, *phase, os.Getenv); err != nil {
			fmt.Fprintf(os.Stderr, "assert HTTP import preservation failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("overlap_http_import=result_preserved")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "assert-delivery-unused" {
		flags := flag.NewFlagSet("assert-delivery-unused", flag.ContinueOnError)
		accountIDPath := flags.String("account-id-file", "", "private authenticated account ID file")
		familyIDPath := flags.String("family-id-file", "", "private HTTP family ID file")
		expectedVersion := flags.Int64("expected-version", 0, "expected schema version")
		if err := flags.Parse(os.Args[2:]); err != nil || *accountIDPath == "" || *familyIDPath == "" || *expectedVersion <= 0 || flags.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "usage: baby-tracker-overlap-check assert-delivery-unused --account-id-file PATH --family-id-file PATH --expected-version N")
			os.Exit(2)
		}
		if err := assertHTTPDeliveryUnused(context.Background(), *accountIDPath, *familyIDPath, *expectedVersion, os.Getenv); err != nil {
			fmt.Fprintf(os.Stderr, "assert HTTP delivery predecessor unused failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("overlap_http_delivery=result_unused")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "verify-error-response" {
		flags := flag.NewFlagSet("verify-error-response", flag.ContinueOnError)
		responsePath := flags.String("response", "", "error response body")
		headerPath := flags.String("headers", "", "error response headers")
		expectedStatus := flags.Int("expected-status", 0, "expected HTTP status")
		expectedCode := flags.String("expected-code", "", "expected stable error code")
		if err := flags.Parse(os.Args[2:]); err != nil || *responsePath == "" || *headerPath == "" || *expectedStatus <= 0 || *expectedCode == "" || flags.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "usage: baby-tracker-overlap-check verify-error-response --response PATH --headers PATH --expected-status N --expected-code CODE")
			os.Exit(2)
		}
		if err := verifyOverlapErrorResponse(*responsePath, *headerPath, *expectedStatus, *expectedCode); err != nil {
			fmt.Fprintf(os.Stderr, "verify overlap error response failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("overlap_error=result_verified")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "write-session-curl-config" {
		flags := flag.NewFlagSet("write-session-curl-config", flag.ContinueOnError)
		responsePath := flags.String("response", "", "session response body")
		curlConfig := flags.String("curl-config", "", "private curl configuration output")
		familyIDPath := flags.String("family-id-file", "", "stable family ID file")
		if err := flags.Parse(os.Args[2:]); err != nil || *responsePath == "" || *curlConfig == "" || *familyIDPath == "" || flags.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "usage: baby-tracker-overlap-check write-session-curl-config --response PATH --curl-config PATH --family-id-file PATH")
			os.Exit(2)
		}
		if err := writeOverlapSessionCurlConfig(*responsePath, *curlConfig, *familyIDPath); err != nil {
			fmt.Fprintf(os.Stderr, "write overlap session curl config failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("overlap_session=result_refreshed")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "verify-invitation-response" {
		flags := flag.NewFlagSet("verify-invitation-response", flag.ContinueOnError)
		responsePath := flags.String("response", "", "invitation response body")
		headerPath := flags.String("headers", "", "invitation response headers")
		familyIDPath := flags.String("family-id-file", "", "stable family ID file")
		invitationIDPath := flags.String("invitation-id-file", "", "stable invitation ID file")
		expectedStatus := flags.Int("expected-status", 0, "expected HTTP status")
		captureInvitationID := flags.Bool("capture-invitation-id", false, "capture invitation ID")
		if err := flags.Parse(os.Args[2:]); err != nil || *responsePath == "" || *headerPath == "" || *familyIDPath == "" || *invitationIDPath == "" || flags.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "usage: baby-tracker-overlap-check verify-invitation-response --response PATH --headers PATH --family-id-file PATH --invitation-id-file PATH --expected-status N [--capture-invitation-id]")
			os.Exit(2)
		}
		if err := verifyOverlapInvitationResponse(*responsePath, *headerPath, *familyIDPath, *invitationIDPath, *expectedStatus, *captureInvitationID); err != nil {
			fmt.Fprintf(os.Stderr, "verify overlap invitation response failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("overlap_invitation=result_verified")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "verify-family-response" {
		flags := flag.NewFlagSet("verify-family-response", flag.ContinueOnError)
		responsePath := flags.String("response", "", "family response body")
		headerPath := flags.String("headers", "", "family response headers")
		expectedStatus := flags.Int("expected-status", 0, "expected HTTP status")
		familyIDPath := flags.String("family-id-file", "", "stable family ID file")
		captureFamilyID := flags.Bool("capture-family-id", false, "capture family ID instead of reading it")
		if err := flags.Parse(os.Args[2:]); err != nil || *responsePath == "" || *headerPath == "" || *familyIDPath == "" || flags.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "usage: baby-tracker-overlap-check verify-family-response --response PATH --headers PATH --expected-status N --family-id-file PATH [--capture-family-id]")
			os.Exit(2)
		}
		if err := verifyOverlapFamilyResponse(*responsePath, *headerPath, *familyIDPath, *expectedStatus, *captureFamilyID); err != nil {
			fmt.Fprintf(os.Stderr, "verify overlap family response failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("overlap_family_http=result_verified")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "seed-session" {
		flags := flag.NewFlagSet("seed-session", flag.ContinueOnError)
		curlConfig := flags.String("curl-config", "", "0600 curl configuration output")
		identitySubject := flags.String("identity-subject", overlapIdentitySubject, "fixed overlap identity subject")
		refreshRequest := flags.String("refresh-request", "", "optional 0600 refresh request output")
		accountIDPath := flags.String("account-id-file", "", "optional 0600 authenticated account ID output")
		if err := flags.Parse(os.Args[2:]); err != nil || *curlConfig == "" || flags.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "usage: baby-tracker-overlap-check seed-session --curl-config ABSOLUTE_PATH")
			os.Exit(2)
		}
		cfg, err := config.Load(os.Getenv)
		if err == nil {
			err = seedOverlapFamilySessionForSubject(context.Background(), cfg, *curlConfig, *identitySubject, *refreshRequest, *accountIDPath)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "seed overlap session failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("overlap_session=result_seeded")
		return
	}
	flags := flag.NewFlagSet("baby-tracker-overlap-check", flag.ContinueOnError)
	scenario := flags.String("scenario", "", "scenario label")
	expectedVersion := flags.Int64("expected-version", 0, "expected schema version")
	if err := flags.Parse(os.Args[1:]); err != nil || *scenario == "" || *expectedVersion <= 0 || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: baby-tracker-overlap-check --scenario LABEL --expected-version N")
		os.Exit(2)
	}
	if err := run(context.Background(), *scenario, *expectedVersion, os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "overlap scenario failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("scenario=%s version=%d result=passed\n", *scenario, *expectedVersion)
}

func assertDeliveryFeature(ctx context.Context, statePath string, getenv func(string) string) error {
	stateRaw, stateErr := readBoundedOverlapFile(statePath, 64*1024)
	if stateErr != nil {
		return errors.New("invalid private delivery feature artifacts")
	}
	var state struct {
		Owner, Caregiver protocol.SessionResponse
		FamilyID         string
		Cursor           protocol.Cursor
	}
	if json.Unmarshal(stateRaw, &state) != nil || !validOverlapUUID(state.FamilyID) || state.Cursor.Validate() != nil || state.Cursor.FamilyID != state.FamilyID ||
		!validOverlapUUID(state.Owner.Account.ID) || state.Owner.DeviceID == "" || state.Caregiver.DeviceID == "" || state.Owner.DeviceID == state.Caregiver.DeviceID {
		return errors.New("invalid protocol state artifact")
	}
	migratorURL := getenv("BT_OVERLAP_MIGRATOR_DATABASE_URL")
	if migratorURL == "" {
		return errors.New("overlap migrator database credential is required")
	}
	migrator, err := database.OpenAndPing(ctx, migratorURL)
	if err != nil {
		return fmt.Errorf("open overlap migrator database: %w", err)
	}
	defer migrator.Close()
	tx, err := migrator.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delivery feature assertion: %w", err)
	}
	defer tx.Rollback(ctx)
	if err = tenant.SetLocal(ctx, tx, tenant.Context{AccountID: state.Owner.Account.ID, FamilyID: state.FamilyID}); err != nil {
		return fmt.Errorf("set delivery feature tenant: %w", err)
	}
	var entitlementRows int64
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM family_entitlements
		WHERE family_id=$1 AND entitlement_key='family_sync' AND source='hosted_beta' AND status='active'
		AND (expires_at IS NULL OR expires_at > now())`, state.FamilyID).Scan(&entitlementRows); err != nil || entitlementRows != 1 {
		return fmt.Errorf("feature authority entitlement rows=%d: %v", entitlementRows, err)
	}
	var protocolRows int64
	var deviceID string
	var instanceID string
	var epoch, sequence, generation int64
	if err = tx.QueryRow(ctx, `SELECT count(*),COALESCE(min(device_id::text),''),COALESCE(min(instance_id::text),''),COALESCE(min(recovery_epoch),0),COALESCE(min(sequence),0),COALESCE(min(generation),0)
		FROM delivery_pending WHERE family_id=$1`, state.FamilyID).Scan(&protocolRows, &deviceID, &instanceID, &epoch, &sequence, &generation); err != nil ||
		protocolRows != 1 || deviceID != state.Caregiver.DeviceID || deviceID == state.Owner.DeviceID || instanceID != state.Cursor.ServerInstanceID ||
		epoch != int64(state.Cursor.RecoveryEpoch) || sequence != int64(state.Cursor.Sequence) || generation < 2 {
		return fmt.Errorf("protocol durable wake rows=%d device_match=%t actor_excluded=%t cursor_match=%t generation=%d: %v", protocolRows,
			deviceID == state.Caregiver.DeviceID, deviceID != state.Owner.DeviceID,
			instanceID == state.Cursor.ServerInstanceID && epoch == int64(state.Cursor.RecoveryEpoch) && sequence == int64(state.Cursor.Sequence), generation, err)
	}
	return tx.Commit(ctx)
}

var importPredecessorTables = []string{
	"import_sessions", "import_chunks", "import_baby_candidates", "import_record_manifests",
	"import_conflicts", "import_conflict_receipts", "import_baby_mappings",
}

func assertHTTPImportUnused(ctx context.Context, accountIDPath, familyIDPath string, expectedVersion int64, getenv func(string) string) error {
	if expectedVersion <= 0 {
		return errors.New("expected schema version is required")
	}
	accountBytes, accountErr := readBoundedOverlapFile(accountIDPath, 128)
	familyBytes, familyErr := readBoundedOverlapFile(familyIDPath, 128)
	accountID, familyID := strings.TrimSpace(string(accountBytes)), strings.TrimSpace(string(familyBytes))
	if accountErr != nil || familyErr != nil || !validOverlapUUID(accountID) || !validOverlapUUID(familyID) {
		return errors.New("invalid private HTTP tenant artifacts")
	}
	runtimeURL := getenv("BT_OVERLAP_RUNTIME_DATABASE_URL")
	if runtimeURL == "" {
		return errors.New("overlap runtime database credential is required")
	}
	runtime, err := database.OpenAndPing(ctx, runtimeURL)
	if err != nil {
		return fmt.Errorf("open overlap runtime database: %w", err)
	}
	defer runtime.Close()
	var schemaVersion int64
	if err := runtime.QueryRow(ctx, "SELECT COALESCE(max(version),0) FROM schema_migrations").Scan(&schemaVersion); err != nil || schemaVersion != expectedVersion {
		return fmt.Errorf("HTTP post-probe schema version mismatch: got=%d want=%d: %w", schemaVersion, expectedVersion, err)
	}
	tx, err := runtime.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tenant.SetLocal(ctx, tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		return err
	}
	var memberships int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.memberships
		WHERE family_id=$1 AND account_id=$2 AND status='active'`, familyID, accountID).Scan(&memberships); err != nil || memberships != 1 {
		return fmt.Errorf("HTTP tenant artifacts do not identify one active membership: count=%d: %w", memberships, err)
	}
	if expectedVersion < 12 {
		var present int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname='public' AND c.relname = ANY($1)`, importPredecessorTables).Scan(&present); err != nil || present != 0 {
			return fmt.Errorf("schema-eleven unexpectedly exposes import tables count=%d: %w", present, err)
		}
		return tx.Rollback(ctx)
	}
	var rows int64
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM public.import_sessions) +
		(SELECT count(*) FROM public.import_chunks) +
		(SELECT count(*) FROM public.import_baby_candidates) +
		(SELECT count(*) FROM public.import_record_manifests) +
		(SELECT count(*) FROM public.import_conflicts) +
		(SELECT count(*) FROM public.import_conflict_receipts) +
		(SELECT count(*) FROM public.import_baby_mappings)`).Scan(&rows); err != nil || rows != 0 {
		return fmt.Errorf("HTTP compatibility predecessor used import tables rows=%d: %w", rows, err)
	}
	return tx.Rollback(ctx)
}

func assertHTTPImportPreserved(ctx context.Context, accountIDPath, familyIDPath string, expectedVersion int64, phase string, getenv func(string) string) error {
	if expectedVersion < 12 {
		return errors.New("import preservation requires schema twelve or newer")
	}
	var firstImportID, secondImportID string
	switch phase {
	case "prior_at_n_minus_1":
		firstImportID = "93000000-0000-4000-8000-000000010004"
		secondImportID = "93000000-0000-4000-8000-000000010012"
	case "prior_at_n":
		firstImportID = "93000000-0000-4000-8000-000000020004"
		secondImportID = "93000000-0000-4000-8000-000000020012"
	case "current_at_n":
		firstImportID = "93000000-0000-4000-8000-000000030004"
		secondImportID = "93000000-0000-4000-8000-000000030012"
	default:
		return fmt.Errorf("invalid import overlap phase %q", phase)
	}
	accountBytes, accountErr := readBoundedOverlapFile(accountIDPath, 128)
	familyBytes, familyErr := readBoundedOverlapFile(familyIDPath, 128)
	accountID, familyID := strings.TrimSpace(string(accountBytes)), strings.TrimSpace(string(familyBytes))
	if accountErr != nil || familyErr != nil || !validOverlapUUID(accountID) || !validOverlapUUID(familyID) {
		return errors.New("invalid private HTTP tenant artifacts")
	}
	runtimeURL := getenv("BT_OVERLAP_RUNTIME_DATABASE_URL")
	if runtimeURL == "" {
		return errors.New("overlap runtime database credential is required")
	}
	runtime, err := database.OpenAndPing(ctx, runtimeURL)
	if err != nil {
		return fmt.Errorf("open overlap runtime database: %w", err)
	}
	defer runtime.Close()
	var schemaVersion int64
	if err := runtime.QueryRow(ctx, "SELECT COALESCE(max(version),0) FROM schema_migrations").Scan(&schemaVersion); err != nil || schemaVersion != expectedVersion {
		return fmt.Errorf("HTTP post-import schema version mismatch: got=%d want=%d: %w", schemaVersion, expectedVersion, err)
	}
	tx, err := runtime.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tenant.SetLocal(ctx, tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		return err
	}
	var memberships int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.memberships
		WHERE family_id=$1 AND account_id=$2 AND status='active'`, familyID, accountID).Scan(&memberships); err != nil || memberships != 1 {
		return fmt.Errorf("HTTP import tenant membership count=%d: %w", memberships, err)
	}
	var sessions, chunks, candidates, manifests, conflicts, receipts, mappings int64
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM public.import_sessions),
		(SELECT count(*) FROM public.import_chunks),
		(SELECT count(*) FROM public.import_baby_candidates),
		(SELECT count(*) FROM public.import_record_manifests),
		(SELECT count(*) FROM public.import_conflicts),
		(SELECT count(*) FROM public.import_conflict_receipts),
		(SELECT count(*) FROM public.import_baby_mappings)`).Scan(
		&sessions, &chunks, &candidates, &manifests, &conflicts, &receipts, &mappings); err != nil {
		return fmt.Errorf("read preserved import inventory: %w", err)
	}
	if sessions != 2 || chunks != 3 || candidates != 2 || manifests != 1 || conflicts != 1 || receipts != 1 || mappings != 2 {
		return fmt.Errorf("preserved import inventory mismatch sessions=%d chunks=%d candidates=%d manifests=%d conflicts=%d receipts=%d mappings=%d",
			sessions, chunks, candidates, manifests, conflicts, receipts, mappings)
	}
	var exactSessions bool
	if err := tx.QueryRow(ctx, `SELECT count(*)=2 AND bool_and(manifest_complete AND mapping_complete AND finalized_at IS NOT NULL AND mapping_completed_at IS NOT NULL)
		FROM public.import_sessions WHERE import_id = ANY($1::uuid[])`, []string{firstImportID, secondImportID}).Scan(&exactSessions); err != nil || !exactSessions {
		return fmt.Errorf("preserved import session identities exact=%t: %w", exactSessions, err)
	}
	return tx.Rollback(ctx)
}

func assertHTTPDeliveryUnused(ctx context.Context, accountIDPath, familyIDPath string, expectedVersion int64, getenv func(string) string) error {
	accountBytes, accountErr := readBoundedOverlapFile(accountIDPath, 128)
	familyBytes, familyErr := readBoundedOverlapFile(familyIDPath, 128)
	accountID, familyID := strings.TrimSpace(string(accountBytes)), strings.TrimSpace(string(familyBytes))
	if expectedVersion <= 0 || accountErr != nil || familyErr != nil || !validOverlapUUID(accountID) || !validOverlapUUID(familyID) {
		return errors.New("invalid delivery predecessor tenant artifacts")
	}
	migratorURL := getenv("BT_OVERLAP_MIGRATOR_DATABASE_URL")
	runtimeURL := getenv("BT_OVERLAP_RUNTIME_DATABASE_URL")
	projectorURL := getenv("BT_OVERLAP_PROJECTOR_DATABASE_URL")
	if migratorURL == "" || runtimeURL == "" || projectorURL == "" || migratorURL == runtimeURL || migratorURL == projectorURL || runtimeURL == projectorURL {
		return errors.New("distinct overlap database credentials are required")
	}
	migrator, err := database.OpenAndPing(ctx, migratorURL)
	if err != nil {
		return err
	}
	defer migrator.Close()
	runtime, err := database.OpenAndPing(ctx, runtimeURL)
	if err != nil {
		return err
	}
	defer runtime.Close()
	projector, err := database.OpenAndPing(ctx, projectorURL)
	if err != nil {
		return err
	}
	defer projector.Close()
	return verifyDeliveryPredecessorUnused(ctx, migrator, runtime, projector, accountID, familyID, expectedVersion)
}

func run(ctx context.Context, scenario string, expectedVersion int64, getenv func(string) string) error {
	if scenario == "" || expectedVersion <= 0 {
		return errors.New("scenario is required")
	}
	migratorURL := getenv("BT_OVERLAP_MIGRATOR_DATABASE_URL")
	runtimeURL, projectorURL := getenv("BT_OVERLAP_RUNTIME_DATABASE_URL"), getenv("BT_OVERLAP_PROJECTOR_DATABASE_URL")
	if migratorURL == "" || runtimeURL == "" || projectorURL == "" || migratorURL == runtimeURL || migratorURL == projectorURL || runtimeURL == projectorURL {
		return errors.New("distinct overlap database credentials are required")
	}
	migrator, err := database.OpenAndPing(ctx, migratorURL)
	if err != nil {
		return err
	}
	defer migrator.Close()
	runtime, err := database.OpenAndPing(ctx, runtimeURL)
	if err != nil {
		return err
	}
	defer runtime.Close()
	projector, err := database.OpenAndPing(ctx, projectorURL)
	if err != nil {
		return err
	}
	defer projector.Close()
	var count, minimum, maximum int64
	if err := runtime.QueryRow(ctx, "SELECT count(*),COALESCE(min(version),0),COALESCE(max(version),0) FROM schema_migrations").Scan(&count, &minimum, &maximum); err != nil {
		return fmt.Errorf("read schema migration catalog: %w", err)
	}
	if count != expectedVersion || minimum != 1 || maximum != expectedVersion {
		return fmt.Errorf("schema migration catalog is not exact contiguous 1..%d: count=%d min=%d max=%d", expectedVersion, count, minimum, maximum)
	}
	if scenario == "prior_at_n_minus_1" && expectedVersion >= 6 {
		if err := seedOverlapAccount(ctx, migrator); err != nil {
			return fmt.Errorf("seed predecessor account: %w", err)
		}
	}
	if expectedVersion < 6 {
		if err := guardedTenantRoundTrip(ctx, runtime); err != nil {
			return err
		}
		if scenario == "prior_at_n_minus_1" {
			if err := seedOverlapAccount(ctx, runtime); err != nil {
				return fmt.Errorf("seed predecessor account: %w", err)
			}
		}
	} else if err := mustDeny(ctx, runtime, "INSERT INTO public.families(id,display_name) VALUES ('92000000-0000-4000-8000-000000000001','escape')"); err != nil {
		return fmt.Errorf("runtime structural DML boundary: %w", err)
	}
	if expectedVersion >= 6 {
		if err := exerciseSchemaSixAccount(ctx, runtime); err != nil {
			return fmt.Errorf("schema-six authenticated account round trip: %w", err)
		}
	}
	if expectedVersion >= 9 {
		if err := exerciseSchemaNineRecords(ctx, migrator, runtime, scenario); err != nil {
			return fmt.Errorf("schema-nine record storage round trip: %w", err)
		}
	}
	if expectedVersion >= 10 {
		if err := verifySchemaTenSnapshotStaging(ctx, migrator, runtime, projector, scenario); err != nil {
			return fmt.Errorf("schema-ten snapshot staging boundary: %w", err)
		}
	}
	if expectedVersion >= 11 {
		if err := exerciseSchemaElevenTimerStorage(ctx, migrator, runtime, projector, scenario, expectedVersion); err != nil {
			return fmt.Errorf("schema-eleven timer storage boundary: %w", err)
		}
	}
	if expectedVersion >= 12 {
		if err := verifySchemaTwelveImportPredecessor(ctx, migrator, runtime, projector, scenario); err != nil {
			return fmt.Errorf("schema-twelve import predecessor boundary: %w", err)
		}
	}
	if expectedVersion >= 13 {
		if err := verifyDeliveryPredecessorUnused(ctx, migrator, runtime, projector, overlapAccountID,
			overlapScenarioUUID("family", scenario), expectedVersion); err != nil {
			return fmt.Errorf("schema-thirteen delivery predecessor boundary: %w", err)
		}
	}
	if err := mustDeny(ctx, runtime, "CREATE TABLE public.overlap_escape(id bigint)"); err != nil {
		return fmt.Errorf("runtime DDL boundary: %w", err)
	}
	if expectedVersion >= 3 {
		if err := mustDeny(ctx, projector, "INSERT INTO public.accounts(id,display_name) VALUES ('91000000-0000-4000-8000-000000000001','escape')"); err != nil {
			return fmt.Errorf("projector tenant boundary: %w", err)
		}
		if err := mustDeny(ctx, projector, "UPDATE public.security_projection_head SET revision=revision+1 WHERE singleton"); err != nil {
			return fmt.Errorf("projector direct-write boundary: %w", err)
		}
	}
	if expectedVersion >= 4 {
		if err := database.VerifyRoleSeparation(ctx, runtime, projector, expectedVersion); err != nil {
			return fmt.Errorf("role separation: %w", err)
		}
	}
	if expectedVersion >= 5 {
		if err := mustDeny(ctx, runtime, "SELECT * FROM public.auth_challenges LIMIT 0"); err != nil {
			return fmt.Errorf("runtime auth-table boundary: %w", err)
		}
		if err := mustDeny(ctx, projector, "SELECT * FROM public.sessions LIMIT 0"); err != nil {
			return fmt.Errorf("projector auth-table boundary: %w", err)
		}
		rows, err := runtime.Query(ctx, "SELECT * FROM app.lookup_access_session('session.v1',decode(repeat('00',32),'hex'))")
		if err != nil {
			return fmt.Errorf("schema-five runtime lookup: %w", err)
		}
		rows.Close()
	}
	return nil
}

func verifyDeliveryPredecessorUnused(ctx context.Context, migrator, runtime, projector *pgxpool.Pool, accountID, familyID string, expectedVersion int64) error {
	var schemaVersion int64
	if err := migrator.QueryRow(ctx, "SELECT COALESCE(max(version),0) FROM schema_migrations").Scan(&schemaVersion); err != nil || schemaVersion != expectedVersion {
		return fmt.Errorf("delivery predecessor schema version mismatch: got=%d want=%d: %w", schemaVersion, expectedVersion, err)
	}
	if expectedVersion < 13 {
		var absent bool
		if err := migrator.QueryRow(ctx, "SELECT to_regclass('public.delivery_pending') IS NULL").Scan(&absent); err != nil || !absent {
			return fmt.Errorf("pre-schema-thirteen unexpectedly exposes delivery_pending: absent=%t: %w", absent, err)
		}
		return nil
	}

	var forcedRLS, functionShape, privilegeShape bool
	if err := migrator.QueryRow(ctx, `SELECT c.relrowsecurity AND c.relforcerowsecurity
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND c.relname='delivery_pending' AND c.relkind='r'`).Scan(&forcedRLS); err != nil || !forcedRLS {
		return fmt.Errorf("delivery_pending forced RLS=%t: %w", forcedRLS, err)
	}
	if err := migrator.QueryRow(ctx, `SELECT p.prosecdef AND p.proconfig = ARRAY['search_path=pg_catalog']
		FROM pg_catalog.pg_proc p
		WHERE p.oid='app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)'::regprocedure`).Scan(&functionShape); err != nil || !functionShape {
		return fmt.Errorf("schedule_family_wakes security shape=%t: %w", functionShape, err)
	}
	if err := migrator.QueryRow(ctx, `SELECT
		has_function_privilege('baby_tracker_runtime','app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)','EXECUTE')
		AND NOT has_function_privilege('baby_tracker_projector','app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)','EXECUTE')
		AND NOT has_function_privilege('baby_tracker_apple_revoker','app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)','EXECUTE')
		AND NOT has_table_privilege('baby_tracker_runtime','public.delivery_pending','SELECT,INSERT,UPDATE,DELETE')
		AND NOT has_table_privilege('baby_tracker_projector','public.delivery_pending','SELECT,INSERT,UPDATE,DELETE')
		AND NOT has_table_privilege('baby_tracker_apple_revoker','public.delivery_pending','SELECT,INSERT,UPDATE,DELETE')`).Scan(&privilegeShape); err != nil || !privilegeShape {
		return fmt.Errorf("delivery predecessor privilege shape=%t: %w", privilegeShape, err)
	}

	visibility, err := migrator.Begin(ctx)
	if err != nil {
		return err
	}
	defer visibility.Rollback(ctx)
	if err := tenant.SetLocal(ctx, visibility, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		return err
	}
	var memberships, rows int64
	if err := visibility.QueryRow(ctx, `SELECT count(*) FROM public.memberships
		WHERE family_id=$1 AND account_id=$2 AND status='active'`, familyID, accountID).Scan(&memberships); err != nil || memberships != 1 {
		return fmt.Errorf("delivery predecessor tenant membership count=%d: %w", memberships, err)
	}
	if err := visibility.QueryRow(ctx, "SELECT count(*) FROM public.delivery_pending WHERE family_id=$1", familyID).Scan(&rows); err != nil || rows != 0 {
		return fmt.Errorf("compatibility predecessor used delivery_pending rows=%d: %w", rows, err)
	}
	if err := visibility.Rollback(ctx); err != nil {
		return err
	}
	if err := mustDeny(ctx, runtime, "SELECT count(*) FROM public.delivery_pending"); err != nil {
		return fmt.Errorf("runtime delivery table boundary: %w", err)
	}
	if err := mustDeny(ctx, projector, "SELECT count(*) FROM public.delivery_pending"); err != nil {
		return fmt.Errorf("projector delivery table boundary: %w", err)
	}
	return nil
}

func verifySchemaTwelveImportPredecessor(ctx context.Context, migrator, runtime, projector *pgxpool.Pool, scenario string) error {
	var exact bool
	if err := migrator.QueryRow(ctx, `SELECT count(*)=7 AND bool_and(c.relrowsecurity AND c.relforcerowsecurity)
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND c.relname = ANY($1)`, importPredecessorTables).Scan(&exact); err != nil || !exact {
		return fmt.Errorf("forced import RLS inventory exact=%t: %w", exact, err)
	}
	// Scope the empty-table compatibility probe to this scenario's tenant. Other
	// overlap phases intentionally leave durable imports that must survive N-1 -> N.
	visibility, err := runtime.Begin(ctx)
	if err != nil {
		return err
	}
	defer visibility.Rollback(ctx)
	if err := tenant.SetLocal(ctx, visibility, tenant.Context{
		AccountID: overlapAccountID,
		FamilyID:  overlapScenarioUUID("family", scenario),
	}); err != nil {
		return err
	}
	var rows int64
	if err := visibility.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM public.import_sessions) +
		(SELECT count(*) FROM public.import_chunks) +
		(SELECT count(*) FROM public.import_baby_candidates) +
		(SELECT count(*) FROM public.import_record_manifests) +
		(SELECT count(*) FROM public.import_conflicts) +
		(SELECT count(*) FROM public.import_conflict_receipts) +
		(SELECT count(*) FROM public.import_baby_mappings)`).Scan(&rows); err != nil || rows != 0 {
		return fmt.Errorf("compatibility predecessor used import tables rows=%d: %w", rows, err)
	}
	if err := visibility.Rollback(ctx); err != nil {
		return err
	}
	tx, err := runtime.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tenant.SetLocal(ctx, tx, tenant.Context{AccountID: overlapAccountID, FamilyID: overlapScenarioUUID("family", scenario)}); err != nil {
		return err
	}
	var unfinished int64
	if err := tx.QueryRow(ctx, `SELECT app.import_unfinished_session_count($1,$2)`, overlapAccountID,
		time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)).Scan(&unfinished); err != nil || unfinished != 0 {
		return fmt.Errorf("import unfinished helper count=%d: %w", unfinished, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		return err
	}
	if err := mustDeny(ctx, runtime, "DELETE FROM public.import_sessions"); err != nil {
		return fmt.Errorf("runtime import delete boundary: %w", err)
	}
	if err := mustDeny(ctx, runtime, "UPDATE public.import_chunks SET storage_bytes=storage_bytes"); err != nil {
		return fmt.Errorf("runtime immutable import boundary: %w", err)
	}
	if err := mustDeny(ctx, projector, "SELECT count(*) FROM public.import_sessions"); err != nil {
		return fmt.Errorf("projector import boundary: %w", err)
	}
	return nil
}

func verifySchemaTenSnapshotStaging(ctx context.Context, migrator, runtime, projector *pgxpool.Pool, scenario string) error {
	var exact bool
	if err := migrator.QueryRow(ctx, `SELECT count(*)=4 AND bool_and(c.relrowsecurity AND c.relforcerowsecurity)
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND c.relname = ANY($1)`, []string{
		"sync_page_sessions", "sync_snapshots", "sync_snapshot_entities", "sync_snapshot_admissions",
	}).Scan(&exact); err != nil || !exact {
		return fmt.Errorf("forced tenant RLS inventory exact=%t: %w", exact, err)
	}
	tx, err := runtime.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tenant.SetLocal(ctx, tx, tenant.Context{AccountID: overlapAccountID, FamilyID: overlapScenarioUUID("family", scenario)}); err != nil {
		return err
	}
	var accountActive, familyActive int64
	if err := tx.QueryRow(ctx, "SELECT account_active,family_active FROM app.sync_snapshot_admission_counts($1,app.current_family_id(),$2)", overlapAccountID,
		time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)).Scan(&accountActive, &familyActive); err != nil || accountActive < 0 || familyActive < 0 {
		return fmt.Errorf("snapshot admission counts account=%d family=%d: %w", accountActive, familyActive, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		return err
	}
	if err := mustDeny(ctx, runtime, "SELECT count(*) FROM public.sync_snapshot_admissions"); err != nil {
		return fmt.Errorf("runtime admission metadata boundary: %w", err)
	}
	if err := mustDeny(ctx, runtime, "UPDATE public.sync_snapshots SET encoded_bytes=encoded_bytes"); err != nil {
		return fmt.Errorf("runtime snapshot mutation boundary: %w", err)
	}
	if err := mustDeny(ctx, projector, "SELECT count(*) FROM public.sync_snapshots"); err != nil {
		return fmt.Errorf("projector snapshot boundary: %w", err)
	}
	return nil
}

func exerciseSchemaElevenTimerStorage(ctx context.Context, migrator, runtime, projector *pgxpool.Pool, scenario string, expectedVersion int64) error {
	// The timer's child rows: its finished sides until 000027 dropped that
	// table, its finished parts from then on.
	childTable := "timer_side_intervals"
	insertChild := `INSERT INTO timer_side_intervals(family_id,timer_id,position,side,seconds)
		VALUES($1,$2,0,'left',30)`
	if expectedVersion >= 27 {
		childTable = "timer_components"
		insertChild = `INSERT INTO timer_components(family_id,timer_id,component_id,position,kind,side,seconds)
		VALUES($1,$2,$2,0,'breast','left',30)`
	}
	var exact bool
	if err := migrator.QueryRow(ctx, `SELECT count(*)=2 AND bool_and(c.relrowsecurity AND c.relforcerowsecurity)
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND c.relname = ANY($1)`, []string{
		"active_timers", childTable,
	}).Scan(&exact); err != nil || !exact {
		return fmt.Errorf("forced tenant RLS inventory exact=%t: %w", exact, err)
	}
	familyID := overlapScenarioUUID("family", scenario)
	babyID := overlapScenarioUUID("baby", scenario)
	deviceID := overlapScenarioUUID("device", scenario)
	timerID := overlapScenarioUUID("timer", scenario)
	now := time.Date(2026, 9, 20, 12, 1, 0, 0, time.UTC)
	tx, err := runtime.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tenant.SetLocal(ctx, tx, tenant.Context{AccountID: overlapAccountID, FamilyID: familyID}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SELECT id FROM families WHERE id=$1 FOR UPDATE", familyID); err != nil {
		return fmt.Errorf("lock timer family: %w", err)
	}
	var sequence int64
	if err := tx.QueryRow(ctx, `UPDATE families SET next_sequence=next_sequence+1
		WHERE id=$1 AND next_sequence<9007199254740991 RETURNING next_sequence`, familyID).Scan(&sequence); err != nil || sequence != 2 {
		return fmt.Errorf("allocate timer cursor=%d: %w", sequence, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO active_timers(family_id,id,baby_id,entity_version,creator_account_id,
		last_editor_account_id,client_mutation_time,accepted_at,last_sequence,storage_bytes,timer_kind,started_at,
		time_zone_id,current_side,current_side_started_at)
		VALUES($1,$2,$3,1,$4,$4,$5,$5,$6,128,'feed',$5,'UTC','left',$5)`,
		familyID, timerID, babyID, overlapAccountID, now, sequence); err != nil {
		return fmt.Errorf("insert timer: %w", err)
	}
	if _, err := tx.Exec(ctx, insertChild, familyID, timerID); err != nil {
		return fmt.Errorf("insert timer interval: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,
		actor_account_id,device_id,operation,canonical_entity,accepted_at)
		VALUES($1,$2,'timer',$3,1,$4,$5,'timer_start','{}',$6)`,
		familyID, sequence, timerID, overlapAccountID, deviceID, now); err != nil {
		return fmt.Errorf("insert timer journal: %w", err)
	}
	var observedTimer, observedSide string
	if err := tx.QueryRow(ctx, `SELECT t.id::text,i.side FROM active_timers t
		JOIN `+childTable+` i ON i.family_id=t.family_id AND i.timer_id=t.id
		WHERE t.family_id=$1 AND t.id=$2`, familyID, timerID).Scan(&observedTimer, &observedSide); err != nil || observedTimer != timerID || observedSide != "left" {
		return fmt.Errorf("read timer id=%q side=%q: %w", observedTimer, observedSide, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if err := mustDeny(ctx, runtime, "DELETE FROM public.active_timers"); err != nil {
		return fmt.Errorf("runtime timer delete boundary: %w", err)
	}
	if err := mustDeny(ctx, projector, "SELECT count(*) FROM public.active_timers"); err != nil {
		return fmt.Errorf("projector timer boundary: %w", err)
	}
	return nil
}

func exerciseSchemaNineRecords(ctx context.Context, migrator, runtime *pgxpool.Pool, scenario string) error {
	familyID := overlapScenarioUUID("family", scenario)
	deviceID := overlapScenarioUUID("device", scenario)
	babyID := overlapScenarioUUID("baby", scenario)
	mutationID := overlapScenarioUUID("mutation", scenario)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	setup, err := migrator.Begin(ctx)
	if err != nil {
		return err
	}
	defer setup.Rollback(ctx)
	if err := tenant.SetLocal(ctx, setup, tenant.Context{AccountID: overlapAccountID, FamilyID: familyID}); err != nil {
		return err
	}
	if _, err := setup.Exec(ctx, "INSERT INTO families(id,display_name) VALUES($1,'Overlap records')", familyID); err != nil {
		return err
	}
	if _, err := setup.Exec(ctx, "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')", familyID, overlapAccountID); err != nil {
		return err
	}
	if _, err := setup.Exec(ctx, `INSERT INTO devices(id,account_id,server_instance_id,canonical_origin,
		continuity_key_id,continuity_digest,created_at,updated_at)
		VALUES($1,$2,'123e4567-e89b-42d3-a456-426614174000','https://overlap.family.example',
		'device.v1',decode(repeat('11',32),'hex'),$3,$3)`, deviceID, overlapAccountID, now); err != nil {
		return err
	}
	if err := setup.Commit(ctx); err != nil {
		return err
	}

	tx, err := runtime.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tenant.SetLocal(ctx, tx, tenant.Context{AccountID: overlapAccountID, FamilyID: familyID}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SELECT id FROM families WHERE id=$1 FOR UPDATE", familyID); err != nil {
		return fmt.Errorf("lock family: %w", err)
	}
	var sequence int64
	if err := tx.QueryRow(ctx, `UPDATE families SET next_sequence=next_sequence+1
		WHERE id=$1 AND next_sequence<9007199254740991 RETURNING next_sequence`, familyID).Scan(&sequence); err != nil || sequence != 1 {
		return fmt.Errorf("allocate cursor=%d: %w", sequence, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO family_record_usage(family_id,updated_at) VALUES($1,$2)`, familyID, now); err != nil {
		return fmt.Errorf("insert family usage: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE family_record_usage
		SET canonical_record_count=0,charged_storage_bytes=256,updated_at=$2 WHERE family_id=$1`, familyID, now); err != nil {
		return fmt.Errorf("update family usage: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO babies(family_id,id,entity_version,creator_account_id,last_editor_account_id,
		client_mutation_time,accepted_at,last_sequence,storage_bytes,name,normalized_name,normalization_version,birth_date,sort_order)
		VALUES($1,$2,1,$3,$3,$4,$4,$5,256,'Overlap baby','overlap baby','nfc-trim-fold-nfc-v1','2026-01-01',0)`,
		familyID, babyID, overlapAccountID, now, sequence); err != nil {
		return fmt.Errorf("insert baby: %w", err)
	}
	canonical := fmt.Sprintf(`{"entity_kind":"baby","meta":{"id":%q,"family_id":%q,"entity_version":1,"creator_account_id":%q,"last_editor_account_id":%q,"client_mutation_time":"2026-09-20T12:00:00.000000Z","accepted_at":"2026-09-20T12:00:00.000000Z","tombstoned_at":null},"payload":{"name":"Overlap baby","birth_date":"2026-01-01","sort_order":0}}`, babyID, familyID, overlapAccountID, overlapAccountID)
	if _, err := tx.Exec(ctx, `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,
		actor_account_id,device_id,operation,canonical_entity,accepted_at)
		VALUES($1,$2,'baby',$3,1,$4,$5,'create',$6::jsonb,$7)`,
		familyID, sequence, babyID, overlapAccountID, deviceID, canonical, now); err != nil {
		return fmt.Errorf("insert journal: %w", err)
	}
	var receiptResult string
	if err := tx.QueryRow(ctx, `SELECT app.save_mutation_receipt($1,$2,$3,$4,
		decode(repeat('22',32),'hex'),'{"status":"applied"}'::jsonb,$5,$6)`,
		overlapAccountID, familyID, deviceID, mutationID, now, sequence).Scan(&receiptResult); err != nil || receiptResult != "inserted" {
		return fmt.Errorf("save receipt=%q: %w", receiptResult, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	structural, err := runtime.Begin(ctx)
	if err != nil {
		return err
	}
	defer structural.Rollback(ctx)
	if err := tenant.SetLocal(ctx, structural, tenant.Context{AccountID: overlapAccountID, FamilyID: familyID}); err != nil {
		return err
	}
	if _, err := structural.Exec(ctx, "UPDATE families SET display_name='forbidden' WHERE id=$1", familyID); err == nil {
		return errors.New("runtime structural family update unexpectedly succeeded")
	}
	return nil
}

func overlapScenarioUUID(domain, scenario string) string {
	encoded := fmt.Sprintf("%x", sha256.Sum256([]byte(domain+"\x00"+scenario)))
	return encoded[:8] + "-" + encoded[8:12] + "-4" + encoded[13:16] + "-8" + encoded[17:20] + "-" + encoded[20:32]
}

func seedOverlapAccount(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tenant.SetAccountLocal(ctx, tx, overlapAccountID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO accounts(id,display_name) VALUES ($1,'Before overlap')", overlapAccountID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func exerciseSchemaSixAccount(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tenant.SetAccountLocal(ctx, tx, overlapAccountID); err != nil {
		return err
	}
	var accountID, displayName string
	if err := tx.QueryRow(ctx, "SELECT account_id::text,display_name FROM app.current_account_profile($1)", overlapAccountID).Scan(&accountID, &displayName); err != nil {
		return fmt.Errorf("read account profile: %w", err)
	}
	if accountID != overlapAccountID {
		return fmt.Errorf("account profile mismatch: %s", accountID)
	}
	if _, err := tx.Exec(ctx, "SELECT app.update_own_profile($1,$2,$3,$4)", overlapAccountID, overlapProfileCommandID,
		"Overlap caregiver", time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)); err != nil {
		return fmt.Errorf("write account profile: %w", err)
	}
	if err := tx.QueryRow(ctx, "SELECT display_name FROM app.current_account_profile($1)", overlapAccountID).Scan(&displayName); err != nil {
		return fmt.Errorf("read updated account profile: %w", err)
	}
	if displayName != "Overlap caregiver" {
		return fmt.Errorf("updated profile mismatch: %q", displayName)
	}
	return tx.Commit(ctx)
}

func guardedTenantRoundTrip(ctx context.Context, pool *pgxpool.Pool) error {
	const accountID = "91000000-0000-4000-8000-000000000001"
	const familyID = "92000000-0000-4000-8000-000000000001"
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tenant.SetLocal(ctx, tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO accounts(id,display_name) VALUES ($1,'overlap')", accountID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO families(id,display_name) VALUES ($1,'overlap')", familyID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO memberships(family_id,account_id,role) VALUES ($1,$2,'owner')", familyID, accountID); err != nil {
		return err
	}
	var observed string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM accounts WHERE id=$1", accountID).Scan(&observed); err != nil || observed != accountID {
		return errors.New("guarded tenant read did not observe its write")
	}
	return tx.Rollback(ctx)
}

func mustDeny(ctx context.Context, pool *pgxpool.Pool, statement string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, statement); err == nil {
		return errors.New("operation unexpectedly succeeded")
	}
	return nil
}
