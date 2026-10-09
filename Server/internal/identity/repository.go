package identity

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	runtime *pgxpool.Pool
}

type identityLookup struct {
	AccountID, AuthorityStatus, DisplayName string
	ProfileRequired                         bool
}

func NewRepository(runtime *pgxpool.Pool) (*Repository, error) {
	if runtime == nil {
		return nil, errors.New("runtime database pool is required")
	}
	return &Repository{runtime: runtime}, nil
}

func (repository *Repository) CreateChallenge(ctx context.Context, digest [32]byte, binding OriginBinding, expiresAt time.Time) error {
	if repository == nil || repository.runtime == nil {
		return ErrUnavailable
	}
	createdAt := expiresAt.Add(-5 * time.Minute)
	var ignored string
	if err := repository.runtime.QueryRow(ctx, `SELECT app.create_auth_challenge($1,$2,$3,$4,$5)`,
		digest[:], binding.Origin, binding.InstanceID, expiresAt, createdAt).Scan(&ignored); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (repository *Repository) ConsumeChallenge(ctx context.Context, digest [32]byte, binding OriginBinding, now time.Time) error {
	if repository == nil || repository.runtime == nil {
		return ErrUnavailable
	}
	var ignored string
	err := repository.runtime.QueryRow(ctx, `SELECT app.consume_auth_challenge($1,$2,$3,$4)`,
		digest[:], binding.Origin, binding.InstanceID, now).Scan(&ignored)
	if err == nil {
		return nil
	}
	var databaseError *pgconn.PgError
	if errors.As(err, &databaseError) && databaseError.Code == "P0002" {
		return ErrInvalidOrExpiredChallenge
	}
	return ErrUnavailable
}

func (repository *Repository) lookupIdentity(ctx context.Context, provider string, digest KeyedDigest) (identityLookup, bool, error) {
	if repository == nil || repository.runtime == nil {
		return identityLookup{}, false, ErrUnavailable
	}
	var result identityLookup
	err := repository.runtime.QueryRow(ctx, `SELECT * FROM app.lookup_auth_identity($1,$2,$3)`,
		provider, digest.KeyID, digest.Sum[:]).Scan(&result.AccountID, &result.AuthorityStatus, &result.DisplayName, &result.ProfileRequired)
	if errors.Is(err, pgx.ErrNoRows) {
		return identityLookup{}, false, nil
	}
	if err != nil {
		return identityLookup{}, false, ErrUnavailable
	}
	return result, true, nil
}

func (repository *Repository) LookupAuthIdentity(ctx context.Context, provider, keyID string, digest [32]byte) (AuthIdentityLookup, error) {
	row, found, err := repository.lookupIdentity(ctx, provider, KeyedDigest{KeyID: keyID, Sum: digest})
	if err != nil {
		return AuthIdentityLookup{}, err
	}
	if !found {
		return AuthIdentityLookup{}, nil
	}
	return AuthIdentityLookup{AccountID: row.AccountID, AuthorityStatus: row.AuthorityStatus, DisplayName: row.DisplayName, ProfileRequired: row.ProfileRequired}, nil
}

func (repository *Repository) ReserveAppleNotification(ctx context.Context, candidates []KeyedDigest, commandID string, receivedAt time.Time) (string, bool, error) {
	if repository == nil || repository.runtime == nil || len(candidates) == 0 || len(candidates) > 8 || !validNonzeroUUID(commandID) ||
		receivedAt.IsZero() || receivedAt.Location() != time.UTC || receivedAt.Year() < 1 || receivedAt.Year() > 9999 {
		return "", false, ErrUnavailable
	}
	type encodedCandidate struct {
		KeyID  string `json:"key_id"`
		Digest string `json:"digest"`
	}
	encoded := make([]encodedCandidate, 0, len(candidates))
	seenIDs := make(map[string]struct{}, len(candidates))
	seenDigests := make(map[KeyedDigest]struct{}, len(candidates))
	for _, candidate := range candidates {
		if !keyIDPattern.MatchString(candidate.KeyID) {
			return "", false, ErrUnavailable
		}
		if _, exists := seenIDs[candidate.KeyID]; exists {
			return "", false, ErrUnavailable
		}
		if _, exists := seenDigests[candidate]; exists {
			return "", false, ErrUnavailable
		}
		seenIDs[candidate.KeyID] = struct{}{}
		seenDigests[candidate] = struct{}{}
		encoded = append(encoded, encodedCandidate{KeyID: candidate.KeyID, Digest: hex.EncodeToString(candidate.Sum[:])})
	}
	payload, err := json.Marshal(encoded)
	if err != nil {
		return "", false, ErrUnavailable
	}
	var storedCommandID string
	var newlyReserved bool
	err = repository.runtime.QueryRow(ctx, `SELECT * FROM app.reserve_apple_notification($1::jsonb,$2,$3,$4,$5)`,
		string(payload), candidates[0].KeyID, candidates[0].Sum[:], commandID, receivedAt).Scan(&storedCommandID, &newlyReserved)
	if err != nil || !validNonzeroUUID(storedCommandID) {
		return "", false, ErrUnavailable
	}
	return storedCommandID, newlyReserved, nil
}
