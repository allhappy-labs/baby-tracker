package familyread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"github.com/jackc/pgx/v5"
)

// Service reads canonical family data for integration credentials. It holds
// no authorization state: every call presents the credential to the guard,
// and every read happens inside that guard's transaction.
type Service struct {
	guard          Guard
	cursorKey      [32]byte
	now            func() time.Time
	maxResultBytes int
	summaryTimeout time.Duration
	// maxMeasurements is MaxSummaryMeasurements; tests lower it.
	maxMeasurements int
}

// New builds the service. cursorKey authenticates history continuations and
// must be 32 secret random bytes dedicated to that purpose. A service with a
// nil guard or clock or an all-zero key refuses every call as unavailable.
func New(guard Guard, cursorKey [32]byte, now func() time.Time) *Service {
	return &Service{guard: guard, cursorKey: cursorKey, now: now, maxResultBytes: MaxResultBytes,
		summaryTimeout: SummaryTimeout, maxMeasurements: MaxSummaryMeasurements}
}

func (s *Service) ready() error {
	if s == nil || s.guard == nil || s.now == nil || s.cursorKey == ([32]byte{}) {
		return authority.ErrUnavailable
	}
	return nil
}

// ListBabies returns the authorized family's live babies, ordered by sort
// order then ID, at one family data revision. The result is complete or an
// error: more than MaxBabies babies, or more than MaxResultBytes encoded,
// returns ErrResultTooLarge.
func (s *Service) ListBabies(ctx context.Context, credential string) (BabiesResult, error) {
	if err := s.ready(); err != nil {
		return BabiesResult{}, err
	}
	var result BabiesResult
	err := s.guard.WithIntegrationRead(ctx, credential, func(ctx context.Context, tx pgx.Tx, grant access.Grant) error {
		revision, err := familyRevision(ctx, tx, grant.FamilyID)
		if err != nil {
			return err
		}
		babies, err := readBabies(ctx, tx, grant.FamilyID, MaxBabies+1)
		if err != nil {
			return err
		}
		if len(babies) > MaxBabies {
			return ErrResultTooLarge
		}
		candidate := BabiesResult{Revision: revision, Babies: babies}
		size, err := encodedBytes(candidate)
		if err != nil {
			return err
		}
		if size > s.maxResultBytes {
			return ErrResultTooLarge
		}
		result = candidate
		return nil
	})
	if err != nil {
		return BabiesResult{}, err
	}
	return result, nil
}

// History returns one page of a baby's canonical care history (see
// HistorySelection) bounded by the query limit and MaxResultBytes.
func (s *Service) History(ctx context.Context, credential string, q HistoryQuery) (HistoryResult, error) {
	return s.history(ctx, credential, q, nil)
}

// HistoryWithBudget is History with the transport's final-encoding predicate
// applied, inside the same guarded read and revision, before the page's
// cursor is chosen. The page keeps canonical order and may hold fewer
// entries than the limit; its cursor continues after the last kept entry.
// When the fixed fields or the first entity cannot fit it returns
// ErrResultTooLarge rather than skipping data. fits is never retained or
// called after HistoryWithBudget returns.
func (s *Service) HistoryWithBudget(ctx context.Context, credential string, q HistoryQuery, fits HistoryBudget) (HistoryResult, error) {
	if fits == nil {
		return HistoryResult{}, authority.ErrUnavailable
	}
	return s.history(ctx, credential, q, fits)
}

func familyRevision(ctx context.Context, tx pgx.Tx, familyID string) (uint64, error) {
	var revision int64
	err := tx.QueryRow(ctx, `SELECT next_sequence FROM families WHERE id=$1`, familyID).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		// The guard authorized this family; not seeing it means the tenant
		// snapshot is inconsistent, so fail closed.
		return 0, authority.ErrUnavailable
	}
	if err != nil {
		return 0, err
	}
	if revision < 0 {
		return 0, dataInvalid("family", familyID)
	}
	return uint64(revision), nil
}

func encodedBytes(value any) (int, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	return len(raw), nil
}

func dataInvalid(kind, id string) error {
	return fmt.Errorf("%w: %s %s", ErrDataInvalid, kind, id)
}
