package limits

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const takeRateLimitsSQL = `SELECT allowed,remaining,retry_after_seconds FROM app.take_rate_limits($1::jsonb,$2)`

type Repository struct {
	runtime *pgxpool.Pool
}

func NewRepository(runtime *pgxpool.Pool) (*Repository, error) {
	if runtime == nil {
		return nil, errors.New("limits repository requires runtime database")
	}
	return &Repository{runtime: runtime}, nil
}

func (repository *Repository) take(ctx context.Context, payload []byte, now time.Time) (Decision, error) {
	if repository == nil || repository.runtime == nil {
		return Decision{}, errors.New("limits repository is unavailable")
	}
	var charges []wireCharge
	if err := json.Unmarshal(payload, &charges); err != nil || len(charges) == 0 || len(charges) > maximumCharges {
		return Decision{}, errors.New("invalid rate-limit payload")
	}
	preflight := Decision{Allowed: true, Remaining: math.MaxInt}
	for _, charge := range charges {
		if charge.Cost <= charge.Limit {
			continue
		}
		end, err := time.Parse(time.RFC3339Nano, charge.WindowEnd)
		if err != nil || !end.After(now) {
			return Decision{}, errors.New("invalid rate-limit window")
		}
		preflight.Allowed, preflight.Remaining = false, 0
		preflight.RetryAfter = max(preflight.RetryAfter, time.Duration(math.Ceil(end.Sub(now).Seconds()))*time.Second)
	}
	if !preflight.Allowed {
		return preflight, nil
	}
	// Migration 000005 freezes the SECURITY DEFINER function's accepted scope
	// registry. Later protocol features keep that audited SQL immutable and map
	// their dimensions onto stable accepted storage namespaces. Key digests were
	// already domain-separated with the public logical scope before this mapping,
	// so no bucket can collide with legacy traffic or another new dimension.
	storageScopes := map[string]string{
		ScopeSyncMutationAccount: ScopeAuthRefreshAccount,
		ScopeSyncMutationDevice:  ScopeAuthRefreshDevice,
		ScopeSyncMutationFamily:  ScopeFamilyStructuralAccount,
		ScopeSyncPullAccount:     ScopeFamilyStructuralDevice,
		ScopeSyncPullDevice:      ScopeInvitationCreateAccount,
		ScopeLiveAttemptIP:       ScopeAuthChallengeIP,
		ScopeLiveAttemptDevice:   ScopeAuthRefreshDevice,
		ScopeLiveAttemptAccount:  ScopeAuthRefreshAccount,
		ScopeLiveAttemptFamily:   ScopeFamilyStructuralAccount,
		ScopeExportIP:            ScopeAuthChallengeIP,
		ScopeExportAccount:       ScopeAuthRefreshAccount,
		ScopeLiveActivityAccount: ScopeAuthRefreshAccount,
		ScopeLiveActivityDevice:  ScopeAuthRefreshDevice,
		ScopeExportDevice:        ScopeAuthRefreshDevice,
	}
	for index := range charges {
		if scope, ok := storageScopes[charges[index].Scope]; ok {
			charges[index].Scope = scope
		}
	}
	sort.Slice(charges, func(left, right int) bool {
		if charges[left].Scope != charges[right].Scope {
			return charges[left].Scope < charges[right].Scope
		}
		return charges[left].KeyDigest < charges[right].KeyDigest
	})
	tx, err := repository.runtime.Begin(ctx)
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback(ctx)
	combined := Decision{Allowed: true, Remaining: math.MaxInt}
	for start := 0; start < len(charges); start += 3 {
		end := min(start+3, len(charges))
		chunk, err := json.Marshal(charges[start:end])
		if err != nil {
			return Decision{}, err
		}
		var allowed bool
		var remaining, retryAfterSeconds int
		if err := tx.QueryRow(ctx, takeRateLimitsSQL, chunk, now).Scan(&allowed, &remaining, &retryAfterSeconds); err != nil {
			return Decision{}, err
		}
		combined.Allowed = combined.Allowed && allowed
		combined.Remaining = min(combined.Remaining, remaining)
		combined.RetryAfter = max(combined.RetryAfter, time.Duration(retryAfterSeconds)*time.Second)
		if !allowed {
			return combined, nil // deferred rollback preserves all-or-none admission
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Decision{}, err
	}
	return combined, nil
}
