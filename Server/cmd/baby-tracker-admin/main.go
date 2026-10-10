package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/migrate"
	"babytracker/server/internal/operator"
	"babytracker/server/migrations"
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

func runWithOperatorLoader(ctx context.Context, arguments []string, getenv func(string) string, output io.Writer, load operatorLoader) error {
	if len(arguments) > 0 && (arguments[0] == "ownership-recover" || arguments[0] == "entitlement" || arguments[0] == "mcp") {
		commands, closeCommands, err := load(ctx, getenv, arguments)
		if err != nil {
			return err
		}
		defer closeCommands()
		return RunWithServices(ctx, arguments, commands, output)
	}
	if len(arguments) == 1 && arguments[0] == "capabilities" {
		fmt.Fprintln(output, "postgres-authority-v1")
		return nil
	}
	if len(arguments) == 1 && arguments[0] == "migrate" {
		return runMigrate(ctx, getenv)
	}
	if len(arguments) >= 2 && arguments[0] == "authority" {
		return runAuthority(ctx, arguments[1:], getenv, output)
	}
	return errors.New("usage: baby-tracker-admin capabilities|migrate|authority init|authority verify|authority rotate-epoch|authority family-revision|ownership-recover|entitlement|mcp")
}

func loadOperatorCommands(ctx context.Context, getenv func(string) string, arguments []string) (OperatorCommands, func(), error) {
	operatorConfig, err := config.LoadOperator(getenv)
	if err != nil {
		return nil, nil, err
	}
	requestedPolicyMissing := len(arguments) == 0 || (arguments[0] == "ownership-recover" && operatorConfig.OwnershipPolicyID == "") ||
		(arguments[0] == "entitlement" && operatorConfig.EntitlementPolicyID == "") || (arguments[0] == "mcp" && operatorConfig.IntegrationPolicyID == "")
	if requestedPolicyMissing {
		service, err := operator.New(operator.Options{Now: time.Now})
		return service, func() {}, err
	}
	// Hosted mode composes no operator authority (its server needs an Apple
	// code exchanger that exists only in identitytest builds).
	mode, err := config.LoadMode(getenv)
	if err != nil {
		return nil, nil, err
	}
	if mode == config.ModeHosted {
		return nil, nil, errors.New("hosted_adapter_unavailable")
	}
	runtime, err := openOperatorRuntime(ctx, getenv)
	if err != nil {
		return nil, nil, err
	}
	service, err := operator.New(operator.Options{Authority: runtime.adapter, IntegrationAuthority: runtime.adapter, OperatorID: operatorConfig.OperatorID,
		OwnershipPolicyID: operatorConfig.OwnershipPolicyID, EntitlementPolicyID: operatorConfig.EntitlementPolicyID,
		IntegrationPolicyID: operatorConfig.IntegrationPolicyID, ConfirmationKey: operatorConfig.ConfirmationKey, Now: time.Now})
	if err != nil {
		runtime.close()
		return nil, nil, err
	}
	// The node must be ready before fresh operator work is composed; an
	// exact retry's receipt probe is answered by the adapter ungated.
	if err := runtime.readiness.Check(ctx); err != nil {
		runtime.close()
		return nil, nil, err
	}
	return service, runtime.close, nil
}

func runMigrate(ctx context.Context, getenv func(string) string) error {
	// A migrate service written for the file ledger (ledger directories,
	// projector credential, hosted provider) is refused like the server.
	if err := config.RejectRetiredSecuritySettings(getenv); err != nil {
		return err
	}
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
