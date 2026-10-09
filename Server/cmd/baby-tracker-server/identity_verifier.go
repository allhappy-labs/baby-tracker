//go:build !identitytest

package main

import (
	"net/http"
	"os"
	"time"

	"babytracker/server/internal/auth"
	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
)

func newConfiguredIdentityVerifier(cfg config.Config) (identity.IdentityVerifier, error) {
	return identity.NewAppleVerifier(identity.AppleOptions{Audience: cfg.AppleAudience, Now: time.Now})
}

func wrapIdentityTestHandler(handler http.Handler, _ string) http.Handler { return handler }

func identityTestHostedAdapterEnabled(config.Config) bool { return false }

func configureIdentityTestHostedAuth(config.Config, *auth.Options) (bool, error) { return false, nil }

func loadServerConfig() (config.Config, error) { return config.Load(os.Getenv) }
