package family

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
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
	"babytracker/server/internal/identity"
	"babytracker/server/internal/security"
)

type Options struct {
	Security                      *security.Service
	Repository                    *Repository
	Entitlements                  EntitlementPolicy
	InvitationKeys                identity.DigestKeyring
	Origin                        string
	Now                           func() time.Time
	MaxCaregivers, MaxInvitations int
	InvitationTTL                 time.Duration
}

type Service struct {
	security                      *security.Service
	repository                    *Repository
	entitlements                  EntitlementPolicy
	invitationKeys                identity.DigestKeyring
	origin                        string
	now                           func() time.Time
	maxCaregivers, maxInvitations int
	invitationTTL                 time.Duration
}

func New(options Options) (*Service, error) {
	if options.Security == nil || options.Repository == nil || options.Entitlements == nil || options.Now == nil ||
		!validKeyring(options.InvitationKeys) || !validOrigin(options.Origin) || options.MaxCaregivers <= 0 ||
		options.MaxInvitations <= 0 || options.InvitationTTL <= 0 {
		return nil, errors.New("family service requires authority, repository, entitlement policy, invitation keys, HTTPS origin, clock, positive caps, and TTL")
	}
	return &Service{security: options.Security, repository: options.Repository, entitlements: options.Entitlements,
		invitationKeys: options.InvitationKeys, origin: options.Origin, now: options.Now,
		maxCaregivers: options.MaxCaregivers, maxInvitations: options.MaxInvitations, invitationTTL: options.InvitationTTL}, nil
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

func (service *Service) Authorize(ctx context.Context, principal access.Principal, familyID, action string) (access.Grant, error) {
	if service == nil || service.security == nil || familyID == "" ||
		!allowed("owner", action) && !allowed("caregiver", action) && !allowed("viewer", action) {
		return access.Grant{}, ErrMembershipRevoked
	}
	var account security.AccountView
	var accountOK bool
	var slot security.FamilySlotView
	var slotOK bool
	var family security.FamilyView
	var familyOK bool
	var authorityMembership security.MembershipView
	var authorityMembershipExists bool
	var authorityHead security.Head
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		account, accountOK = view.Account(principal.AccountID)
		slot, slotOK = view.FamilySlot(principal.AccountID)
		family, familyOK = view.Family(familyID)
		authorityMembership, authorityMembershipExists = view.Membership(familyID, principal.AccountID)
		authorityHead = view.Head()
		return nil
	})
	if err != nil {
		return access.Grant{}, familyError(err)
	}
	membership, memberOK := authorityMembership, authorityMembershipExists
	if !accountOK || account.Status != "active" || !slotOK || slot.FamilyID != familyID || slot.State != "active" || !familyOK || family.Status != "active" ||
		!memberOK || membership.Status != "active" || !allowed(membership.Role, action) {
		if slotOK && slot.FamilyID == familyID && slot.State == "family_recovery" {
			return access.Grant{}, ErrRecoveryOnly
		}
		if memberOK && membership.Role != "owner" && !allowed(membership.Role, action) {
			// A view-only caregiver refused something an ordinary caregiver
			// could do is not missing ownership — they are missing write
			// access, and telling them "owner required" sends them looking for
			// a transfer that would not help and that nobody meant to offer.
			//
			// `membership.go` has always said record mutations get the precise
			// code from inside the sync transaction. They did not: this gate
			// runs first, and its answer is the one the client sees. The inner
			// gate still exists and still matters — it is what catches a
			// replayed mutation that never passed through here.
			if membership.Role == "viewer" && allowed("caregiver", action) {
				return access.Grant{}, ErrReadOnlyCaregiver
			}
			return access.Grant{}, ErrOwnerRequired
		}
		return access.Grant{}, ErrMembershipRevoked
	}
	if !entitlementExempt(action) {
		if err := service.entitlements.Check(ctx, familyID, "family_sync", service.now().UTC()); err != nil {
			return access.Grant{}, err
		}
	}
	return access.Grant{AccountID: principal.AccountID, FamilyID: familyID, Role: membership.Role, Revision: authorityHead.Revision, MembershipRevision: membership.Generation}, nil
}

func (service *Service) Account(ctx context.Context, principal access.Principal) (AccountView, error) {
	if service == nil || service.repository == nil {
		return AccountView{}, ErrUnavailable
	}
	view, err := service.repository.account(ctx, service.security, principal)
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
	var receiptExists bool
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		_, receiptExists = view.Receipt(principal.AccountID, commandID)
		return nil
	})
	if err != nil {
		return AccountView{}, familyError(err)
	}
	if receiptExists {
		return AccountView{}, ErrCommandIDReused
	}
	if err := service.repository.updateProfile(ctx, service.security, principal, commandID, displayName, service.now().UTC()); err != nil {
		return AccountView{}, familyError(err)
	}
	return service.Account(ctx, principal)
}

type familyMaterial struct {
	Version, Kind, FamilyID, ActorAccountID, DisplayName string
}

func encodeFamilyMaterial(kind, familyID, accountID, displayName string) ([]byte, [32]byte, error) {
	material := struct {
		Version        string `json:"version"`
		Kind           string `json:"kind"`
		FamilyID       string `json:"family_id"`
		ActorAccountID string `json:"actor_account_id"`
		DisplayName    string `json:"display_name"`
	}{"v1", kind, familyID, accountID, displayName}
	encoded, err := json.Marshal(material)
	if err != nil {
		return nil, [32]byte{}, err
	}
	return encoded, sha256.Sum256(encoded), nil
}

func (service *Service) Create(ctx context.Context, principal access.Principal, commandID, displayName string) (FamilyView, error) {
	if service == nil || !validDisplayName(displayName) {
		return FamilyView{}, ErrInvalidRequest
	}
	familyID, err := FamilyID(principal.AccountID, commandID)
	if err != nil {
		return FamilyView{}, err
	}
	material, materialDigest, err := encodeFamilyMaterial("family_create", familyID, principal.AccountID, displayName)
	if err != nil {
		return FamilyView{}, ErrUnavailable
	}
	now := service.now().UTC()
	kind := security.EventKind("family_created")
	var payload any = security.FamilyCreatedPayload{Version: "v1", FamilyID: familyID, OwnerAccountID: principal.AccountID, ProjectionMaterialDigest: hex.EncodeToString(materialDigest[:])}
	if hosted, ok := service.entitlements.(hostedBootstrapPolicy); ok && hosted.hostedBootstrap() {
		kind = "family_bootstrapped"
		payload = security.FamilyBootstrappedPayload{Version: "v1", FamilyID: familyID, OwnerAccountID: principal.AccountID,
			EntitlementKey: "family_sync", EntitlementSource: "hosted_beta",
			ProjectionMaterialDigest: hex.EncodeToString(materialDigest[:])}
	}
	rawPayload, _ := json.Marshal(payload)
	command := security.Command{ID: commandID, Kind: kind, AffectedAccountIDs: []string{principal.AccountID}, AffectedFamilyIDs: []string{familyID}, Payload: rawPayload}
	commandHash, err := security.CanonicalCommandHash(principal, command)
	if err != nil {
		return FamilyView{}, familyError(err)
	}
	var receipt security.ReceiptView
	var receiptExists bool
	err = service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		receipt, receiptExists = view.Receipt(principal.AccountID, commandID)
		return nil
	})
	if err != nil {
		return FamilyView{}, familyError(err)
	}
	if receiptExists {
		if receipt.CommandHash != commandHash {
			return FamilyView{}, ErrCommandIDReused
		}
		return service.familyAfterReceipt(ctx, principal, familyID)
	}
	if err := service.repository.StageFamilyMaterial(ctx, FamilyMaterialStage{CommandID: commandID, CommandHash: commandHash,
		ProjectionMaterialDigest: materialDigest, Material: material, AccountID: principal.AccountID, Now: now}); err != nil {
		return service.replayFamilyView(ctx, principal, familyID, commandID, kind, commandHash)
	}
	if _, err := service.security.Execute(ctx, principal, command); err != nil {
		if replay, replayErr := service.replayFamilyView(ctx, principal, familyID, commandID, kind, commandHash); replayErr == nil || replayErr == ErrCommandIDReused {
			return replay, replayErr
		}
		return FamilyView{}, familyError(err)
	}
	return service.familyAfterReceipt(ctx, principal, familyID)
}

func (service *Service) replayFamilyView(ctx context.Context, principal access.Principal, expectedFamilyID, commandID string, kind security.EventKind, commandHash [32]byte) (FamilyView, error) {
	var receipt security.ReceiptView
	var exists bool
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		receipt, exists = view.Receipt(principal.AccountID, commandID)
		return nil
	})
	if err != nil {
		return FamilyView{}, familyError(err)
	}
	if !exists {
		return FamilyView{}, ErrUnavailable
	}
	if receipt.Kind != kind || receipt.CommandHash != commandHash {
		return FamilyView{}, ErrCommandIDReused
	}
	return service.familyAfterReceipt(ctx, principal, expectedFamilyID)
}

func (service *Service) familyAfterReceipt(ctx context.Context, principal access.Principal, expectedFamilyID string) (FamilyView, error) {
	var slot security.FamilySlotView
	var ok bool
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		slot, ok = view.FamilySlot(principal.AccountID)
		return nil
	})
	if err != nil {
		return FamilyView{}, familyError(err)
	}
	if !ok || slot.State != "active" || slot.FamilyID != expectedFamilyID {
		return FamilyView{}, ErrFamilyUnavailable
	}
	view, err := service.repository.family(ctx, service.security, principal, expectedFamilyID, "read_receipt")
	if err != nil {
		return FamilyView{}, familyError(err)
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
	var slot security.FamilySlotView
	var ok bool
	var receipt security.ReceiptView
	var receiptExists bool
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		slot, ok = view.FamilySlot(principal.AccountID)
		receipt, receiptExists = view.Receipt(principal.AccountID, commandID)
		return nil
	})
	if err != nil {
		return FamilyView{}, familyError(err)
	}
	if !ok || slot.State != "active" {
		return FamilyView{}, ErrFamilyUnavailable
	}
	material, materialDigest, err := encodeFamilyMaterial("family_profile_repair", slot.FamilyID, principal.AccountID, displayName)
	if err != nil {
		return FamilyView{}, ErrUnavailable
	}
	payload := security.FamilyProfileRepairedPayload{Version: "v1", FamilyID: slot.FamilyID, OwnerAccountID: principal.AccountID,
		ProjectionMaterialDigest: hex.EncodeToString(materialDigest[:])}
	raw, _ := json.Marshal(payload)
	command := security.Command{ID: commandID, Kind: "family_profile_repaired", AffectedAccountIDs: []string{principal.AccountID},
		AffectedFamilyIDs: []string{slot.FamilyID}, Payload: raw}
	commandHash, err := security.CanonicalCommandHash(principal, command)
	if err != nil {
		return FamilyView{}, familyError(err)
	}
	if receiptExists {
		if receipt.Kind != command.Kind || receipt.CommandHash != commandHash {
			return FamilyView{}, ErrCommandIDReused
		}
		return service.familyAfterReceipt(ctx, principal, slot.FamilyID)
	}
	if _, err := service.Authorize(ctx, principal, slot.FamilyID, "repair_profile"); err != nil {
		return FamilyView{}, err
	}
	now := service.now().UTC()
	if err := service.repository.StageFamilyMaterial(ctx, FamilyMaterialStage{CommandID: commandID, CommandHash: commandHash,
		ProjectionMaterialDigest: materialDigest, Material: material, AccountID: principal.AccountID, Now: now}); err != nil {
		if errors.Is(err, ErrProfileNotRequired) {
			return FamilyView{}, ErrProfileNotRequired
		}
		return service.replayFamilyView(ctx, principal, slot.FamilyID, commandID, command.Kind, commandHash)
	}
	if _, err := service.security.Execute(ctx, principal, command); err != nil {
		if replay, replayErr := service.replayFamilyView(ctx, principal, slot.FamilyID, commandID, command.Kind, commandHash); replayErr == nil || replayErr == ErrCommandIDReused {
			return replay, replayErr
		}
		return FamilyView{}, familyError(err)
	}
	return service.familyAfterReceipt(ctx, principal, slot.FamilyID)
}

func (service *Service) ListActors(ctx context.Context, principal access.Principal, after, highWater string, limit int) (ActorLabelPage, error) {
	if limit < 1 || limit > 100 || (after != "" && !validUUID(after)) || !validHighWater(highWater) ||
		(after != "" && highWater == "") {
		return ActorLabelPage{}, ErrInvalidRequest
	}
	var slot security.FamilySlotView
	var ok bool
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		slot, ok = view.FamilySlot(principal.AccountID)
		return nil
	})
	if err != nil {
		return ActorLabelPage{}, familyError(err)
	}
	if !ok || slot.State != "active" {
		return ActorLabelPage{}, ErrFamilyUnavailable
	}
	page, err := service.repository.actorLabels(ctx, service.security, principal, slot.FamilyID, after, highWater, limit)
	if err != nil {
		return ActorLabelPage{}, familyError(err)
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
	var slot security.FamilySlotView
	var ok bool
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		slot, ok = view.FamilySlot(principal.AccountID)
		return nil
	})
	if err != nil {
		return FamilyView{}, familyError(err)
	}
	if !ok || slot.State != "active" {
		return FamilyView{}, ErrFamilyUnavailable
	}
	view, err := service.repository.family(ctx, service.security, principal, slot.FamilyID, "read")
	if err != nil {
		return FamilyView{}, familyError(err)
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
	var receipt security.ReceiptView
	var receiptExists bool
	var authorityInvitation security.InvitationView
	var authorityInvitationExists bool
	var slot security.FamilySlotView
	var ok bool
	err = service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		receipt, receiptExists = view.Receipt(principal.AccountID, commandID)
		authorityInvitation, authorityInvitationExists = view.Invitation(invitationID)
		slot, ok = view.FamilySlot(principal.AccountID)
		return nil
	})
	if err != nil {
		return InvitationSecret{}, familyError(err)
	}
	if receiptExists {
		invitation, invitationExists := authorityInvitation, authorityInvitationExists
		if receipt.Kind != "invitation_created" || !invitationExists || receipt.Result.Revision == 0 {
			return InvitationSecret{}, ErrCommandIDReused
		}
		expiresAt, parseErr := time.Parse(time.RFC3339Nano, invitation.ExpiresAt)
		if parseErr != nil {
			return InvitationSecret{}, ErrUnavailable
		}
		return InvitationSecret{Invitation: InvitationView{ID: invitationID, FamilyID: invitation.FamilyID, Role: invitation.Role, Status: invitation.Status,
			ExpiresAt: expiresAt, CreatedAt: expiresAt.Add(-service.invitationTTL)}, SecretUnavailable: true}, nil
	}
	if !ok || slot.State != "active" {
		return InvitationSecret{}, ErrFamilyUnavailable
	}
	token, err := newInvitationToken()
	if err != nil {
		return InvitationSecret{}, ErrUnavailable
	}
	digest, err := invitationDigest(service.invitationKeys.Current, token)
	if err != nil {
		return InvitationSecret{}, err
	}
	now := service.now().UTC().Truncate(time.Microsecond)
	expiresAt := now.Add(service.invitationTTL)
	payload := security.InvitationPayload{Version: "v1", InvitationID: invitationID, FamilyID: slot.FamilyID, CreatorAccountID: principal.AccountID,
		TokenDigest: digest, Role: role, Status: "active", ExpiresAt: expiresAt.Format(time.RFC3339Nano)}
	raw, _ := json.Marshal(payload)
	command := security.Command{ID: commandID, Kind: "invitation_created", AffectedAccountIDs: []string{principal.AccountID}, AffectedFamilyIDs: []string{slot.FamilyID}, Payload: raw}
	if _, err := service.security.Execute(ctx, principal, command); err != nil {
		if replay, replayErr := service.replayInvitation(ctx, principal, commandID, invitationID); replayErr == nil || replayErr == ErrCommandIDReused {
			return replay, replayErr
		}
		return InvitationSecret{}, familyError(err)
	}
	return InvitationSecret{Invitation: InvitationView{ID: invitationID, FamilyID: slot.FamilyID, Role: role, Status: "active", ExpiresAt: expiresAt, CreatedAt: now},
		Token: token, Link: service.origin + "/invite#token=" + token}, nil
}

func (service *Service) replayInvitation(ctx context.Context, principal access.Principal, commandID, invitationID string) (InvitationSecret, error) {
	var receipt security.ReceiptView
	var exists bool
	var invitation security.InvitationView
	var invitationExists bool
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		receipt, exists = view.Receipt(principal.AccountID, commandID)
		invitation, invitationExists = view.Invitation(invitationID)
		return nil
	})
	if err != nil {
		return InvitationSecret{}, familyError(err)
	}
	if !exists {
		return InvitationSecret{}, ErrUnavailable
	}
	if receipt.Kind != "invitation_created" || !invitationExists {
		return InvitationSecret{}, ErrCommandIDReused
	}
	expiresAt, parseErr := time.Parse(time.RFC3339Nano, invitation.ExpiresAt)
	if parseErr != nil {
		return InvitationSecret{}, ErrUnavailable
	}
	return InvitationSecret{Invitation: InvitationView{ID: invitationID, FamilyID: invitation.FamilyID, Role: invitation.Role, Status: invitation.Status,
		ExpiresAt: expiresAt, CreatedAt: expiresAt.Add(-service.invitationTTL)}, SecretUnavailable: true}, nil
}

func (service *Service) lookupInvitation(ctx context.Context, principal access.Principal, token string) (invitationLookup, string, error) {
	if !validInvitationToken(token) {
		return invitationLookup{}, "", ErrInvitationInvalid
	}
	candidates, err := invitationCandidates(service.invitationKeys, token)
	if err != nil {
		return invitationLookup{}, "", err
	}
	var matched invitationLookup
	matchedDigest := ""
	matches := 0
	var operationalErr error
	for _, digest := range candidates {
		item, found, lookupErr := service.repository.lookupInvitation(ctx, service.security, principal, digest)
		if lookupErr != nil {
			operationalErr = lookupErr
			continue
		}
		if found {
			matched, matchedDigest, matches = item, digest, matches+1
		}
	}
	if operationalErr != nil {
		return invitationLookup{}, "", familyError(operationalErr)
	}
	if matches == 0 {
		return invitationLookup{}, "", ErrInvitationInvalid
	}
	if matches != 1 {
		return invitationLookup{}, "", ErrAuthorizationQuarantine
	}
	return matched, matchedDigest, nil
}

func (service *Service) Preview(ctx context.Context, principal access.Principal, token string) (InvitationPreview, error) {
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
	return InvitationPreview{FamilyDisplayName: invitation.FamilyDisplayName, ExpiresAt: invitation.ExpiresAt}, nil
}

func (service *Service) Redeem(ctx context.Context, principal access.Principal, commandID, token string, confirmed bool) (FamilyView, error) {
	if !confirmed {
		return FamilyView{}, ErrConfirmationRequired
	}
	if !validUUID(commandID) {
		return FamilyView{}, ErrInvalidRequest
	}
	invitation, digest, err := service.lookupInvitation(ctx, principal, token)
	if err != nil {
		return FamilyView{}, err
	}
	payload := security.InvitationPayload{Version: "v1", InvitationID: invitation.ID, FamilyID: invitation.FamilyID,
		RedeemerAccountID: principal.AccountID, TokenDigest: digest, Role: invitation.Role, Status: "redeemed", ExpiresAt: invitation.ExpiresAt.Format(time.RFC3339Nano)}
	raw, _ := json.Marshal(payload)
	command := security.Command{ID: commandID, Kind: "invitation_redeemed", AffectedAccountIDs: []string{principal.AccountID}, AffectedFamilyIDs: []string{invitation.FamilyID}, Payload: raw}
	if _, err := service.security.Execute(ctx, principal, command); err != nil {
		return FamilyView{}, familyError(err)
	}
	return service.familyAfterReceipt(ctx, principal, invitation.FamilyID)
}

func (service *Service) Invitation(ctx context.Context, principal access.Principal, invitationID string) (InvitationView, error) {
	if !validUUID(invitationID) {
		return InvitationView{}, ErrInvitationInvalid
	}
	var slot security.FamilySlotView
	var ok bool
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		slot, ok = view.FamilySlot(principal.AccountID)
		return nil
	})
	if err != nil {
		return InvitationView{}, familyError(err)
	}
	if !ok || slot.State != "active" {
		return InvitationView{}, ErrFamilyUnavailable
	}
	view, found, err := service.repository.invitation(ctx, service.security, principal, slot.FamilyID, invitationID)
	if err != nil {
		return InvitationView{}, familyError(err)
	}
	if !found {
		return InvitationView{}, ErrInvitationInvalid
	}
	return view, nil
}

func (service *Service) PendingInvitations(ctx context.Context, principal access.Principal) ([]InvitationView, error) {
	var slot security.FamilySlotView
	var ok bool
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		slot, ok = view.FamilySlot(principal.AccountID)
		return nil
	})
	if err != nil {
		return nil, familyError(err)
	}
	if !ok || slot.State != "active" {
		return nil, ErrFamilyUnavailable
	}
	views, err := service.repository.pendingInvitations(ctx, service.security, principal, slot.FamilyID, service.now().UTC())
	if err != nil {
		return nil, familyError(err)
	}
	return views, nil
}

func (service *Service) RevokeInvitation(ctx context.Context, principal access.Principal, commandID, invitationID string) (security.CommandResult, error) {
	if !validUUID(commandID) || !validUUID(invitationID) {
		return security.CommandResult{}, ErrInvalidRequest
	}
	var invitation security.InvitationView
	var ok bool
	var receipt security.ReceiptView
	var receiptExists bool
	var slot security.FamilySlotView
	var slotOK bool
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		invitation, ok = view.Invitation(invitationID)
		receipt, receiptExists = view.Receipt(principal.AccountID, commandID)
		slot, slotOK = view.FamilySlot(principal.AccountID)
		return nil
	})
	if err != nil {
		return security.CommandResult{}, familyError(err)
	}
	if receiptExists {
		if receipt.Kind != "invitation_revoked" || !ok {
			return security.CommandResult{}, ErrCommandIDReused
		}
		command := revokeInvitationCommand(commandID, invitationID, invitation)
		commandHash, hashErr := security.CanonicalCommandHash(principal, command)
		if hashErr != nil || commandHash != receipt.CommandHash {
			return security.CommandResult{}, ErrCommandIDReused
		}
		return receipt.Result, nil
	}
	if !slotOK || slot.State != "active" {
		return security.CommandResult{}, ErrFamilyUnavailable
	}
	if !ok || invitation.FamilyID != slot.FamilyID {
		return security.CommandResult{}, ErrInvitationInvalid
	}
	command := revokeInvitationCommand(commandID, invitationID, invitation)
	result, err := service.security.Execute(ctx, principal, command)
	return result, familyErrorOrNil(err)
}

func revokeInvitationCommand(commandID, invitationID string, invitation security.InvitationView) security.Command {
	payload := security.InvitationPayload{Version: "v1", InvitationID: invitationID, FamilyID: invitation.FamilyID, CreatorAccountID: invitation.AccountID,
		TokenDigest: invitation.TokenDigest, Role: invitation.Role, Status: "revoked", ExpiresAt: invitation.ExpiresAt}
	raw, _ := json.Marshal(payload)
	return security.Command{ID: commandID, Kind: "invitation_revoked", AffectedAccountIDs: []string{invitation.AccountID}, AffectedFamilyIDs: []string{invitation.FamilyID}, Payload: raw}
}

func (service *Service) Transfer(ctx context.Context, principal access.Principal, commandID, newOwnerID string) (security.CommandResult, error) {
	if !validUUID(commandID) || !validUUID(newOwnerID) {
		return security.CommandResult{}, ErrInvalidRequest
	}
	var slot security.FamilySlotView
	var ok bool
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		slot, ok = view.FamilySlot(principal.AccountID)
		return nil
	})
	if err != nil {
		return security.CommandResult{}, familyError(err)
	}
	if !ok || slot.State != "active" {
		return security.CommandResult{}, ErrFamilyUnavailable
	}
	payload := security.OwnerChangePayload{Version: "v1", FamilyID: slot.FamilyID, OldOwnerAccountID: principal.AccountID, NewOwnerAccountID: newOwnerID}
	raw, _ := json.Marshal(payload)
	result, err := service.security.Execute(ctx, principal, security.Command{ID: commandID, Kind: "owner_transferred", AffectedAccountIDs: []string{principal.AccountID, newOwnerID}, AffectedFamilyIDs: []string{slot.FamilyID}, Payload: raw})
	return result, familyErrorOrNil(err)
}

// ChangeRole moves a caregiver between writing and view-only.
//
// Owner-only, and refused for the owner's own row: a family must always have
// exactly one owner, and demoting yourself here would leave it with none.
// Stepping down is Transfer, which names a successor in the same command.
func (service *Service) ChangeRole(ctx context.Context, principal access.Principal, commandID, accountID, role string) (security.CommandResult, error) {
	if !validUUID(commandID) || !validUUID(accountID) || (role != "caregiver" && role != "viewer") {
		return security.CommandResult{}, ErrInvalidRequest
	}
	var slot security.FamilySlotView
	var ok bool
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		slot, ok = view.FamilySlot(principal.AccountID)
		return nil
	})
	if err != nil {
		return security.CommandResult{}, familyError(err)
	}
	if !ok || slot.State != "active" {
		return security.CommandResult{}, ErrFamilyUnavailable
	}
	payload := security.MemberRolePayload{Version: "v1", AccountID: accountID, FamilyID: slot.FamilyID, Role: role}
	raw, _ := json.Marshal(payload)
	result, err := service.security.Execute(ctx, principal, security.Command{
		ID: commandID, Kind: "member_role_changed",
		AffectedAccountIDs: []string{accountID}, AffectedFamilyIDs: []string{slot.FamilyID}, Payload: raw,
	})
	return result, familyErrorOrNil(err)
}

func (service *Service) Remove(ctx context.Context, principal access.Principal, commandID, accountID string) (security.CommandResult, error) {
	if !validUUID(commandID) || !validUUID(accountID) {
		return security.CommandResult{}, ErrInvalidRequest
	}
	var slot security.FamilySlotView
	var ok bool
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		slot, ok = view.FamilySlot(principal.AccountID)
		return nil
	})
	if err != nil {
		return security.CommandResult{}, familyError(err)
	}
	if !ok || slot.State != "active" {
		return security.CommandResult{}, ErrFamilyUnavailable
	}
	payload := security.MembershipPayload{Version: "v1", AccountID: accountID, FamilyID: slot.FamilyID}
	raw, _ := json.Marshal(payload)
	result, err := service.security.Execute(ctx, principal, security.Command{ID: commandID, Kind: "member_removed", AffectedAccountIDs: []string{accountID}, AffectedFamilyIDs: []string{slot.FamilyID}, Payload: raw})
	return result, familyErrorOrNil(err)
}

func (service *Service) Leave(ctx context.Context, principal access.Principal, commandID string) (security.CommandResult, error) {
	if !validUUID(commandID) {
		return security.CommandResult{}, ErrInvalidRequest
	}
	var receipt security.ReceiptView
	var receiptExists bool
	var slot security.FamilySlotView
	var ok bool
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		receipt, receiptExists = view.Receipt(principal.AccountID, commandID)
		slot, ok = view.FamilySlot(principal.AccountID)
		return nil
	})
	if err != nil {
		return security.CommandResult{}, familyError(err)
	}
	if receiptExists {
		if receipt.Kind != "member_left" {
			return security.CommandResult{}, ErrCommandIDReused
		}
		return receipt.Result, nil
	}
	if !ok || slot.State != "active" {
		return security.CommandResult{}, ErrFamilyUnavailable
	}
	payload := security.MembershipPayload{Version: "v1", AccountID: principal.AccountID, FamilyID: slot.FamilyID}
	raw, _ := json.Marshal(payload)
	result, err := service.security.Execute(ctx, principal, security.Command{ID: commandID, Kind: "member_left", AffectedAccountIDs: []string{principal.AccountID}, AffectedFamilyIDs: []string{slot.FamilyID}, Payload: raw})
	return result, familyErrorOrNil(err)
}

func familyErrorOrNil(err error) error {
	if err == nil {
		return nil
	}
	return familyError(err)
}
