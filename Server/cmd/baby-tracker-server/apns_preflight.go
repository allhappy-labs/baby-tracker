package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/config"
)

// apns-preflight checks the operator's Apple push credentials before push is
// turned on (main's live-activity-preflight, narrowed to Apple): it loads
// BT_APNS_* exactly as startup does, sends one push of the chosen type to an
// all-zero device token in the chosen environment and prints one line,
//
//	apns_preflight environment=<env> push_type=<type> outcome=<outcome> status=<n> reason=<reason>
//
// It exits 0 only for outcome=pass, and never prints a token, the key, the
// key ID or the provider token.

const (
	apnsPreflightUsage    = "usage: baby-tracker-server apns-preflight --environment sandbox|production [--push-type background|liveactivity]"
	apnsPreflightDeadline = 30 * time.Second
)

var errAPNsPreflightUsage = errors.New(apnsPreflightUsage)

// apnsPreflighter is the APNs client as the preflight uses it (*apns.Client).
type apnsPreflighter interface {
	Preflight(ctx context.Context, env apns.Environment, pushType string) (apns.PreflightResult, error)
	CloseIdleConnections()
}

// connectAPNsPreflight is newAPNSClient as an apnsPreflighter: an identitytest
// build preflights only against its recorder.
func connectAPNsPreflight(creds config.APNSCredentials) (apnsPreflighter, error) {
	client, err := newAPNSClient(creds)
	if err != nil {
		return nil, err // never a typed nil
	}
	return client, nil
}

func apnsPreflightCommand(arguments []string) {
	ctx, cancel := context.WithTimeout(context.Background(), apnsPreflightDeadline)
	err := runAPNsPreflight(ctx, arguments, os.Getenv, connectAPNsPreflight, os.Stdout)
	cancel()
	switch {
	case errors.Is(err, errAPNsPreflightUsage):
		fmt.Fprintln(os.Stderr, apnsPreflightUsage)
		os.Exit(2)
	case err != nil:
		fmt.Fprintln(os.Stderr, "apns-preflight: "+err.Error())
		os.Exit(1)
	}
}

// runAPNsPreflight returns nil only for a pass. Configuration errors name the
// variable, never its value; nothing is printed to out unless Apple was
// asked.
func runAPNsPreflight(ctx context.Context, arguments []string, getenv func(string) string,
	connect func(config.APNSCredentials) (apnsPreflighter, error), out io.Writer) error {
	flags := flag.NewFlagSet("apns-preflight", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	environment := flags.String("environment", "", "sandbox (development-signed builds) or production (TestFlight and App Store builds)")
	pushType := flags.String("push-type", apns.PushBackground, "background (wakes) or liveactivity (the <bundle>.push-type.liveactivity topic)")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return errAPNsPreflightUsage
	}
	env := apns.Environment(*environment)
	if env != apns.Sandbox && env != apns.Production {
		return errAPNsPreflightUsage
	}
	if *pushType != apns.PushBackground && *pushType != apns.PushLiveActivity {
		return errAPNsPreflightUsage
	}
	credentials, err := config.LoadAPNSCredentials(getenv)
	if err != nil {
		return err
	}
	client, err := connect(credentials)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	result, err := client.Preflight(ctx, env, *pushType)
	if err != nil {
		// A request that could not be built (the provider token did not
		// sign): a fixed message, never the underlying error's text.
		return errors.New("the preflight request could not be built")
	}
	fmt.Fprintf(out, "apns_preflight environment=%s push_type=%s outcome=%s status=%d reason=%s\n", env, *pushType, result.Outcome, result.Status, result.Reason)
	if result.Outcome != apns.PreflightPass {
		return fmt.Errorf("APNs preflight did not pass: outcome %s", result.Outcome)
	}
	return nil
}
