package family

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/identity"
	"github.com/jackc/pgx/v5"
)

// Options configures a Service. Role policy (Allowed), the family_sync
// entitlement and the caregiver and invitation caps are the authority's
// options: the authority evaluates them inside its transactions.
type Options struct {
	Authority      *authority.Service
	InvitationKeys identity.DigestKeyring
	Origin         string
	Now            func() time.Time
	InvitationTTL  time.Duration
}

// Service is the family surface: it shapes requests into authority
// commands and reads, and maps their codes to the family's legacy codes.
type Service struct {
	authority      *authority.Service
	invitationKeys identity.DigestKeyring
	origin         string
	now            func() time.Time
	invitationTTL  time.Duration
}

func New(options Options) (*Service, error) {
	if options.Authority == nil || options.Now == nil || !validKeyring(options.InvitationKeys) || !validOrigin(options.Origin) || options.InvitationTTL <= 0 {
		return nil, errors.New("family service requires authority, invitation keys, HTTPS origin, clock, and TTL")
	}
	return &Service{authority: options.Authority, invitationKeys: options.InvitationKeys, origin: options.Origin, now: options.Now,
		invitationTTL: options.InvitationTTL}, nil
}

func validKeyring(ring identity.DigestKeyring) bool {
	if !validKey(ring.Current) || len(ring.Previous) > 7 {
		return false
	}
	ids := map[string]bool{ring.Current.ID: true}
	secrets := map[[32]byte]bool{ring.Current.Secret: true}
	for _, key := range ring.Previous {
		if !validKey(key) || ids[key.ID] || secrets[key.Secret] {
			return false
		}
		ids[key.ID], secrets[key.Secret] = true, true
	}
	return true
}

func validKey(key identity.DigestKey) bool {
	if key.ID == "" || len(key.ID) > 64 || key.Secret == ([32]byte{}) {
		return false
	}
	for index, character := range key.ID {
		if (character >= 'a' && character <= 'z') || (index > 0 && character >= '0' && character <= '9') || (index > 0 && (character == '.' || character == '_' || character == '-')) {
			continue
		}
		return false
	}
	return true
}

func validOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Path == "" && !strings.HasSuffix(raw, "/")
}

func validDisplayName(value string) bool {
	if value != strings.TrimSpace(value) || !utf8.ValidString(value) || utf8.RuneCountInString(value) < 1 || utf8.RuneCountInString(value) > 80 || len(value) > 320 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validUUID(value string) bool {
	return canonicalUUID.MatchString(value) && value != "00000000-0000-0000-0000-000000000000"
}

func deterministicID(domain, actorID, commandID string) (string, error) {
	if !validUUID(actorID) || !validUUID(commandID) {
		return "", ErrInvalidRequest
	}
	hash := sha256.New()
	for _, field := range []string{domain, actorID, commandID} {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		hash.Write(size[:])
		hash.Write([]byte(field))
	}
	digest := hash.Sum(nil)
	value := digest[:16]
	value[6] = (value[6] & 0x0f) | 0x80
	value[8] = (value[8] & 0x3f) | 0x80
	raw := hex.EncodeToString(value)
	return fmt.Sprintf("%s-%s-%s-%s-%s", raw[:8], raw[8:12], raw[12:16], raw[16:20], raw[20:]), nil
}

func FamilyID(actorID, commandID string) (string, error) {
	return deterministicID("baby-tracker-family-id-v1", actorID, commandID)
}
func InvitationID(actorID, commandID string) (string, error) {
	return deterministicID("baby-tracker-invitation-id-v1", actorID, commandID)
}

// withFamilyRead runs run in a read transaction over the caller's family as
// the authority grants it for action. These reads named the family by the
// caller's active slot in the legacy service, so a caller whose slot is a
// family_recovery reservation (or whose session is recovery scoped) is
// family_unavailable here, as it was there.
func (service *Service) withFamilyRead(ctx context.Context, principal access.Principal, action string, run func(pgx.Tx, access.Grant) error) error {
	if service == nil || service.authority == nil {
		return ErrUnavailable
	}
	err := service.authority.WithFamilyReadTx(ctx, principal, action, run)
	if authority.CodeOf(err) == "recovery_only" {
		return ErrFamilyUnavailable
	}
	return familyErrorOrNil(err)
}

func (service *Service) Account(ctx context.Context, principal access.Principal) (AccountView, error) {
	if service == nil || service.authority == nil {
		return AccountView{}, ErrUnavailable
	}
	view, err := readAccount(ctx, service.authority, principal)
	if err != nil {
		return AccountView{}, familyError(err)
	}
	return view, nil
}

func (service *Service) UpdateProfile(ctx context.Context, principal access.Principal, commandID, displayName string, confirmed bool) (AccountView, error) {
	if !confirmed {
		return AccountView{}, ErrConfirmationRequired
	}
	if !validUUID(commandID) || !validDisplayName(displayName) {
		return AccountView{}, ErrInvalidRequest
	}
	if service == nil || service.authority == nil {
		return AccountView{}, ErrUnavailable
	}
	// A command ID the caller already spent on a security command is never
	// a profile command (the profile table keeps its own receipts).
	if validUUID(principal.AccountID) {
		if _, found, err := service.authority.CommandReceipt(ctx, principal.AccountID, commandID); err != nil {
			return AccountView{}, familyError(err)
		} else if found {
			return AccountView{}, ErrCommandIDReused
		}
	}
	if err := updateProfile(ctx, service.authority, principal, commandID, displayName, service.now().UTC()); err != nil {
		return AccountView{}, familyError(err)
	}
	return service.Account(ctx, principal)
}

func (service *Service) Create(ctx context.Context, principal access.Principal, commandID, displayName string) (FamilyView, error) {
	if service == nil || !validDisplayName(displayName) {
		return FamilyView{}, ErrInvalidRequest
	}
	familyID, err := FamilyID(principal.AccountID, commandID)
	if err != nil {
		return FamilyView{}, err
	}
	if service.authority == nil {
		return FamilyView{}, ErrUnavailable
	}
	result, err := service.authority.CreateFamily(ctx, authority.CreateFamily{CommandID: commandID, OwnerAccountID: principal.AccountID,
		FamilyID: familyID, DisplayName: displayName, Principal: principal})
	if err != nil {
		return FamilyView{}, familyError(err)
	}
	return service.familyAfterReceipt(ctx, principal, result.FamilyID)
}

// familyAfterReceipt reads the family a committed (or exactly retried)
// command names, provided it is still the caller's family: a receipt never
// returns a family the caller has since left. An empty expectedFamilyID
// accepts the caller's current family.
func (service *Service) familyAfterReceipt(ctx context.Context, principal access.Principal, expectedFamilyID string) (FamilyView, error) {
	var view FamilyView
	err := service.withFamilyRead(ctx, principal, "read_receipt", func(tx pgx.Tx, grant access.Grant) error {
		if expectedFamilyID != "" && grant.FamilyID != expectedFamilyID {
			return ErrFamilyUnavailable
		}
		var err error
		view, err = readFamily(ctx, tx, grant.FamilyID)
		return err
	})
	if err != nil {
		return FamilyView{}, err
	}
	return view, nil
}

func (service *Service) RepairProfile(ctx context.Context, principal access.Principal, commandID, displayName string, confirmed bool) (FamilyView, error) {
	if !confirmed {
		return FamilyView{}, ErrConfirmationRequired
	}
	if !validUUID(commandID) || !validDisplayName(displayName) {
		return FamilyView{}, ErrInvalidRequest
	}
	if service == nil || service.authority == nil {
		return FamilyView{}, ErrUnavailable
	}
	if _, err := service.authority.RepairFamilyProfile(ctx, authority.RepairFamilyProfile{CommandID: commandID, OwnerAccountID: principal.AccountID,
		DisplayName: displayName, Principal: principal}); err != nil {
		return FamilyView{}, familyError(err)
	}
	return service.familyAfterReceipt(ctx, principal, "")
}

func (service *Service) ListActors(ctx context.Context, principal access.Principal, after, highWater string, limit int) (ActorLabelPage, error) {
	if limit < 1 || limit > 100 || (after != "" && !validUUID(after)) || !validHighWater(highWater) ||
		(after != "" && highWater == "") {
		return ActorLabelPage{}, ErrInvalidRequest
	}
	var page ActorLabelPage
	err := service.withFamilyRead(ctx, principal, "read", func(tx pgx.Tx, grant access.Grant) error {
		var err error
		page, err = readActorLabels(ctx, tx, grant.FamilyID, after, highWater, limit)
		return err
	})
	if err != nil {
		return ActorLabelPage{}, err
	}
	return page, nil
}

func validHighWater(value string) bool {
	if value == "" {
		return true
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	return err == nil && parsed > 0 && parsed <= 9007199254740991 && strconv.FormatUint(parsed, 10) == value
}

func (service *Service) Current(ctx context.Context, principal access.Principal) (FamilyView, error) {
	var view FamilyView
	err := service.withFamilyRead(ctx, principal, "read", func(tx pgx.Tx, grant access.Grant) error {
		var err error
		view, err = readFamily(ctx, tx, grant.FamilyID)
		return err
	})
	if err != nil {
		return FamilyView{}, err
	}
	return view, nil
}

// Invite mints a link that confers `role` when it is redeemed.
//
// An empty role means caregiver. The decision is made when the link is
// created rather than when a stranger redeems it, because the person who
// knows what access to hand out is the one writing the message, not the one
// receiving it — and because a redeemer who could choose their own role would
// not be a viewer for long.
//
// An exact retry returns the stored invitation without its secret
// (SecretUnavailable): only the first attempt's caller ever held the token.
// The retry is recognised before the caller's current slot matters, so it is
// answered for the family the original command named.
func (service *Service) Invite(ctx context.Context, principal access.Principal, commandID, role string) (InvitationSecret, error) {
	if role == "" {
		role = "caregiver"
	}
	if role != "caregiver" && role != "viewer" {
		return InvitationSecret{}, ErrInvalidRequest
	}
	invitationID, err := InvitationID(principal.AccountID, commandID)
	if err != nil {
		return InvitationSecret{}, err
	}
	if service == nil || service.authority == nil {
		return InvitationSecret{}, ErrUnavailable
	}
	receipt, found, err := service.authority.CommandReceipt(ctx, principal.AccountID, commandID)
	if err != nil {
		return InvitationSecret{}, familyError(err)
	}
	var familyID string
	if found {
		if receipt.Kind != authority.KindInvitationCreated || len(receipt.FamilyIDs) != 1 {
			return InvitationSecret{}, ErrCommandIDReused
		}
		familyID = receipt.FamilyIDs[0]
	} else {
		slot, err := service.authority.FamilySlot(ctx, principal.AccountID)
		if err != nil {
			return InvitationSecret{}, familyError(err)
		}
		if slot == nil || slot.State != "active" {
			return InvitationSecret{}, ErrFamilyUnavailable
		}
		familyID = slot.FamilyID
	}
	token, err := newInvitationToken()
	if err != nil {
		return InvitationSecret{}, ErrUnavailable
	}
	digest, err := invitationDigest(service.invitationKeys.Current, token)
	if err != nil {
		return InvitationSecret{}, err
	}
	expiresAt := service.now().UTC().Truncate(time.Microsecond).Add(service.invitationTTL)
	result, err := service.authority.CreateInvitation(ctx, authority.CreateInvitation{CommandID: commandID, CreatorAccountID: principal.AccountID,
		InvitationID: invitationID, FamilyID: familyID, TokenDigest: digest, Role: role, ExpiresAt: expiresAt, Principal: principal})
	if err != nil {
		return InvitationSecret{}, familyError(err)
	}
	stored := result.Invitation
	view := InvitationView{ID: stored.ID, FamilyID: stored.FamilyID, Role: stored.Role, Status: stored.Status,
		ExpiresAt: stored.ExpiresAt.UTC(), CreatedAt: stored.CreatedAt.UTC()}
	if result.SecretUnavailable {
		return InvitationSecret{Invitation: view, SecretUnavailable: true}, nil
	}
	return InvitationSecret{Invitation: view, Token: token, Link: service.origin + "/invite#token=" + token}, nil
}

// lookupInvitation resolves an invitation token for its invitee. The lookup
// crosses families, so it runs only for a caller whose session is live (the
// legacy lookup ran in the caller's account context): the session is checked
// first, then the invitation of an active family is found by the token's
// digest under every invitation key (more than one match is
// authorization_quarantine).
func (service *Service) lookupInvitation(ctx context.Context, principal access.Principal, token string) (authority.InvitationView, []string, error) {
	if !validInvitationToken(token) {
		return authority.InvitationView{}, nil, ErrInvitationInvalid
	}
	candidates, err := invitationCandidates(service.invitationKeys, token)
	if err != nil {
		return authority.InvitationView{}, nil, err
	}
	if service.authority == nil {
		return authority.InvitationView{}, nil, ErrUnavailable
	}
	if err := service.authority.WithAccountTx(ctx, principal, func(pgx.Tx) error { return nil }); err != nil {
		return authority.InvitationView{}, nil, familyError(err)
	}
	invitation, found, err := service.authority.LookupInvitation(ctx, candidates)
	if err != nil {
		return authority.InvitationView{}, nil, familyError(err)
	}
	if !found {
		return authority.InvitationView{}, nil, ErrInvitationInvalid
	}
	return invitation, candidates, nil
}

func (service *Service) Preview(ctx context.Context, principal access.Principal, token string) (InvitationPreview, error) {
	if service == nil {
		return InvitationPreview{}, ErrUnavailable
	}
	invitation, _, err := service.lookupInvitation(ctx, principal, token)
	if err != nil {
		return InvitationPreview{}, err
	}
	switch invitation.Status {
	case "redeemed":
		return InvitationPreview{}, ErrInvitationConsumed
	case "revoked":
		return InvitationPreview{}, ErrInvitationRevoked
	case "active":
	default:
		return InvitationPreview{}, ErrInvitationInvalid
	}
	if !service.now().UTC().Before(invitation.ExpiresAt) {
		return InvitationPreview{}, ErrInvitationExpired
	}
	return InvitationPreview{FamilyDisplayName: invitation.FamilyDisplayName, ExpiresAt: invitation.ExpiresAt.UTC()}, nil
}

func (service *Service) Redeem(ctx context.Context, principal access.Principal, commandID, token string, confirmed bool) (FamilyView, error) {
	if !confirmed {
		return FamilyView{}, ErrConfirmationRequired
	}
	if !validUUID(commandID) {
		return FamilyView{}, ErrInvalidRequest
	}
	if service == nil {
		return FamilyView{}, ErrUnavailable
	}
	_, candidates, err := service.lookupInvitation(ctx, principal, token)
	if err != nil {
		return FamilyView{}, err
	}
	result, err := service.authority.RedeemInvitation(ctx, authority.RedeemInvitation{CommandID: commandID, RedeemerAccountID: principal.AccountID,
		TokenDigestCandidates: candidates, Principal: principal})
	if err != nil {
		return FamilyView{}, familyError(err)
	}
	return service.familyAfterReceipt(ctx, principal, result.FamilyID)
}

func (service *Service) Invitation(ctx context.Context, principal access.Principal, invitationID string) (InvitationView, error) {
	if !validUUID(invitationID) {
		return InvitationView{}, ErrInvitationInvalid
	}
	var view InvitationView
	err := service.withFamilyRead(ctx, principal, "manage_members", func(tx pgx.Tx, grant access.Grant) error {
		found := false
		var err error
		view, found, err = readInvitation(ctx, tx, grant.FamilyID, invitationID)
		if err == nil && !found {
			return ErrInvitationInvalid
		}
		return err
	})
	if err != nil {
		return InvitationView{}, err
	}
	return view, nil
}

func (service *Service) PendingInvitations(ctx context.Context, principal access.Principal) ([]InvitationView, error) {
	var views []InvitationView
	err := service.withFamilyRead(ctx, principal, "manage_members", func(tx pgx.Tx, grant access.Grant) error {
		var err error
		views, err = readPendingInvitations(ctx, tx, grant.FamilyID, service.now().UTC())
		return err
	})
	if err != nil {
		return nil, err
	}
	return views, nil
}

func (service *Service) RevokeInvitation(ctx context.Context, principal access.Principal, commandID, invitationID string) (authority.Result, error) {
	if !validUUID(commandID) || !validUUID(invitationID) {
		return authority.Result{}, ErrInvalidRequest
	}
	if service == nil || service.authority == nil {
		return authority.Result{}, ErrUnavailable
	}
	return commandResult(service.authority.RevokeInvitation(ctx, authority.RevokeInvitation{CommandID: commandID, ActorAccountID: principal.AccountID,
		InvitationID: invitationID, Principal: principal}))
}

func (service *Service) Transfer(ctx context.Context, principal access.Principal, commandID, newOwnerID string) (authority.Result, error) {
	if !validUUID(commandID) || !validUUID(newOwnerID) {
		return authority.Result{}, ErrInvalidRequest
	}
	if service == nil || service.authority == nil {
		return authority.Result{}, ErrUnavailable
	}
	return commandResult(service.authority.TransferOwner(ctx, authority.TransferOwner{CommandID: commandID, OldOwnerID: principal.AccountID,
		NewOwnerID: newOwnerID, Principal: principal}))
}

// ChangeRole moves a caregiver between writing and view-only.
//
// Owner-only, and refused for the owner's own row: a family must always have
// exactly one owner, and demoting yourself here would leave it with none.
// Stepping down is Transfer, which names a successor in the same command.
func (service *Service) ChangeRole(ctx context.Context, principal access.Principal, commandID, accountID, role string) (authority.Result, error) {
	if !validUUID(commandID) || !validUUID(accountID) || (role != "caregiver" && role != "viewer") {
		return authority.Result{}, ErrInvalidRequest
	}
	if service == nil || service.authority == nil {
		return authority.Result{}, ErrUnavailable
	}
	return commandResult(service.authority.ChangeRole(ctx, authority.ChangeRole{CommandID: commandID, OwnerAccountID: principal.AccountID,
		AccountID: accountID, Role: role, Principal: principal}))
}

func (service *Service) Remove(ctx context.Context, principal access.Principal, commandID, accountID string) (authority.Result, error) {
	if !validUUID(commandID) || !validUUID(accountID) {
		return authority.Result{}, ErrInvalidRequest
	}
	if service == nil || service.authority == nil {
		return authority.Result{}, ErrUnavailable
	}
	return commandResult(service.authority.RemoveMember(ctx, authority.RemoveMember{CommandID: commandID, OwnerAccountID: principal.AccountID,
		AccountID: accountID, Principal: principal}))
}

func (service *Service) Leave(ctx context.Context, principal access.Principal, commandID string) (authority.Result, error) {
	if !validUUID(commandID) {
		return authority.Result{}, ErrInvalidRequest
	}
	if service == nil || service.authority == nil {
		return authority.Result{}, ErrUnavailable
	}
	return commandResult(service.authority.LeaveFamily(ctx, authority.LeaveFamily{CommandID: commandID, AccountID: principal.AccountID, Principal: principal}))
}

// commandResult maps an authority command's error to the family codes.
func commandResult(result authority.Result, err error) (authority.Result, error) {
	if err != nil {
		return authority.Result{}, familyError(err)
	}
	return result, nil
}
