package export

import (
	"context"
	"errors"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/security"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrOwnerRequired        = errors.New("owner_required")
	ErrNotFound             = errors.New("export_not_found")
	ErrInProgress           = errors.New("export_in_progress")
	ErrRateLimited          = errors.New("export_rate_limited")
	ErrNotReady             = errors.New("export_not_ready")
	ErrExpired              = errors.New("export_expired")
	ErrIntegrity            = errors.New("export_integrity")
	ErrTooLarge             = errors.New("export_too_large")
	ErrAuthorizationChanged = errors.New("export_authorization_changed")
	ErrUnavailable          = errors.New("export_unavailable")
	ErrInvalidRequest       = errors.New("invalid_request")
)

type FamilyResolver interface {
	Current(context.Context, access.Principal) (string, error)
}

type Options struct {
	Security    *security.Service
	Resolver    FamilyResolver
	Identity    protocol.ServerInfo
	Runtime     *pgxpool.Pool
	WorkerPool  *pgxpool.Pool
	Keys        map[string][32]byte
	ActiveKeyID string
	Now         func() time.Time
}

type Service struct{ options Options }
type Worker struct {
	options Options
	guard   *security.ExportSnapshotGuard
}

type Job struct {
	ID, FamilyID, RequesterAccountID, RequestID, State string
	InstanceID                                         string
	RecoveryEpoch, SecurityRevision                    uint64
	CreatedAt, ExpiresAt                               time.Time
	Attempt                                            int
	LeaseID                                            string
}

func New(options Options) (*Service, *Worker, error) {
	if options.Security == nil || options.Resolver == nil || options.Runtime == nil || options.WorkerPool == nil ||
		options.Now == nil || options.Identity.Validate() != nil || options.ActiveKeyID == "" || len(options.Keys) == 0 {
		return nil, nil, ErrUnavailable
	}
	if _, ok := options.Keys[options.ActiveKeyID]; !ok {
		return nil, nil, ErrUnavailable
	}
	guard, err := security.NewExportSnapshotGuard(options.Security, options.Runtime)
	if err != nil {
		return nil, nil, err
	}
	copyKeys := make(map[string][32]byte, len(options.Keys))
	for k, v := range options.Keys {
		if v == ([32]byte{}) {
			return nil, nil, ErrUnavailable
		}
		copyKeys[k] = v
	}
	options.Keys = copyKeys
	return &Service{options: options}, &Worker{options: options, guard: guard}, nil
}
