package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/recovery"
	"babytracker/server/internal/security"
	"babytracker/server/internal/security/localstore"
	"github.com/jackc/pgx/v5/pgxpool"
)

const overlapIdentitySubject = "overlap-family-http-owner"
const overlapCaregiverIdentitySubject = "overlap-family-http-caregiver"

func seedOverlapFamilySession(ctx context.Context, cfg config.Config, curlConfigPath string) error {
	return seedOverlapFamilySessionForSubject(ctx, cfg, curlConfigPath, overlapIdentitySubject, "", "")
}

func seedOverlapFamilySessionForSubject(ctx context.Context, cfg config.Config, curlConfigPath, subject, refreshRequestPath, accountIDPath string) error {
	if cfg.Mode != config.ModeDevelopment || cfg.PublicOrigin == nil || !filepath.IsAbs(curlConfigPath) {
		return errors.New("overlap session seeding requires development mode, canonical origin, and absolute output path")
	}
	if subject != overlapIdentitySubject && subject != overlapCaregiverIdentitySubject {
		return errors.New("unsupported overlap identity subject")
	}
	if refreshRequestPath != "" && !filepath.IsAbs(refreshRequestPath) {
		return errors.New("overlap refresh request path must be absolute")
	}
	if accountIDPath != "" && !filepath.IsAbs(accountIDPath) {
		return errors.New("overlap account ID path must be absolute")
	}
	file, err := openOverlapPrivateFile(curlConfigPath)
	if err != nil {
		return errors.New("reserve curl configuration")
	}
	completed := false
	defer func() {
		_ = file.Close()
		if !completed {
			_ = os.Remove(curlConfigPath)
			if accountIDPath != "" {
				_ = os.Remove(accountIDPath)
			}
		}
	}()
	runtime, err := database.OpenAndPing(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open runtime database: %w", err)
	}
	defer runtime.Close()
	projector, err := database.OpenAndPing(ctx, cfg.ProjectorDatabaseURL)
	if err != nil {
		return fmt.Errorf("open projector database: %w", err)
	}
	defer projector.Close()
	store, err := localstore.Open(cfg.SecurityLedgerDir, cfg.LedgerHeadDir)
	if err != nil {
		return err
	}
	bundle, err := issueOverlapFamilySessionForSubject(ctx, cfg, runtime, projector, store, subject)
	if err != nil {
		return err
	}
	if err := writeOverlapCurlConfigFile(file, bundle.AccessToken); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return errors.New("close curl configuration")
	}
	if refreshRequestPath != "" {
		encoded, encodeErr := json.Marshal(protocol.RefreshRequest{RequestMeta: protocol.RequestMeta{
			ProtocolVersion: "v1", Capabilities: protocol.IdentityCapabilities(),
		}, RefreshToken: bundle.RefreshToken})
		if encodeErr != nil || writeOverlapPrivateFile(refreshRequestPath, string(encoded)) != nil {
			return errors.New("write private overlap refresh request")
		}
	}
	if accountIDPath != "" && writeOverlapPrivateFile(accountIDPath, bundle.Account.ID) != nil {
		return errors.New("write private overlap account ID")
	}
	completed = true
	return nil
}

func issueOverlapFamilySession(ctx context.Context, cfg config.Config, runtime, projector *pgxpool.Pool, store localstore.Store) (auth.SessionBundle, error) {
	return issueOverlapFamilySessionForSubject(ctx, cfg, runtime, projector, store, overlapIdentitySubject)
}

func issueOverlapFamilySessionForSubject(ctx context.Context, cfg config.Config, runtime, projector *pgxpool.Pool, store localstore.Store, subject string) (auth.SessionBundle, error) {
	if runtime == nil || projector == nil || runtime == projector || cfg.PublicOrigin == nil || cfg.RecoveryEpoch == 0 {
		return auth.SessionBundle{}, errors.New("complete overlap session dependencies are required")
	}
	var expectedNonce string
	identityService, sessions, err := newOverlapSessionServicesForSubject(cfg, runtime, projector, store, subject, func() string { return expectedNonce })
	if err != nil {
		return auth.SessionBundle{}, err
	}
	challenge, err := identityService.Challenge(ctx)
	if err != nil {
		return auth.SessionBundle{}, err
	}
	expectedNonce, err = identity.AppleNonce(cfg.PublicOrigin.String(), cfg.ServerInstanceID, challenge.Challenge)
	if err != nil {
		return auth.SessionBundle{}, err
	}
	commandID, err := freshOverlapUUID()
	if err != nil {
		return auth.SessionBundle{}, err
	}
	bundle, err := sessions.Exchange(ctx, identity.ExchangeRequest{CommandID: commandID, Challenge: challenge.Challenge,
		IdentityToken: subject, InstallationID: "overlap-family-http-" + commandID,
		Profile: &identity.ProfileInput{DisplayName: "Overlap caregiver", Confirmed: true}})
	if err != nil {
		return auth.SessionBundle{}, err
	}
	principal, err := sessions.Authenticate(ctx, bundle.AccessToken)
	if err != nil || principal.AccountID != bundle.Account.ID || principal.DeviceID != bundle.DeviceID {
		return auth.SessionBundle{}, errors.New("issued overlap session did not authenticate")
	}
	return bundle, nil
}

func newOverlapSessionServices(cfg config.Config, runtime, projector *pgxpool.Pool, store localstore.Store, expectedNonce func() string) (*identity.Service, *auth.Service, error) {
	return newOverlapSessionServicesForSubject(cfg, runtime, projector, store, overlapIdentitySubject, expectedNonce)
}

func newOverlapSessionServicesForSubject(cfg config.Config, runtime, projector *pgxpool.Pool, store localstore.Store, subject string, expectedNonce func() string) (*identity.Service, *auth.Service, error) {
	if expectedNonce == nil {
		return nil, nil, errors.New("expected nonce source is required")
	}
	now := time.Now
	verifier := overlapIdentityVerifierForSubject(cfg.AppleAudience, subject, expectedNonce, now)
	identityRepository, err := identity.NewRepository(runtime)
	if err != nil {
		return nil, nil, err
	}
	identityService, err := identity.New(identity.Options{Verifier: verifier, Repository: identityRepository,
		Origin: cfg.PublicOrigin.String(), InstanceID: cfg.ServerInstanceID,
		IdentityKeys: overlapKeyring(cfg.Auth.IdentityKeys), Now: now})
	if err != nil {
		return nil, nil, err
	}
	durable, err := recovery.NewVerifiedStore(cfg.LedgerHeadDir, store.Fence, store.Ledger, store.Heads)
	if err != nil {
		return nil, nil, err
	}
	authority, err := security.New(security.Options{Ledger: store.Ledger, Heads: store.Heads,
		Projection: security.NewProjector(projector), Fence: store.Fence, Now: now, Runtime: runtime,
		Decide: func(context.Context, access.Principal, string, string) (access.Grant, error) {
			return access.Grant{}, security.ErrUnavailable
		}, Epoch: func(ctx context.Context) (uint64, error) {
			state, err := durable.LoadUnderFence(ctx)
			if err != nil || state.InstanceID != cfg.ServerInstanceID || state.Epoch != cfg.RecoveryEpoch {
				return 0, errors.New("overlap recovery identity mismatch")
			}
			return state.Epoch, nil
		},
		MaxCaregivers: cfg.Limits.MaxCaregivers, MaxInvitations: cfg.Limits.MaxInvitations})
	if err != nil {
		return nil, nil, err
	}
	authRepository, err := auth.NewRepository(runtime)
	if err != nil {
		return nil, nil, err
	}
	sessions, err := auth.New(auth.Options{Identity: identityService, Verifier: verifier, Security: authority, Repository: authRepository,
		Mode: config.ModeSelfHosted, Origin: cfg.PublicOrigin.String(), InstanceID: cfg.ServerInstanceID,
		TokenKeys: overlapKeyring(cfg.Auth.SessionTokenKeys), DeviceKeys: overlapKeyring(cfg.Auth.DeviceContinuityKeys),
		NotificationKeys: overlapKeyring(cfg.Auth.AppleNotificationKeys), ActorIDKey: cfg.Auth.ActorIDPepper, Now: now})
	if err != nil {
		return nil, nil, err
	}
	return identityService, sessions, nil
}

func overlapIdentityVerifier(audience string, expectedNonce func() string, now func() time.Time) identity.IdentityVerifier {
	return overlapIdentityVerifierForSubject(audience, overlapIdentitySubject, expectedNonce, now)
}

func overlapIdentityVerifierForSubject(audience, subject string, expectedNonce func() string, now func() time.Time) identity.IdentityVerifier {
	return identity.IdentityVerifierFunc(func(_ context.Context, token, nonce string) (identity.VerifiedIdentity, error) {
		if token != subject || nonce == "" || nonce != expectedNonce() {
			return identity.VerifiedIdentity{}, identity.ErrInvalidAppleProof
		}
		current := now().UTC()
		return identity.VerifiedIdentity{Provider: "apple", Audience: audience, Subject: token,
			IssuedAt: current.Add(-time.Minute), ExpiresAt: current.Add(time.Hour)}, nil
	})
}

func freshOverlapUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", errors.New("generate overlap command ID")
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[:4], value[4:6], value[6:8], value[8:10], value[10:]), nil
}

func overlapKeyring(source config.DigestKeyringConfig) identity.DigestKeyring {
	result := identity.DigestKeyring{Current: identity.DigestKey{ID: source.Current.ID, Secret: source.Current.Secret}}
	result.Previous = make([]identity.DigestKey, len(source.Previous))
	for index, key := range source.Previous {
		result.Previous[index] = identity.DigestKey{ID: key.ID, Secret: key.Secret}
	}
	return result
}

func writeOverlapCurlConfig(path, accessToken string) error {
	file, err := openOverlapPrivateFile(path)
	if err != nil {
		return errors.New("create curl configuration")
	}
	if err := writeOverlapCurlConfigFile(file, accessToken); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	return file.Close()
}

func openOverlapPrivateFile(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("absolute private output path is required")
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func writeOverlapCurlConfigFile(file *os.File, accessToken string) error {
	if file == nil || accessToken == "" {
		return errors.New("curl configuration and access token are required")
	}
	if _, err := fmt.Fprintf(file, "silent\nshow-error\nfail\nheader = \"Authorization: Bearer %s\"\n", accessToken); err != nil {
		return errors.New("write curl configuration")
	}
	return nil
}
