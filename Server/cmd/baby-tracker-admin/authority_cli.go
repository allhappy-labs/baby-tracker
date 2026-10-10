package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/family"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/migrate"
	"babytracker/server/internal/operator"
	"babytracker/server/internal/protocol"
	"babytracker/server/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const authorityUsage = "usage: baby-tracker-admin authority init --instance-id <uuid> --epoch 1 --confirm-instance <uuid> | " +
	"authority verify | authority rotate-epoch --expected-epoch <n> --reason-code database_restore --confirm-instance <uuid> | " +
	"authority family-revision --family-id <uuid>"

// runAuthority runs the `authority` commands. init, verify and
// rotate-epoch run as the migrator (BT_DATABASE_URL[_FILE] of the migrate
// service): the runtime role may only read server_state. family-revision
// runs with the server's runtime configuration, like the operator
// commands, behind the same readiness gate.
func runAuthority(ctx context.Context, arguments []string, getenv func(string) string, output io.Writer) error {
	// Settings of the retired file ledger, projector or hosted provider mean
	// the environment was written for the old runtime: refuse it, naming
	// each setting, as the server does (family-revision loads the full
	// configuration, which refuses them too).
	if err := config.RejectRetiredSecuritySettings(getenv); err != nil {
		return err
	}
	switch arguments[0] {
	case "init":
		flags := flag.NewFlagSet("authority init", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		instanceID := flags.String("instance-id", "", "server instance ID")
		epoch := flags.Uint64("epoch", 0, "initial recovery epoch")
		confirmation := flags.String("confirm-instance", "", "confirmed server instance ID")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 || *epoch != 1 || *instanceID == "" || *confirmation != *instanceID {
			return errors.New("authority init requires --instance-id, --epoch 1, and matching --confirm-instance")
		}
		if !protocol.ValidRecoveryEpoch(*epoch) || !canonicalUUID(*instanceID) {
			return errors.New("invalid server identity")
		}
		actorKey, err := config.ActorIDPepper(getenv)
		if err != nil {
			return err
		}
		pool, err := openMigratorPool(ctx, getenv)
		if err != nil {
			return err
		}
		defer pool.Close()
		result, err := authority.InitServerState(ctx, pool, *instanceID, actorKey)
		if err != nil {
			return err
		}
		fmt.Fprintf(output, "instance_id=%s epoch=1 result=%s\n", *instanceID, result)
		return nil
	case "verify":
		if len(arguments) != 1 {
			return errors.New("authority verify takes no flags")
		}
		actorKey, err := config.ActorIDPepper(getenv)
		if err != nil {
			return err
		}
		pool, err := openMigratorPool(ctx, getenv)
		if err != nil {
			return err
		}
		defer pool.Close()
		return verifyAuthority(ctx, pool, getenv, actorKey, output)
	case "rotate-epoch":
		flags := flag.NewFlagSet("authority rotate-epoch", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		expectedEpoch := flags.Uint64("expected-epoch", 0, "expected current recovery epoch")
		reason := flags.String("reason-code", "", "rotation reason")
		confirmation := flags.String("confirm-instance", "", "confirmed server instance ID")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 || !protocol.ValidRecoveryEpoch(*expectedEpoch) ||
			*expectedEpoch >= protocol.MaxSafeInteger || *reason != "database_restore" || !canonicalUUID(*confirmation) {
			return errors.New("authority rotate-epoch requires --expected-epoch, --reason-code database_restore, and --confirm-instance")
		}
		pool, err := openMigratorPool(ctx, getenv)
		if err != nil {
			return err
		}
		defer pool.Close()
		rotated, err := authority.RotateRecoveryEpoch(ctx, pool, *confirmation, *expectedEpoch)
		if err != nil {
			return err
		}
		fmt.Fprintf(output, "instance_id=%s epoch=%d result=rotated\n", rotated.InstanceID, rotated.RecoveryEpoch)
		return nil
	case "family-revision":
		flags := flag.NewFlagSet("authority family-revision", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		familyID := flags.String("family-id", "", "family ID")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 || !canonicalUUID(*familyID) {
			return errors.New("authority family-revision requires --family-id")
		}
		runtime, err := openOperatorRuntime(ctx, getenv)
		if err != nil {
			return err
		}
		defer runtime.close()
		revision, err := runtime.adapter.FamilyRevision(ctx, *familyID)
		if err != nil {
			return err
		}
		fmt.Fprintf(output, "family_id=%s revision=%d\n", *familyID, revision)
		return nil
	default:
		return errors.New(authorityUsage)
	}
}

// verifyAuthority checks server_state against the configured actor-ID key
// (and, when set, BT_SERVER_INSTANCE_ID and BT_RECOVERY_EPOCH), then walks
// every audit chain from genesis. Broken chains are failed closed and
// printed; any failed chain or unverifiable chain is an error (exit 1).
func verifyAuthority(ctx context.Context, pool *pgxpool.Pool, getenv func(string) string, actorKey [32]byte, output io.Writer) error {
	state, err := authority.LoadServerState(ctx, pool)
	if err != nil {
		return err
	}
	instanceID, epoch := state.InstanceID, state.RecoveryEpoch
	if configured := getenv("BT_SERVER_INSTANCE_ID"); configured != "" {
		instanceID = configured
	}
	if configured := getenv("BT_RECOVERY_EPOCH"); configured != "" {
		parsed, err := strconv.ParseUint(configured, 10, 64)
		if err != nil {
			return errors.New("BT_RECOVERY_EPOCH is invalid")
		}
		epoch = parsed
	}
	if err := state.Check(instanceID, epoch, actorKey); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	verifier, err := authority.NewAuditVerifier(pool, time.Now, logger)
	if err != nil {
		return err
	}
	report, verifyErr := verifier.VerifyAll(ctx)
	for _, failed := range report.Failed {
		fmt.Fprintf(output, "failed scope=%s id=%s\n", failed.Scope, failed.ID)
	}
	result := "verified"
	if len(report.Failed) > 0 || verifyErr != nil {
		result = "failed"
	}
	fmt.Fprintf(output, "instance_id=%s epoch=%d scopes=%d rows=%d failed=%d result=%s\n",
		state.InstanceID, state.RecoveryEpoch, report.Scopes, report.Rows, len(report.Failed), result)
	if verifyErr != nil {
		return fmt.Errorf("authority verification incomplete: %w", verifyErr)
	}
	if len(report.Failed) > 0 {
		return fmt.Errorf("authority verification failed: %d scopes failed closed", len(report.Failed))
	}
	return nil
}

// openMigratorPool opens BT_DATABASE_URL[_FILE] and requires a compatible
// (fully migrated) schema.
func openMigratorPool(ctx context.Context, getenv func(string) string) (*pgxpool.Pool, error) {
	databaseURL, err := config.DatabaseURL(getenv)
	if err != nil {
		return nil, err
	}
	pool, err := database.OpenAndPing(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		pool.Close()
		return nil, err
	}
	if _, err := migrate.New(migrations.Files).CompatibleVersion(ctx, pool, compatibility); err != nil {
		pool.Close()
		return nil, errors.New("database compatibility check failed; run baby-tracker-admin migrate first")
	}
	return pool, nil
}

// adminReadiness is the node readiness the operator commands require
// (the server's, minus its connection budget and cached key inventory):
// a compatible schema, the runtime login's exact manifest privileges, and
// server_state equal to the configured instance, epoch and actor-ID key.
type adminReadiness struct {
	pool          *pgxpool.Pool
	runner        *migrate.Runner
	compatibility migrate.Compatibility
	instanceID    string
	epoch         uint64
	actorKey      [32]byte
}

func (r adminReadiness) Check(ctx context.Context) error {
	version, err := r.runner.CompatibleVersion(ctx, r.pool, r.compatibility)
	if err != nil {
		return errors.New("database compatibility check failed")
	}
	if err := database.VerifyRuntimeRole(ctx, r.pool, version); err != nil {
		return errors.New("database runtime role check failed")
	}
	state, err := authority.LoadServerState(ctx, r.pool)
	if err != nil {
		return err
	}
	if err := state.Check(r.instanceID, r.epoch, r.actorKey); err != nil {
		return err
	}
	confirmed, err := r.runner.CompatibleVersion(ctx, r.pool, r.compatibility)
	if err != nil || confirmed != version {
		return errors.New("database compatibility changed during role validation")
	}
	return nil
}

type operatorRuntime struct {
	adapter   *operator.SecurityAuthority
	readiness adminReadiness
	close     func()
}

// openOperatorRuntime composes the authority exactly as the server does
// (same runtime credential, instance, epoch, token keyring, family limits
// and entitlement policy) behind the admin readiness gate.
func openOperatorRuntime(ctx context.Context, getenv func(string) string) (operatorRuntime, error) {
	runtimeConfig, err := config.Load(getenv)
	if err != nil {
		return operatorRuntime{}, err
	}
	pool, err := database.OpenAndPing(ctx, runtimeConfig.DatabaseURL)
	if err != nil {
		return operatorRuntime{}, fmt.Errorf("open runtime database: %w", err)
	}
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		pool.Close()
		return operatorRuntime{}, err
	}
	service, err := authority.New(authority.Options{
		Pool: pool, Now: time.Now, InstanceID: runtimeConfig.ServerInstanceID, RecoveryEpoch: runtimeConfig.RecoveryEpoch,
		TokenKeys:     adminKeyring(runtimeConfig.Auth.SessionTokenKeys),
		MaxCaregivers: runtimeConfig.Limits.MaxCaregivers, MaxInvitations: runtimeConfig.Limits.MaxInvitations,
		RequireFamilySyncEntitlement: runtimeConfig.Mode == config.ModeHosted,
		Allowed:                      family.Allowed, EntitlementExempt: authority.LegacyEntitlementExempt,
	})
	if err != nil {
		pool.Close()
		return operatorRuntime{}, err
	}
	readiness := adminReadiness{pool: pool, runner: migrate.New(migrations.Files), compatibility: compatibility,
		instanceID: runtimeConfig.ServerInstanceID, epoch: runtimeConfig.RecoveryEpoch, actorKey: runtimeConfig.Auth.ActorIDPepper}
	adapter, err := operator.NewSecurityAuthority(service, readiness)
	if err != nil {
		pool.Close()
		return operatorRuntime{}, err
	}
	return operatorRuntime{adapter: adapter, readiness: readiness, close: pool.Close}, nil
}

func adminKeyring(source config.DigestKeyringConfig) identity.DigestKeyring {
	result := identity.DigestKeyring{Current: identity.DigestKey{ID: source.Current.ID, Secret: source.Current.Secret}}
	result.Previous = make([]identity.DigestKey, len(source.Previous))
	for index, key := range source.Previous {
		result.Previous[index] = identity.DigestKey{ID: key.ID, Secret: key.Secret}
	}
	return result
}

func canonicalUUID(value string) bool {
	if len(value) != 36 || value == "00000000-0000-0000-0000-000000000000" {
		return false
	}
	for index, character := range value {
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return false
			}
		default:
			if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
				return false
			}
		}
	}
	return true
}
