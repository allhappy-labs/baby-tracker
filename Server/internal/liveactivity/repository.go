package liveactivity

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrVersionConflict = errors.New("live_activity_version_conflict")
var ErrRegistrationDenied = errors.New("live_activity_registration_denied")

type Repository struct{ topic string }

func NewRepository(topic string) *Repository { return &Repository{topic: topic} }

type registrationBinding struct {
	instance, root string
	expires        time.Time
}

func (r *Repository) binding(ctx context.Context, tx pgx.Tx, p access.Principal, g access.Grant) (registrationBinding, error) {
	var b registrationBinding
	if r.topic == "" || p.AccountID != g.AccountID || g.MembershipRevision == 0 || g.MembershipRevision > protocol.MaxSafeInteger {
		return b, ErrRegistrationDenied
	}
	err := tx.QueryRow(ctx, `SELECT instance_id::text,lineage_root_session_id::text,authority_expires_at FROM app.live_activity_session_binding($1,$2,$3,$4)`, p.AccountID, p.DeviceID, p.SessionID, p.RecoveryEpoch).Scan(&b.instance, &b.root, &b.expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrRegistrationDenied
	}
	return b, err
}
func receipt(b registrationBinding, epoch, version uint64) protocol.LiveActivityRegistrationReceipt {
	return protocol.LiveActivityRegistrationReceipt{ProtocolVersion: "v1", ServerInstanceID: b.instance, RecoveryEpoch: epoch, AcceptedRegistrationVersion: version, Generations: []protocol.LiveActivityGenerationMapping{}}
}
func registrationError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "P0001" && pgErr.Message == "live_activity_version_conflict" {
		return ErrVersionConflict
	}
	if errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "42501") {
		return ErrRegistrationDenied
	}
	return err
}
func (r *Repository) RegisterDevice(ctx context.Context, tx pgx.Tx, p access.Principal, g access.Grant, req protocol.LiveActivityDeviceRegistration) (protocol.LiveActivityRegistrationReceipt, error) {
	if err := req.Validate(); err != nil {
		return protocol.LiveActivityRegistrationReceipt{}, err
	}
	b, err := r.binding(ctx, tx, p, g)
	if err != nil {
		return protocol.LiveActivityRegistrationReceipt{}, err
	}
	var fenced bool
	if err = tx.QueryRow(ctx, `SELECT app.fence_live_activity_installation($1,$2,$3,$4,$5,$6,$7,$8,false)`, b.instance, p.RecoveryEpoch, req.InstallationID, p.AccountID, p.DeviceID, g.FamilyID, req.RegistrationVersion, b.expires).Scan(&fenced); err != nil {
		return protocol.LiveActivityRegistrationReceipt{}, registrationError(err)
	}
	if !fenced {
		return protocol.LiveActivityRegistrationReceipt{}, ErrVersionConflict
	}
	// The conditional UPSERT is the version CAS. A no-op identical retry returns
	// the stored version without firing an UPDATE trigger or enqueueing extra work.
	var version uint64
	err = tx.QueryRow(ctx, `INSERT INTO live_activity_devices(installation_id,account_id,device_id,family_id,instance_id,recovery_epoch,registration_version,environment,topic,push_token,payload_version,feed_break_minutes,session_id,lineage_root_session_id,membership_revision,authority_expires_at,registered_sequence)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,(SELECT next_sequence FROM families WHERE id=$4))
 ON CONFLICT(instance_id,recovery_epoch,installation_id,family_id) DO UPDATE SET
 registration_version=EXCLUDED.registration_version,environment=EXCLUDED.environment,topic=EXCLUDED.topic,push_token=EXCLUDED.push_token,payload_version=EXCLUDED.payload_version,feed_break_minutes=EXCLUDED.feed_break_minutes,
 session_id=EXCLUDED.session_id,lineage_root_session_id=EXCLUDED.lineage_root_session_id,membership_revision=EXCLUDED.membership_revision,authority_expires_at=EXCLUDED.authority_expires_at,revoked=false
 WHERE live_activity_devices.account_id=EXCLUDED.account_id AND live_activity_devices.device_id=EXCLUDED.device_id AND live_activity_devices.family_id=EXCLUDED.family_id
 AND live_activity_devices.registration_version<EXCLUDED.registration_version RETURNING registration_version`, req.InstallationID, p.AccountID, p.DeviceID, g.FamilyID, b.instance, p.RecoveryEpoch, req.RegistrationVersion, req.APNsEnvironment, r.topic, req.PushToStartToken, req.PayloadVersion, req.FeedBreakMinutes, p.SessionID, b.root, g.MembershipRevision, b.expires).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		var identical bool
		err = tx.QueryRow(ctx, `SELECT registration_version,NOT revoked AND registration_version=$7 AND environment=$8 AND topic=$9 AND push_token IS NOT DISTINCT FROM $10 AND payload_version=$11 AND feed_break_minutes=$12 AND lineage_root_session_id=$13 AND membership_revision=$14
 FROM live_activity_devices WHERE installation_id=$1 AND account_id=$2 AND device_id=$3 AND family_id=$4 AND instance_id=$5 AND recovery_epoch=$6`, req.InstallationID, p.AccountID, p.DeviceID, g.FamilyID, b.instance, p.RecoveryEpoch, req.RegistrationVersion, req.APNsEnvironment, r.topic, req.PushToStartToken, req.PayloadVersion, req.FeedBreakMinutes, b.root, g.MembershipRevision).Scan(&version, &identical)
		if errors.Is(err, pgx.ErrNoRows) {
			return protocol.LiveActivityRegistrationReceipt{}, ErrRegistrationDenied
		}
		if err == nil && !identical {
			return protocol.LiveActivityRegistrationReceipt{}, ErrVersionConflict
		}
	}
	if err != nil {
		return protocol.LiveActivityRegistrationReceipt{}, registrationError(err)
	}
	result := receipt(b, p.RecoveryEpoch, version)
	var registrationID string
	if err = tx.QueryRow(ctx, `SELECT registration_id::text FROM live_activity_devices WHERE installation_id=$1 AND family_id=$2 AND instance_id=$3 AND recovery_epoch=$4`, req.InstallationID, g.FamilyID, b.instance, p.RecoveryEpoch).Scan(&registrationID); err != nil {
		return result, err
	}
	targets, err := registrationTargets(ctx, tx, registrationID, req.ForegroundInventory)
	for _, target := range targets {
		if target.State.Status == "startUnconfirmed" && target.State.StartAttemptID != "" {
			result.UnconfirmedStarts = append(result.UnconfirmedStarts, protocol.LiveActivityUnconfirmedStart{Scope: protocol.LiveActivityScope{ServerInstanceID: b.instance, RecoveryEpoch: p.RecoveryEpoch, FamilyID: g.FamilyID, BabyID: target.BabyID, KindRaw: target.Kind}, GenerationID: target.State.GenerationID, AttemptID: target.State.StartAttemptID})
		}
	}
	return result, err
}
func (r *Repository) UnregisterDevice(ctx context.Context, tx pgx.Tx, p access.Principal, installationID string, version uint64) error {
	if !protocol.ValidUUID(installationID) || version == 0 || version > protocol.MaxSafeInteger {
		return ErrRegistrationDenied
	}
	var b registrationBinding
	var family string
	err := tx.QueryRow(ctx, `SELECT instance_id::text,lineage_root_session_id::text,authority_expires_at,app.current_family_id()::text FROM app.live_activity_session_binding($1,$2,$3,$4)`, p.AccountID, p.DeviceID, p.SessionID, p.RecoveryEpoch).Scan(&b.instance, &b.root, &b.expires, &family)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRegistrationDenied
	}
	if err != nil {
		return err
	}
	var fenced bool
	err = tx.QueryRow(ctx, `SELECT app.fence_live_activity_installation($1,$2,$3,$4,$5,$6,$7,$8,true)`, b.instance, p.RecoveryEpoch, installationID, p.AccountID, p.DeviceID, family, version, b.expires).Scan(&fenced)
	if err != nil {
		return registrationError(err)
	}
	if !fenced {
		return ErrVersionConflict
	}
	_, err = tx.Exec(ctx, `UPDATE live_activity_devices SET registration_version=$5,revoked=true WHERE installation_id=$1 AND account_id=$2 AND device_id=$3 AND recovery_epoch=$4 AND registration_version<$5`, installationID, p.AccountID, p.DeviceID, p.RecoveryEpoch, version)
	return registrationError(err)
}
func (r *Repository) RegisterActivity(ctx context.Context, tx pgx.Tx, p access.Principal, g access.Grant, req protocol.LiveActivityTokenRegistration) (protocol.LiveActivityRegistrationReceipt, error) {
	if err := req.Validate(); err != nil {
		return protocol.LiveActivityRegistrationReceipt{}, err
	}
	b, err := r.binding(ctx, tx, p, g)
	if err != nil {
		return protocol.LiveActivityRegistrationReceipt{}, err
	}
	if !req.Scope.MatchesRealm(b.instance, p.RecoveryEpoch, g.FamilyID) {
		return protocol.LiveActivityRegistrationReceipt{}, ErrRegistrationDenied
	}
	var id, env string
	var version uint64
	err = tx.QueryRow(ctx, `SELECT registration_id::text,environment,registration_version FROM live_activity_devices WHERE installation_id=$1 AND account_id=$2 AND device_id=$3 AND family_id=$4 AND instance_id=$5 AND recovery_epoch=$6 AND NOT revoked AND membership_revision=$7 AND lineage_root_session_id=$8 AND topic=$9 FOR UPDATE`, req.InstallationID, p.AccountID, p.DeviceID, g.FamilyID, b.instance, p.RecoveryEpoch, g.MembershipRevision, b.root, r.topic).Scan(&id, &env, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return protocol.LiveActivityRegistrationReceipt{}, ErrRegistrationDenied
	}
	if err != nil {
		return protocol.LiveActivityRegistrationReceipt{}, err
	}
	var baby bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM babies WHERE family_id=$1 AND id=$2)`, g.FamilyID, req.Scope.BabyID).Scan(&baby)
	if err != nil {
		return protocol.LiveActivityRegistrationReceipt{}, err
	}
	if !baby {
		return protocol.LiveActivityRegistrationReceipt{}, ErrRegistrationDenied
	}
	var canonical *string
	targets, err := registrationTargets(ctx, tx, id, nil)
	if err != nil {
		return protocol.LiveActivityRegistrationReceipt{}, err
	}
	for _, target := range targets {
		s := target.State
		if target.BabyID == req.Scope.BabyID && target.Kind == req.Scope.KindRaw && s.GenerationID != "" && s.Status != "terminal" && ((req.KnownGenerationID != nil && *req.KnownGenerationID == s.GenerationID) || referenceEqual(req.TimerID, s.TimerID) || (req.TimerID == nil && target.Kind == "feed" && referenceEqual(req.SessionID, s.SessionID))) {
			canonical = &s.GenerationID
			break
		}
	}
	var generation string
	// The server's own retirement of a card (an end it sent, a duplicate it
	// ended, a dead token) is sticky: the phone's later report that the card
	// ended or was dismissed is ActivityKit echoing that retirement, never the
	// receiver dismissing the lifecycle. Only a live card clears it.
	err = tx.QueryRow(ctx, `INSERT INTO live_activity_tokens(registration_id,account_id,family_id,activity_id,baby_id,kind,environment,topic,token,token_version,known_generation_id,timer_id,session_id,state,generation_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,coalesce($15::uuid,gen_random_uuid()))
 ON CONFLICT(registration_id,activity_id) DO UPDATE SET environment=EXCLUDED.environment,topic=EXCLUDED.topic,token=EXCLUDED.token,token_version=EXCLUDED.token_version,known_generation_id=EXCLUDED.known_generation_id,timer_id=EXCLUDED.timer_id,session_id=EXCLUDED.session_id,state=EXCLUDED.state,server_retired=live_activity_tokens.server_retired AND EXCLUDED.state<>'active',generation_id=coalesce($15::uuid,live_activity_tokens.generation_id)
 WHERE live_activity_tokens.token_version<EXCLUDED.token_version AND live_activity_tokens.baby_id=EXCLUDED.baby_id AND live_activity_tokens.kind=EXCLUDED.kind
 RETURNING generation_id::text`, id, p.AccountID, g.FamilyID, req.ActivityID, req.Scope.BabyID, req.Scope.KindRaw, env, r.topic, req.Token, req.TokenVersion, req.KnownGenerationID, req.TimerID, req.SessionID, req.State, canonical).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		var identical bool
		err = tx.QueryRow(ctx, `SELECT generation_id::text,(token=$3 OR (token IS NULL AND state<>'active' AND token_fingerprint=sha256(convert_to($3,'UTF8')))) AND token_version=$4 AND baby_id=$5 AND kind=$6 AND known_generation_id IS NOT DISTINCT FROM $7::uuid AND timer_id IS NOT DISTINCT FROM $8::uuid AND session_id IS NOT DISTINCT FROM $9::uuid AND state=$10 AND environment=$11 AND topic=$12 FROM live_activity_tokens WHERE registration_id=$1 AND activity_id=$2`, id, req.ActivityID, req.Token, req.TokenVersion, req.Scope.BabyID, req.Scope.KindRaw, req.KnownGenerationID, req.TimerID, req.SessionID, req.State, env, r.topic).Scan(&generation, &identical)
		if err == nil && !identical {
			return protocol.LiveActivityRegistrationReceipt{}, ErrVersionConflict
		}
	}
	if err != nil {
		return protocol.LiveActivityRegistrationReceipt{}, registrationError(err)
	}
	result := receipt(b, p.RecoveryEpoch, version)
	result.AcceptedTokenVersion = &req.TokenVersion
	result.Generations = append(result.Generations, protocol.LiveActivityGenerationMapping{ActivityID: req.ActivityID, Scope: req.Scope, GenerationID: generation})
	return result, nil
}

type registrationTarget struct {
	BabyID string      `json:"baby_id"`
	Kind   string      `json:"kind"`
	State  TargetState `json:"state"`
}

func registrationTargets(ctx context.Context, tx pgx.Tx, id string, inventory *protocol.LiveActivityForegroundInventory) ([]registrationTarget, error) {
	var proof []byte
	if inventory != nil {
		proof, _ = json.Marshal(inventory)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT app.live_activity_registration_state($1,$2)`, id, proof).Scan(&raw); err != nil {
		return nil, registrationError(err)
	}
	if raw == nil {
		return nil, ErrRegistrationDenied
	}
	var targets []registrationTarget
	err := json.Unmarshal(raw, &targets)
	return targets, err
}
