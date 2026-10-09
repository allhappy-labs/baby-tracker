//go:build identitytest

package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"babytracker/server/internal/auth"
	"babytracker/server/internal/config"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/identity/hostedcredentials"
	"babytracker/server/internal/testidentity"
	"babytracker/server/internal/testkit"
)

var identityTestState struct {
	sync.RWMutex
	issuer *testidentity.Issuer
}

func newConfiguredIdentityVerifier(cfg config.Config) (identity.IdentityVerifier, error) {
	issuer, err := testidentity.New("protocol-e2e", time.Now)
	if err != nil {
		return nil, err
	}
	verifier, err := issuer.Verifier(cfg.AppleAudience)
	if err != nil {
		return nil, err
	}
	identityTestState.Lock()
	identityTestState.issuer = issuer
	identityTestState.Unlock()
	return verifier, nil
}

func wrapIdentityTestHandler(handler http.Handler, audience string) http.Handler {
	mux := http.NewServeMux()
	faults := &testkit.RecoveryFaults{}
	mux.Handle("/", httpapi.RecoveryFaultHandler(handler, func(point, attempt string) error { return faults.FailOnce(testkit.RecoveryFaultPoint(point), attempt) }))
	mux.HandleFunc("POST /__identitytest/proof", func(response http.ResponseWriter, request *http.Request) {
		var input struct {
			Subject string `json:"subject"`
			Nonce   string `json:"nonce"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(response, request.Body, 1024))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || input.Subject == "" || len(input.Nonce) != 64 {
			http.Error(response, "invalid request", http.StatusBadRequest)
			return
		}
		identityTestState.RLock()
		issuer := identityTestState.issuer
		identityTestState.RUnlock()
		if issuer == nil {
			http.Error(response, "unavailable", http.StatusServiceUnavailable)
			return
		}
		token, err := issuer.Sign(audience, input.Subject, input.Nonce)
		if err != nil {
			http.Error(response, "unavailable", http.StatusServiceUnavailable)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(response).Encode(map[string]string{"identity_token": token})
	})
	registerIdentityTestAPNS(mux)
	return mux
}

func identityTestHostedAdapterEnabled(cfg config.Config) bool {
	return cfg.Mode == config.ModeHosted && os.Getenv(identityTestHostedAdapterEnv) == "1"
}

func loadServerConfig() (config.Config, error) {
	if os.Getenv("BT_MODE") != string(config.ModeHosted) || os.Getenv(identityTestHostedAdapterEnv) != "1" {
		return config.Load(os.Getenv)
	}
	cfg, err := config.Load(func(name string) string {
		if name == "BT_MODE" {
			return string(config.ModeSelfHosted)
		}
		return os.Getenv(name)
	})
	if err != nil {
		return config.Config{}, err
	}
	cfg.Mode = config.ModeHosted
	return cfg, nil
}

type identityTestApple struct {
	issuer   *testidentity.Issuer
	audience string
}

func (apple *identityTestApple) ExchangeCode(_ context.Context, request identity.AppleCodeExchangeRequest) (identity.AppleTokens, error) {
	if apple == nil || apple.issuer == nil || request.AuthorizationCode == "" {
		return identity.AppleTokens{}, identity.ErrInvalidAppleProof
	}
	proof, err := apple.issuer.Sign(apple.audience, request.ExpectedIdentity.Subject, request.ExpectedNonce)
	if err != nil {
		return identity.AppleTokens{}, err
	}
	return identity.AppleTokens{IdentityToken: proof, RefreshToken: "identitytest-refresh-" + request.AuthorizationCode}, nil
}

func (*identityTestApple) Revoke(context.Context, identity.AppleRevocationRequest) error { return nil }

func configureIdentityTestHostedAuth(cfg config.Config, options *auth.Options) (bool, error) {
	if !identityTestHostedAdapterEnabled(cfg) {
		return false, nil
	}
	identityTestState.RLock()
	issuer := identityTestState.issuer
	identityTestState.RUnlock()
	if issuer == nil || options == nil {
		return false, errHostedAdapterUnavailable
	}
	apple := &identityTestApple{issuer: issuer, audience: cfg.AppleAudience}
	store, err := newIdentityTestSecretStore(filepath.Join(cfg.LedgerHeadDir, "identitytest-credentials"))
	if err != nil {
		return false, err
	}
	adapters, err := hostedcredentials.NewCredentialAdapters(store, identityTestCipher{key: cfg.Auth.DeviceContinuityKeys.Current.Secret}, "identitytest-vault-v1")
	if err != nil {
		return false, err
	}
	rotator, err := identity.NewCredentialRotator(adapters.Vault, apple, identityTestUUID)
	if err != nil {
		return false, err
	}
	options.Apple = apple
	options.Credentials = rotator
	return true, nil
}

type identityTestSecretStore struct {
	directory string
	mu        sync.Mutex
}

type identityTestSecretRecord struct {
	Object    hostedcredentials.SecretObject `json:"object"`
	CreatedAt time.Time                      `json:"created_at"`
}

func newIdentityTestSecretStore(directory string) (*identityTestSecretStore, error) {
	if directory == "" {
		return nil, errHostedAdapterUnavailable
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	return &identityTestSecretStore{directory: directory}, nil
}

func (store *identityTestSecretStore) path(id string) string {
	digest := sha256.Sum256([]byte(id))
	return filepath.Join(store.directory, hex.EncodeToString(digest[:])+".json")
}

func (store *identityTestSecretStore) Resolve(_ context.Context, id string) (hostedcredentials.SecretObject, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, err := store.read(id)
	if errors.Is(err, fs.ErrNotExist) {
		return hostedcredentials.SecretObject{}, false, nil
	}
	return record.Object, err == nil, err
}

func (store *identityTestSecretStore) Create(_ context.Context, object hostedcredentials.SecretObject) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	object.Version = identityTestSecretVersion()
	return store.write(object, time.Now().UTC(), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
}

func (store *identityTestSecretStore) CompareAndSwap(_ context.Context, id, version string, object hostedcredentials.SecretObject) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, err := store.read(id)
	if err != nil {
		return false, err
	}
	if record.Object.Version != version {
		return false, nil
	}
	object.Version = identityTestSecretVersion()
	return true, store.replace(object, record.CreatedAt)
}

func (store *identityTestSecretStore) Delete(_ context.Context, id, version string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, err := store.read(id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.Object.Version != version {
		return identity.ErrCredentialConflict
	}
	return os.Remove(store.path(id))
}

func (store *identityTestSecretStore) ListMetadata(_ context.Context, _ time.Time, cursor string, limit int) ([]hostedcredentials.SecretMetadata, string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	entries, err := os.ReadDir(store.directory)
	if err != nil {
		return nil, "", err
	}
	var metadata []hostedcredentials.SecretMetadata
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(store.directory, entry.Name()))
		if readErr != nil {
			return nil, "", readErr
		}
		var record identityTestSecretRecord
		if json.Unmarshal(raw, &record) != nil || record.Object.ID == "" || record.Object.ID <= cursor {
			continue
		}
		metadata = append(metadata, hostedcredentials.SecretMetadata{ObjectID: record.Object.ID, Version: record.Object.Version, KeyID: record.Object.KeyID, CreatedAt: record.CreatedAt})
	}
	sort.Slice(metadata, func(i, j int) bool { return metadata[i].ObjectID < metadata[j].ObjectID })
	next := ""
	if limit > 0 && len(metadata) > limit {
		metadata = metadata[:limit]
		next = metadata[len(metadata)-1].ObjectID
	}
	return metadata, next, nil
}

func (store *identityTestSecretStore) read(id string) (identityTestSecretRecord, error) {
	raw, err := os.ReadFile(store.path(id))
	if err != nil {
		return identityTestSecretRecord{}, err
	}
	var record identityTestSecretRecord
	if err = json.Unmarshal(raw, &record); err != nil || record.Object.ID != id {
		return identityTestSecretRecord{}, identity.ErrCredentialConflict
	}
	return record, nil
}

func (store *identityTestSecretStore) write(object hostedcredentials.SecretObject, createdAt time.Time, flags int) error {
	raw, err := json.Marshal(identityTestSecretRecord{Object: object, CreatedAt: createdAt})
	if err != nil {
		return err
	}
	file, err := os.OpenFile(store.path(object.ID), flags, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (store *identityTestSecretStore) replace(object hostedcredentials.SecretObject, createdAt time.Time) error {
	temporary, err := os.CreateTemp(store.directory, ".credential-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	_ = temporary.Close()
	defer os.Remove(temporaryPath)
	raw, err := json.Marshal(identityTestSecretRecord{Object: object, CreatedAt: createdAt})
	if err != nil {
		return err
	}
	if err = os.WriteFile(temporaryPath, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(temporaryPath, store.path(object.ID))
}

type identityTestCipher struct{ key [32]byte }

func identityTestSecretVersion() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(value[:])
}

func (value identityTestCipher) Seal(_ context.Context, _ string, plaintext, additional []byte) ([]byte, error) {
	block, err := aes.NewCipher(value.key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, additional), nil
}

func (value identityTestCipher) Open(_ context.Context, _ string, ciphertext, additional []byte) ([]byte, error) {
	block, err := aes.NewCipher(value.key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(ciphertext) < gcm.NonceSize() {
		return nil, identity.ErrCredentialConflict
	}
	return gcm.Open(nil, ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():], additional)
}

func identityTestUUID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return ""
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}
