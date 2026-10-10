// Package pushtoken stores each device's APNs token for background wakes
// (PUT/DELETE /v1/devices/push-token, capability push-token-v1). A token is
// always the authenticated session's own device's: the request names no
// device, and the row's device and account come from the session row the
// request transaction authenticated.
package pushtoken

import (
	"context"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

// registerSQL upserts the device's one row. The tenant policy
// (push_token_owner: account_id = app.current_account_id()) scopes it to
// the authenticated account; the conflict update is further limited to a
// row of the same account.
const registerSQL = `INSERT INTO device_push_tokens(device_id, account_id, environment, token, registered_at)
VALUES ($1, $2, $3::text::app.apns_environment, $4, $5)
ON CONFLICT (device_id) DO UPDATE SET environment = EXCLUDED.environment, token = EXCLUDED.token, registered_at = EXCLUDED.registered_at
WHERE device_push_tokens.account_id = EXCLUDED.account_id`

// unregisterSQL deletes the device's row, if any.
const unregisterSQL = `DELETE FROM device_push_tokens WHERE device_id = $1 AND account_id = $2`

// action is the family-policy action the route authorizes: every role may
// register its own phone.
const action = "read"

// Service registers and unregisters device push tokens, each in one
// guarded request transaction (BEGIN and the authority lookup pipelined,
// the statement, COMMIT: three round trips; two when refused).
type Service struct {
	guard authority.RequestGuard
	now   func() time.Time
}

// New is a Service over guard; now stamps registered_at (nil: time.Now).
func New(guard authority.RequestGuard, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{guard: guard, now: now}
}

// Register stores token for the authenticated session's own device (one
// guarded transaction: BEGIN+authority, the upsert, COMMIT). Registering
// the same token again, or another one, replaces the row.
func (s *Service) Register(ctx context.Context, req authority.Request, environment string, token []byte) error {
	if s == nil || s.guard == nil {
		return authority.ErrUnavailable
	}
	at := s.now().UTC()
	return s.guard.WithRequestTx(ctx, req, action, func(tx pgx.Tx, p access.Principal, g access.Grant) error {
		device, account, err := bound(p, g)
		if err != nil {
			return err
		}
		// The handler holds a bad token or environment as Admit's refusal,
		// so this runs only for a caller that skipped it.
		if environment != "sandbox" && environment != "production" || len(token) < protocol.MinPushTokenBytes || len(token) > protocol.MaxPushTokenBytes {
			return protocol.ErrInvalidPushToken
		}
		tag, err := tx.Exec(ctx, registerSQL, device, account, environment, token, at)
		if err != nil {
			return unavailable("register", err)
		}
		if tag.RowsAffected() != 1 {
			return unavailable("register", fmt.Errorf("%d rows", tag.RowsAffected()))
		}
		return nil
	})
}

// Unregister deletes the session's own device's token (idempotent).
func (s *Service) Unregister(ctx context.Context, req authority.Request) error {
	if s == nil || s.guard == nil {
		return authority.ErrUnavailable
	}
	return s.guard.WithRequestTx(ctx, req, action, func(tx pgx.Tx, p access.Principal, g access.Grant) error {
		device, account, err := bound(p, g)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, unregisterSQL, device, account); err != nil {
			return unavailable("unregister", err)
		}
		return nil
	})
}

// bound is the row's device and account: the authenticated session's
// device and the grant's account, which must be the session's.
func bound(p access.Principal, g access.Grant) (string, string, error) {
	if p.DeviceID == "" || g.AccountID == "" || g.AccountID != p.AccountID {
		return "", "", unavailable("bind", errors.New("grant does not name the session's account"))
	}
	return p.DeviceID, g.AccountID, nil
}

// unavailable is a retryable service_unavailable keeping its cause for the
// 5xx log. A pgx error's text carries no bound value, so never the token.
func unavailable(what string, err error) error {
	return fmt.Errorf("pushtoken: %s: %w", what, errors.Join(err, authority.ErrUnavailable))
}
