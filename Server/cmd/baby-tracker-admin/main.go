package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/migrate"
	"babytracker/server/internal/operator"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/recovery"
	"babytracker/server/internal/security"
	"babytracker/server/internal/security/localstore"
	"babytracker/server/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, getenv func(string) string, output io.Writer) error {
	return runWithOperatorLoader(ctx, arguments, getenv, output, loadOperatorCommands)
}

type operatorLoader func(context.Context, func(string) string, []string) (OperatorCommands, func(), error)

type operatorCommandGate interface {
	Reconcile(context.Context) error
	Check(context.Context) error
}

func activateOperatorCommands(ctx context.Context, gate operatorCommandGate, commands OperatorCommands) (OperatorCommands, error) {
	if gate == nil || commands == nil {
		return nil, errors.New("operator recovery gate and commands are required")
	}
	if err := gate.Reconcile(ctx); err != nil {
		return nil, err
	}
	return commands, nil
}

func runWithOperatorLoader(ctx context.Context, arguments []string, getenv func(string) string, output io.Writer, load operatorLoader) error {
	if len(arguments) > 0 && (arguments[0] == "ownership-recover" || arguments[0] == "entitlement") {
		commands, closeCommands, err := load(ctx, getenv, arguments)
		if err != nil {
			return err
		}
		defer closeCommands()
		return RunWithServices(ctx, arguments, commands, output)
	}
	if len(arguments) == 1 && arguments[0] == "capabilities" {
		fmt.Fprintln(output, "security-authority-v1")
		return nil
	}
	if len(arguments) == 1 && arguments[0] == "migrate" {
		return runMigrate(ctx, getenv)
	}
	if len(arguments) < 2 {
		return errors.New("usage: baby-tracker-admin capabilities|migrate|security init|security bind-actor-key|security verify|security reconcile|recovery rotate-epoch")
	}
	switch arguments[0] + " " + arguments[1] {
	case "security init":
		flags := flag.NewFlagSet("security init", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		instanceID := flags.String("instance-id", "", "server instance ID")
		epoch := flags.Uint64("epoch", 0, "initial recovery epoch")
		confirmation := flags.String("confirm-instance", "", "confirmed server instance ID")
		if err := flags.Parse(arguments[2:]); err != nil || flags.NArg() != 0 || *epoch != 1 || *instanceID == "" || *confirmation != *instanceID {
			return errors.New("security init requires --instance-id, --epoch 1, and matching --confirm-instance")
		}
		proposed := recovery.State{InstanceID: *instanceID, Epoch: *epoch, TrustedHead: security.Head{Generation: 1}}
		if err := recovery.ValidateState(proposed); err != nil {
			return errors.New("invalid recovery identity")
		}
		actorKey, err := recovery.LoadActorIDPepperFile(getenv("BT_ACTOR_ID_PEPPER_FILE"))
		if err != nil {
			return err
		}
		store, durable, err := openStores(getenv)
		if err != nil {
			return err
		}
		if err := durable.InitializeAuthorityWithActorIDKey(ctx, store, proposed, actorKey); err != nil {
			return err
		}
		fmt.Fprintf(output, "instance_id=%s epoch=1 revision=0 result=initialized\n", *instanceID)
		return nil
	case "security bind-actor-key":
		flags := flag.NewFlagSet("security bind-actor-key", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		instanceID := flags.String("instance-id", "", "server instance ID")
		confirmation := flags.String("confirm-instance", "", "confirmed server instance ID")
		if err := flags.Parse(arguments[2:]); err != nil || flags.NArg() != 0 || *instanceID == "" || *confirmation != *instanceID {
			return errors.New("security bind-actor-key requires --instance-id and matching --confirm-instance")
		}
		proposed := recovery.State{InstanceID: *instanceID, Epoch: 1, TrustedHead: security.Head{Generation: 1}}
		if err := recovery.ValidateState(proposed); err != nil {
			return errors.New("invalid recovery identity")
		}
		actorKey, err := recovery.LoadActorIDPepperFile(getenv("BT_ACTOR_ID_PEPPER_FILE"))
		if err != nil {
			return err
		}
		_, durable, err := openStores(getenv)
		if err != nil {
			return err
		}
		if err := durable.BindActorIDKey(ctx, *instanceID, *confirmation, actorKey); err != nil {
			return err
		}
		fmt.Fprintf(output, "instance_id=%s result=bound\n", *instanceID)
		return nil
	case "security verify":
		actorKey, err := recovery.LoadActorIDPepperFile(getenv("BT_ACTOR_ID_PEPPER_FILE"))
		if err != nil {
			return err
		}
		_, durable, err := openStores(getenv)
		if err != nil {
			return err
		}
		state, head, err := durable.VerifyAuthorityWithActorIDKey(ctx, actorKey)
		if err != nil {
			if errors.Is(err, recovery.ErrAuthorityChainMismatch) {
				return errors.New("trusted recovery manifest mismatch")
			}
			return err
		}
		fmt.Fprintf(output, "instance_id=%s epoch=%d revision=%d result=verified\n", state.InstanceID, state.Epoch, head.Revision)
		return nil
	case "security reconcile":
		return runReconcile(ctx, getenv, output)
	case "recovery rotate-epoch":
		flags := flag.NewFlagSet("recovery rotate-epoch", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		expectedEpoch := flags.Uint64("expected-epoch", 0, "expected durable epoch")
		expectedRevision := flags.Uint64("expected-revision", ^uint64(0), "expected authority revision")
		reason := flags.String("reason-code", "", "rotation reason")
		confirmation := flags.String("confirm-instance", "", "confirmed server instance ID")
		if err := flags.Parse(arguments[2:]); err != nil || flags.NArg() != 0 || !protocol.ValidRecoveryEpoch(*expectedEpoch) ||
			*expectedRevision == ^uint64(0) || *expectedRevision > protocol.MaxSafeInteger || *reason != "database_restore" || *confirmation == "" {
			return errors.New("recovery rotate-epoch requires expected epoch/revision, database_restore reason, and instance confirmation")
		}
		store, durable, err := openStores(getenv)
		if err != nil {
			return err
		}
		state, err := durable.Load(ctx)
		if err != nil {
			return err
		}
		if state.InstanceID != *confirmation {
			return errors.New("instance confirmation mismatch")
		}
		head, _, err := security.VerifyCommittedChain(ctx, store.Ledger, store.Heads, state.TrustedHead)
		if err != nil || head.Revision != *expectedRevision {
			return errors.New("authority revision conflict")
		}
		rotated, err := durable.Rotate(ctx, *expectedEpoch, head)
		if err != nil {
			return err
		}
		fmt.Fprintf(output, "instance_id=%s epoch=%d revision=%d result=rotated\n", rotated.InstanceID, rotated.Epoch, head.Revision)
		return nil
	default:
		return errors.New("unknown admin command")
	}
}

func loadOperatorCommands(ctx context.Context, getenv func(string) string, arguments []string) (OperatorCommands, func(), error) {
	operatorConfig, err := config.LoadOperator(getenv)
	if err != nil {
		return nil, nil, err
	}
	requestedPolicyMissing := len(arguments) == 0 || (arguments[0] == "ownership-recover" && operatorConfig.OwnershipPolicyID == "") ||
		(arguments[0] == "entitlement" && operatorConfig.EntitlementPolicyID == "")
	if requestedPolicyMissing {
		service, err := operator.New(operator.Options{Now: time.Now})
		return service, func() {}, err
	}
	mode, err := config.LoadMode(getenv)
	if err != nil {
		return nil, nil, err
	}
	if mode == config.ModeHosted {
		return nil, nil, errors.New("hosted_adapter_unavailable")
	}
	runtimeConfig, err := config.Load(getenv)
	if err != nil {
		return nil, nil, err
	}
	store, durable, err := openConfiguredStores(runtimeConfig.SecurityLedgerDir, runtimeConfig.LedgerHeadDir)
	if err != nil {
		return nil, nil, err
	}
	runtimePool, err := database.OpenAndPing(ctx, runtimeConfig.DatabaseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("open runtime database: %w", err)
	}
	projectorPool, err := database.OpenAndPing(ctx, runtimeConfig.ProjectorDatabaseURL)
	if err != nil {
		runtimePool.Close()
		return nil, nil, fmt.Errorf("open projector database: %w", err)
	}
	closePools := func() {
		projectorPool.Close()
		runtimePool.Close()
	}
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		closePools()
		return nil, nil, err
	}
	runner := migrate.New(migrations.Files)
	version, err := runner.CompatibleVersion(ctx, runtimePool, compatibility)
	if err != nil {
		closePools()
		return nil, nil, errors.New("database compatibility check failed")
	}
	if err := database.VerifyRoleSeparation(ctx, runtimePool, projectorPool, version); err != nil {
		closePools()
		return nil, nil, errors.New("database role separation check failed")
	}
	confirmedVersion, err := runner.CompatibleVersion(ctx, runtimePool, compatibility)
	if err != nil || confirmedVersion != version {
		closePools()
		return nil, nil, errors.New("database compatibility changed during role validation")
	}
	authority, err := security.New(security.Options{Ledger: store.Ledger, Heads: store.Heads, Projection: security.NewProjector(projectorPool), Fence: store.Fence,
		Now: time.Now, Runtime: runtimePool, Decide: func(context.Context, access.Principal, string, string) (access.Grant, error) {
			return access.Grant{}, security.ErrUnavailable
		}, Epoch: func(ctx context.Context) (uint64, error) {
			current, err := durable.LoadUnderFence(ctx)
			return current.Epoch, err
		}, MaxCaregivers: runtimeConfig.Limits.MaxCaregivers, MaxInvitations: runtimeConfig.Limits.MaxInvitations,
		RequireFamilySyncEntitlement: runtimeConfig.Mode == config.ModeHosted})
	if err != nil {
		closePools()
		return nil, nil, err
	}
	mirror, err := recovery.NewPostgresMirror(runtimePool, projectorPool)
	if err != nil {
		closePools()
		return nil, nil, err
	}
	gate, err := recovery.NewGate(recovery.Options{Store: durable, Security: authority,
		Schema: compatibilitySchema{runner, runtimePool, compatibility}, Mirror: mirror,
		ExpectedInstanceID: runtimeConfig.ServerInstanceID, ExpectedEpoch: runtimeConfig.RecoveryEpoch, ActorIDKey: runtimeConfig.Auth.ActorIDPepper})
	if err != nil {
		closePools()
		return nil, nil, err
	}
	adapter, err := operator.NewSecurityAuthority(authority, gate)
	if err != nil {
		closePools()
		return nil, nil, err
	}
	service, err := operator.New(operator.Options{Authority: adapter, OperatorID: operatorConfig.OperatorID,
		OwnershipPolicyID: operatorConfig.OwnershipPolicyID, EntitlementPolicyID: operatorConfig.EntitlementPolicyID,
		ConfirmationKey: operatorConfig.ConfirmationKey, Now: time.Now})
	if err != nil {
		closePools()
		return nil, nil, err
	}
	commands, err := activateOperatorCommands(ctx, gate, service)
	if err != nil {
		closePools()
		return nil, nil, err
	}
	return commands, closePools, nil
}

func runMigrate(ctx context.Context, getenv func(string) string) error {
	databaseURL, err := config.DatabaseURL(getenv)
	if err != nil {
		return err
	}
	pool, err := database.OpenAndPing(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()
	if err := migrate.New(migrations.Files).Apply(ctx, pool); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

func openStores(getenv func(string) string) (localstore.Store, *recovery.Store, error) {
	return openConfiguredStores(getenv("BT_SECURITY_LEDGER_DIR"), getenv("BT_LEDGER_HEAD_DIR"))
}

func openConfiguredStores(ledgerDir, headDir string) (localstore.Store, *recovery.Store, error) {
	if !filepath.IsAbs(ledgerDir) || !filepath.IsAbs(headDir) || ledgerDir == headDir {
		return localstore.Store{}, nil, errors.New("security ledger and head directories must be distinct absolute paths")
	}
	store, err := localstore.Open(ledgerDir, headDir)
	if err != nil {
		return localstore.Store{}, nil, err
	}
	durable, err := recovery.NewVerifiedStore(headDir, store.Fence, store.Ledger, store.Heads)
	return store, durable, err
}

type compatibilitySchema struct {
	runner        *migrate.Runner
	pool          *pgxpool.Pool
	compatibility migrate.Compatibility
}

func (schema compatibilitySchema) Check(ctx context.Context) error {
	return schema.runner.CheckCompatibility(ctx, schema.pool, schema.compatibility)
}

func runReconcile(ctx context.Context, getenv func(string) string, output io.Writer) error {
	actorKey, err := recovery.LoadActorIDPepperFile(getenv("BT_ACTOR_ID_PEPPER_FILE"))
	if err != nil {
		return err
	}
	store, durable, err := openStores(getenv)
	if err != nil {
		return err
	}
	state, err := durable.Load(ctx)
	if err != nil {
		return err
	}
	runtimeURL, err := config.DatabaseURL(getenv)
	if err != nil {
		return err
	}
	projectorURL, err := config.ProjectorDatabaseURL(getenv)
	if err != nil {
		return err
	}
	if runtimeURL == projectorURL {
		return errors.New("runtime and projector database credentials must differ")
	}
	runtimePool, err := database.OpenAndPing(ctx, runtimeURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer runtimePool.Close()
	projectorPool, err := database.OpenAndPing(ctx, projectorURL)
	if err != nil {
		return fmt.Errorf("open projector database: %w", err)
	}
	defer projectorPool.Close()
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		return err
	}
	runner := migrate.New(migrations.Files)
	version, err := runner.CompatibleVersion(ctx, runtimePool, compatibility)
	if err != nil {
		return errors.New("database compatibility check failed")
	}
	if err := database.VerifyRoleSeparation(ctx, runtimePool, projectorPool, version); err != nil {
		return errors.New("database role separation check failed")
	}
	confirmedVersion, err := runner.CompatibleVersion(ctx, runtimePool, compatibility)
	if err != nil || confirmedVersion != version {
		return errors.New("database compatibility changed during role validation")
	}
	authority, err := security.New(security.Options{Ledger: store.Ledger, Heads: store.Heads, Projection: security.NewProjector(projectorPool), Fence: store.Fence, Now: time.Now,
		Runtime: runtimePool,
		Decide: func(context.Context, access.Principal, string, string) (access.Grant, error) {
			return access.Grant{}, security.ErrUnavailable
		},
		Epoch: func(ctx context.Context) (uint64, error) {
			current, err := durable.LoadUnderFence(ctx)
			return current.Epoch, err
		}})
	if err != nil {
		return err
	}
	mirror, err := recovery.NewPostgresMirror(runtimePool, projectorPool)
	if err != nil {
		return err
	}
	gate, err := recovery.NewGate(recovery.Options{Store: durable, Security: authority, Schema: compatibilitySchema{runner, runtimePool, compatibility},
		Mirror: mirror, ExpectedInstanceID: state.InstanceID, ExpectedEpoch: state.Epoch, ActorIDKey: actorKey})
	if err != nil {
		return err
	}
	if err := gate.Reconcile(ctx); err != nil {
		return err
	}
	verified, err := durable.Load(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "instance_id=%s epoch=%d revision=%d result=reconciled\n", verified.InstanceID, verified.Epoch, verified.TrustedHead.Revision)
	return nil
}
