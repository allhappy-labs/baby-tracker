package security

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrLiveActivityAuthorizationChanged = errors.New("live_activity_authorization_changed")

// LiveActivitySubject contains no bearer tokens. SessionID, when present, must
// match the registration; the guard always resolves it from tenant storage.
type LiveActivitySubject struct {
	JobID               string    `json:"job_id"`
	LeaseID             string    `json:"lease_id"`
	RegistrationID      string    `json:"registration_id"`
	RegistrationVersion uint64    `json:"registration_version"`
	AccountID           string    `json:"account_id"`
	DeviceID            string    `json:"device_id"`
	FamilyID            string    `json:"family_id"`
	BabyID              string    `json:"baby_id"`
	Kind                string    `json:"kind"`
	InstanceID          string    `json:"instance_id"`
	RecoveryEpoch       uint64    `json:"recovery_epoch"`
	DirtyVersion        uint64    `json:"dirty_version"`
	LeaseDeadline       time.Time `json:"lease_deadline"`
	SessionID           string    `json:"-"`
}

type liveActivityBinding struct {
	sessionID, rootID string
	membership        uint64
	expires           time.Time
	currentRealm      bool
}
type LiveActivitySnapshotGuard struct {
	authority *Service
	runtime   *pgxpool.Pool
}

func NewLiveActivitySnapshotGuard(authority *Service, runtime *pgxpool.Pool) (*LiveActivitySnapshotGuard, error) {
	if authority == nil || runtime == nil {
		return nil, errors.New("live activity snapshot requires authority and runtime pool")
	}
	return &LiveActivitySnapshotGuard{authority, runtime}, nil
}

func readLiveActivityBinding(ctx context.Context, tx pgx.Tx, s LiveActivitySubject) (b liveActivityBinding, err error) {
	err = tx.QueryRow(ctx, `SELECT d.session_id::text,d.lineage_root_session_id::text,d.membership_revision,d.authority_expires_at,
 d.instance_id=s.instance_id AND d.recovery_epoch=s.recovery_epoch
 FROM public.live_activity_devices d CROSS JOIN public.server_state s
 WHERE s.singleton AND d.registration_id=$1 AND d.registration_version=$2 AND d.account_id=$3 AND d.device_id=$4
 AND d.family_id=$5 AND d.instance_id=$6 AND d.recovery_epoch=$7 AND NOT d.revoked`,
		s.RegistrationID, s.RegistrationVersion, s.AccountID, s.DeviceID, s.FamilyID, s.InstanceID, s.RecoveryEpoch).
		Scan(&b.sessionID, &b.rootID, &b.membership, &b.expires, &b.currentRealm)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrLiveActivityAuthorizationChanged
	}
	return
}

// liveActivityGrant uses the validated replay only; projected membership/session
// rows cannot restore a revoked grant. Rotation preserves the absolute expiry.
func (state State) liveActivityGrant(s LiveActivitySubject, b liveActivityBinding, epoch uint64, now time.Time, requireEntitlement bool) (access.Grant, error) {
	denied := ErrLiveActivityAuthorizationChanged
	account, accountOK := state.Account(s.AccountID)
	device, deviceOK := state.Device(s.DeviceID)
	slot, slotOK := state.FamilySlot(s.AccountID)
	family, familyOK := state.Family(s.FamilyID)
	member, memberOK := state.Membership(s.FamilyID, s.AccountID)
	if !b.currentRealm || s.RecoveryEpoch != epoch || !accountOK || account.Status != "active" || !deviceOK || device.Status != "active" || device.AccountID != s.AccountID || device.ServerInstanceID != s.InstanceID ||
		!slotOK || slot.State != "active" || slot.FamilyID != s.FamilyID || !familyOK || family.Status != "active" || !memberOK || member.Status != "active" ||
		(member.Role != "owner" && member.Role != "caregiver" && member.Role != "viewer") || b.membership == 0 || state.MembershipGeneration(s.FamilyID, s.AccountID) != b.membership ||
		!now.Before(b.expires) || !state.familyEntitled(s.FamilyID, now, requireEntitlement) || (s.SessionID != "" && s.SessionID != b.sessionID) {
		return access.Grant{}, denied
	}
	id := b.sessionID
	seen := make(map[string]bool)
	for steps := 0; steps < len(state.sessionLineages); steps++ {
		if seen[id] {
			return access.Grant{}, denied
		}
		seen[id] = true
		session, ok := state.Session(id)
		expiry, err := time.Parse(time.RFC3339Nano, session.ExpiresAt)
		if !ok || err != nil || !now.Before(expiry) || !expiry.Equal(b.expires) || session.AccountID != s.AccountID || session.DeviceID != s.DeviceID || session.RecoveryEpoch != epoch || session.LineageRootSessionID != b.rootID {
			return access.Grant{}, denied
		}
		if session.Status == "active" && session.ChildSessionID == "" {
			return access.Grant{AccountID: s.AccountID, FamilyID: s.FamilyID, Role: member.Role, Revision: state.head.Revision, MembershipRevision: b.membership}, nil
		}
		if session.Status != "rotated" || session.ChildSessionID == "" {
			return access.Grant{}, denied
		}
		child, ok := state.Session(session.ChildSessionID)
		if !ok || child.ParentSessionID != id {
			return access.Grant{}, denied
		}
		id = session.ChildSessionID
	}
	return access.Grant{}, denied
}

// WithLiveActivitySnapshot releases its transaction and authority fence before
// returning. Its callback builds a detached payload; APNs I/O belongs afterward.
func (g *LiveActivitySnapshotGuard) WithLiveActivitySnapshot(ctx context.Context, s LiveActivitySubject, run func(pgx.Tx, access.Grant) error) error {
	if g == nil || g.authority == nil || g.runtime == nil || run == nil {
		return ErrUnavailable
	}
	if err := g.authority.rejectCallbackEntry(ctx); err != nil {
		return err
	}
	return g.authority.WithAuthoritySnapshot(ctx, func(snapshot AuthoritySnapshot) error {
		tx, err := g.runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return ErrUnavailable
		}
		defer tx.Rollback(ctx)
		if err = tenant.SetLocal(ctx, tx, tenant.Context{AccountID: s.AccountID, FamilyID: s.FamilyID}); err != nil {
			return err
		}
		raw, err := json.Marshal(s)
		if err != nil {
			return err
		}
		var live bool
		if err = tx.QueryRow(ctx, `SELECT app.live_activity_claim_live($1)`, raw).Scan(&live); err != nil {
			return ErrUnavailable
		}
		if !live {
			return ErrLiveActivityAuthorizationChanged
		}
		b, err := readLiveActivityBinding(ctx, tx, s)
		if err != nil {
			return err
		}
		grant, err := snapshot.View.owner.state.liveActivityGrant(s, b, snapshot.RecoveryEpoch, g.authority.now().UTC(), g.authority.familyPolicy.RequireFamilySyncEntitlement)
		if err != nil {
			return err
		}
		return run(tx, grant)
	})
}

// SweepLiveActivityRegistrations is called periodically even when there are no
// family mutations. Bounded keyset pages release the fence between batches.
// Only identity coordinates cross the definer; token reads and revocation use
// runtime tenant RLS under current validated authority.
func (g *LiveActivitySnapshotGuard) SweepLiveActivityRegistrations(ctx context.Context) (int, error) {
	if g == nil || g.authority == nil || g.runtime == nil {
		return 0, ErrUnavailable
	}
	after := "00000000-0000-0000-0000-000000000000"
	total := 0
	for {
		batch := 0
		err := g.authority.WithAuthoritySnapshot(ctx, func(snapshot AuthoritySnapshot) error {
			tx, err := g.runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			rows, err := tx.Query(ctx, `SELECT app.live_activity_registration_subjects($1,100)`, after)
			if err != nil {
				return err
			}
			subjects := []LiveActivitySubject{}
			for rows.Next() {
				var raw []byte
				var s LiveActivitySubject
				if err = rows.Scan(&raw); err != nil {
					rows.Close()
					return err
				}
				if err = json.Unmarshal(raw, &s); err != nil {
					rows.Close()
					return err
				}
				subjects = append(subjects, s)
			}
			rows.Close()
			if err = rows.Err(); err != nil {
				return err
			}
			revoked := 0
			for _, s := range subjects {
				if err = tenant.SetLocal(ctx, tx, tenant.Context{AccountID: s.AccountID, FamilyID: s.FamilyID}); err != nil {
					return err
				}
				b, e := readLiveActivityBinding(ctx, tx, s)
				if errors.Is(e, ErrLiveActivityAuthorizationChanged) {
					continue
				}
				if e != nil {
					return e
				}
				if _, e = snapshot.View.owner.state.liveActivityGrant(s, b, snapshot.RecoveryEpoch, g.authority.now().UTC(), g.authority.familyPolicy.RequireFamilySyncEntitlement); e != nil {
					result, e := tx.Exec(ctx, `UPDATE public.live_activity_devices SET revoked=true WHERE registration_id=$1 AND registration_version=$2 AND NOT revoked`, s.RegistrationID, s.RegistrationVersion)
					if e != nil {
						return e
					}
					revoked += int(result.RowsAffected())
				}
			}
			if err = tx.Commit(ctx); err != nil {
				return err
			}
			batch = len(subjects)
			total += revoked
			if batch > 0 {
				after = subjects[batch-1].RegistrationID
			}
			return nil
		})
		if err != nil {
			return total, err
		}
		if batch < 100 {
			return total, nil
		}
	}
}
