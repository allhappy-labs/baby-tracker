package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"babytracker/server/internal/identity"
)

const appleCredentialPurpose = "apple-refresh-revocation-v1"
const maxStageBytes = 32768
const maxSafeJSONInteger = uint64(1<<53 - 1)

var canonicalUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var digestKeyIDPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
var credentialReferencePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
var lowercaseDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func newToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func tokenDigest(key identity.DigestKey, class, token string) identity.KeyedDigest {
	digest := hmac.New(sha256.New, key.Secret[:])
	digest.Write([]byte(class))
	digest.Write([]byte{0})
	digest.Write([]byte(token))
	var sum [32]byte
	copy(sum[:], digest.Sum(nil))
	return identity.KeyedDigest{KeyID: key.ID, Sum: sum}
}

func ActorID(key [32]byte, instanceID string, verified identity.VerifiedIdentity, generation uint64) (string, error) {
	if allZero(key[:]) || !validUUID(instanceID) || verified.Provider != "apple" || verified.Audience == "" || verified.Subject == "" ||
		generation == 0 || generation > maxSafeJSONInteger {
		return "", errors.New("invalid actor ID input")
	}
	digest := framedHMAC(key[:], "baby-tracker-actor-id-v1",
		[]byte(instanceID), []byte(verified.Provider), []byte(verified.Audience), []byte(verified.Subject), uint64Bytes(generation))
	return uuidV8(digest), nil
}

func CredentialObjectID(instanceID, accountID, purpose string) (string, error) {
	if !validUUID(instanceID) || !validUUID(accountID) || purpose != appleCredentialPurpose {
		return "", errors.New("invalid credential object input")
	}
	digest := sha256.Sum256([]byte("baby-tracker-apple-refresh-object-v1\x00" + instanceID + "\x00" + accountID + "\x00" + purpose))
	return uuidV8(digest), nil
}

func DeriveRefreshRotation(input RefreshDerivationInput) (RefreshDerivation, error) {
	fields, err := refreshRotationFields(input)
	if err != nil {
		return RefreshDerivation{}, err
	}
	command := framedHMAC(input.ParentKey.Secret[:], "baby-tracker-refresh-command-v2", fields...)
	session := framedHMAC(input.ParentKey.Secret[:], "baby-tracker-refresh-session-v2", fields...)
	access := framedHMAC(input.ParentKey.Secret[:], "baby-tracker-refresh-access-v2", fields...)
	refresh := framedHMAC(input.ParentKey.Secret[:], "baby-tracker-refresh-token-v2", fields...)
	return RefreshDerivation{
		CommandID: uuidV8(command), SessionID: uuidV8(session),
		AccessToken:  base64.RawURLEncoding.EncodeToString(access[:]),
		RefreshToken: base64.RawURLEncoding.EncodeToString(refresh[:]),
	}, nil
}

func DeriveRefreshReuseCommand(input RefreshDerivationInput) (string, error) {
	if err := validateRefreshParent(input); err != nil {
		return "", err
	}
	digest := framedHMAC(input.ParentKey.Secret[:], "baby-tracker-refresh-reuse-v1",
		[]byte(input.ServerInstanceID), []byte(input.AccountID), []byte(input.DeviceID), []byte(input.ParentSessionID),
		[]byte(input.LineageRootSessionID), uint64Bytes(input.RecoveryEpoch), []byte(input.ParentKey.ID), input.ParentDigest[:])
	return uuidV8(digest), nil
}

func refreshRotationFields(input RefreshDerivationInput) ([][]byte, error) {
	if err := validateRefreshParent(input); err != nil || !digestKeyIDPattern.MatchString(input.OutputKeyID) ||
		(input.Scope != "account" && input.Scope != "family" && input.Scope != "family_recovery") ||
		input.HeadGeneration == 0 || input.HeadGeneration > maxSafeJSONInteger || input.HeadRevision >= maxSafeJSONInteger ||
		input.TargetRevision != input.HeadRevision+1 || allZero(input.HeadHash[:]) {
		return nil, errors.New("invalid refresh derivation input")
	}
	family := ""
	if input.Scope == "account" {
		if input.FamilyID != nil {
			return nil, errors.New("account scope cannot have a family")
		}
	} else {
		if input.FamilyID == nil || !validUUID(*input.FamilyID) {
			return nil, errors.New("family scope requires a family")
		}
		family = *input.FamilyID
	}
	if !validDeadlines(input.AccessExpiresAt, input.RefreshExpiresAt, input.ReauthenticateAt, input.AbsoluteExpiresAt) {
		return nil, errors.New("refresh deadlines must be ordered UTC values")
	}
	return [][]byte{
		[]byte(input.ServerInstanceID), []byte(input.AccountID), []byte(input.DeviceID), []byte(input.ParentSessionID),
		[]byte(input.LineageRootSessionID), uint64Bytes(input.RecoveryEpoch), []byte(input.ParentKey.ID), input.ParentDigest[:],
		[]byte(input.OutputKeyID), []byte(input.Scope), []byte(family),
		[]byte(input.AccessExpiresAt.Format(time.RFC3339Nano)), []byte(input.RefreshExpiresAt.Format(time.RFC3339Nano)),
		[]byte(input.ReauthenticateAt.Format(time.RFC3339Nano)), []byte(input.AbsoluteExpiresAt.Format(time.RFC3339Nano)),
		uint64Bytes(input.HeadGeneration), uint64Bytes(input.HeadRevision), input.HeadHash[:], uint64Bytes(input.TargetRevision),
	}, nil
}

func validateRefreshParent(input RefreshDerivationInput) error {
	if !validUUID(input.ServerInstanceID) || !validUUID(input.AccountID) || !validUUID(input.DeviceID) ||
		!validUUID(input.ParentSessionID) || !validUUID(input.LineageRootSessionID) || input.RecoveryEpoch == 0 ||
		input.RecoveryEpoch > maxSafeJSONInteger ||
		!digestKeyIDPattern.MatchString(input.ParentKey.ID) || allZero(input.ParentKey.Secret[:]) || allZero(input.ParentDigest[:]) {
		return errors.New("invalid refresh parent")
	}
	return nil
}

func EncodeExchangeStage(stage ExchangeStage) ([]byte, [32]byte, error) {
	if err := validateStageCommon(stage.StageCommon, "exchange"); err != nil {
		return nil, [32]byte{}, err
	}
	if stage.IdentityProvider != "apple" || !digestKeyIDPattern.MatchString(stage.IdentityKeyID) ||
		!lowercaseDigestPattern.MatchString(stage.IdentityDigest) || !validBoundedText(stage.IdentityAudience, 255) ||
		!validBoundedText(stage.IdentitySubject, 255) {
		return nil, [32]byte{}, errors.New("invalid exchange identity material")
	}
	if (stage.DisplayName != nil) != stage.ProfileConfirmed || (stage.DisplayName != nil && !validDisplayName(*stage.DisplayName)) {
		return nil, [32]byte{}, errors.New("invalid exchange profile material")
	}
	if !matchingPair(stage.InstallationKeyID, stage.InstallationDigest) ||
		!matchingPair(stage.ContinuityKeyID, stage.ContinuityDigest) ||
		(stage.RegisterDevice != (stage.ContinuityKeyID != nil)) {
		return nil, [32]byte{}, errors.New("invalid exchange device material")
	}
	for _, pair := range [][2]*string{{stage.InstallationKeyID, stage.InstallationDigest}, {stage.ContinuityKeyID, stage.ContinuityDigest}} {
		if pair[0] != nil && (!digestKeyIDPattern.MatchString(*pair[0]) || !lowercaseDigestPattern.MatchString(*pair[1])) {
			return nil, [32]byte{}, errors.New("invalid exchange device digest")
		}
	}
	credential := []*string{stage.AppleCredentialID, stage.AppleCredentialKeyID, stage.AppleCredentialPurpose, stage.AppleCredentialObjectID}
	present := 0
	for _, value := range credential {
		if value != nil {
			present++
		}
	}
	if present != 0 && present != len(credential) {
		return nil, [32]byte{}, errors.New("invalid exchange credential material")
	}
	if present != 0 && (!credentialReferencePattern.MatchString(*stage.AppleCredentialID) || !credentialReferencePattern.MatchString(*stage.AppleCredentialKeyID) ||
		*stage.AppleCredentialPurpose != appleCredentialPurpose || !validUUID(*stage.AppleCredentialObjectID)) {
		return nil, [32]byte{}, errors.New("invalid exchange credential reference")
	}
	return marshalStage(stage)
}

func EncodeRefreshStage(stage RefreshStage) ([]byte, [32]byte, error) {
	if err := validateStageCommon(stage.StageCommon, "refresh"); err != nil || !validUUID(stage.ParentSessionID) || stage.Recovery != nil {
		return nil, [32]byte{}, errors.New("invalid refresh stage")
	}
	return marshalStage(stage)
}

func validateStageCommon(stage StageCommon, kind string) error {
	if stage.Version != "v1" || stage.Kind != kind || !validUUID(stage.AccountID) || !validUUID(stage.DeviceID) ||
		!validUUID(stage.SessionID) || !validUUID(stage.LineageRootSessionID) || !validUUID(stage.ServerInstanceID) ||
		!digestKeyIDPattern.MatchString(stage.TokenKeyID) || !lowercaseDigestPattern.MatchString(stage.AccessDigest) ||
		!lowercaseDigestPattern.MatchString(stage.RefreshDigest) || stage.RecoveryEpoch == 0 || stage.RecoveryEpoch > maxSafeJSONInteger ||
		stage.SecurityRevision == 0 || stage.SecurityRevision > maxSafeJSONInteger ||
		(stage.Scope != "account" && stage.Scope != "family" && stage.Scope != "family_recovery") {
		return errors.New("invalid stage material")
	}
	if !validCanonicalOrigin(stage.CanonicalOrigin) {
		return errors.New("invalid stage origin")
	}
	if stage.Scope == "account" {
		if stage.FamilyID != nil {
			return errors.New("account stage has family")
		}
	} else if stage.FamilyID == nil || !validUUID(*stage.FamilyID) {
		return errors.New("family stage requires family")
	}
	values := []string{stage.AccessExpiresAt, stage.RefreshExpiresAt, stage.ReauthenticateAt, stage.AbsoluteExpiresAt}
	deadlines := make([]time.Time, 0, len(values))
	for _, value := range values {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != value {
			return errors.New("stage deadline is not canonical UTC")
		}
		deadlines = append(deadlines, parsed)
	}
	if !validDeadlines(deadlines[0], deadlines[1], deadlines[2], deadlines[3]) {
		return errors.New("stage deadlines are not ordered")
	}
	return nil
}

func validDeadlines(access, refresh, reauthenticate, absolute time.Time) bool {
	for _, deadline := range []time.Time{access, refresh, reauthenticate, absolute} {
		if !validCanonicalDeadline(deadline) {
			return false
		}
	}
	return !access.After(refresh) && !refresh.After(reauthenticate) && reauthenticate.Equal(absolute)
}

func validCanonicalDeadline(deadline time.Time) bool {
	if deadline.IsZero() || deadline.Location() != time.UTC || deadline.Year() < 1 || deadline.Year() > 9999 {
		return false
	}
	encoded := deadline.Format(time.RFC3339Nano)
	parsed, err := time.Parse(time.RFC3339Nano, encoded)
	return err == nil && parsed.Location() == time.UTC && parsed.Equal(deadline) && parsed.Format(time.RFC3339Nano) == encoded
}

func validCanonicalOrigin(raw string) bool {
	if raw == "" || len(raw) > 2048 || !isASCII(raw) || strings.TrimSpace(raw) != raw || strings.Contains(raw, `\`) {
		return false
	}
	origin, err := url.Parse(raw)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.Opaque != "" || origin.User != nil ||
		origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.Path != "" || origin.RawPath != "" {
		return false
	}
	hostname := origin.Hostname()
	if hostname == "" || hostname != strings.ToLower(hostname) || strings.HasSuffix(hostname, ".") || len(hostname) > 253 {
		return false
	}
	if _, err := netip.ParseAddr(hostname); err == nil {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for index := range label {
			character := label[index]
			if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-') {
				return false
			}
		}
	}
	canonicalHost := hostname
	if port := origin.Port(); port != "" {
		parsed, err := strconv.ParseUint(port, 10, 16)
		if err != nil || parsed == 0 || parsed == 443 || strconv.FormatUint(parsed, 10) != port {
			return false
		}
		canonicalHost += ":" + port
	}
	return origin.Host == canonicalHost && raw == "https://"+canonicalHost
}

func matchingPair(first, second *string) bool { return (first == nil) == (second == nil) }

func validBoundedText(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validDisplayName(value string) bool {
	if !validBoundedText(value, 320) || value != strings.TrimSpace(value) || utf8.RuneCountInString(value) > 80 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func isASCII(value string) bool {
	for _, character := range []byte(value) {
		if character > 0x7f || character == 0 {
			return false
		}
	}
	return true
}

func marshalStage(stage any) ([]byte, [32]byte, error) {
	encoded, err := json.Marshal(stage)
	if err != nil || len(encoded) == 0 || len(encoded) > maxStageBytes {
		return nil, [32]byte{}, errors.New("stage material exceeds bound")
	}
	return encoded, sha256.Sum256(encoded), nil
}

func framedHMAC(key []byte, domain string, fields ...[]byte) [32]byte {
	digest := hmac.New(sha256.New, key)
	writeFrame(digest, []byte(domain))
	for _, field := range fields {
		writeFrame(digest, field)
	}
	var sum [32]byte
	copy(sum[:], digest.Sum(nil))
	return sum
}

type byteWriter interface{ Write([]byte) (int, error) }

func writeFrame(writer byteWriter, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}

func uint64Bytes(value uint64) []byte {
	result := make([]byte, 8)
	binary.BigEndian.PutUint64(result, value)
	return result
}

func uuidV8(digest [32]byte) string {
	raw := digest[:16]
	raw[6] = raw[6]&0x0f | 0x80
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

func validUUID(value string) bool { return canonicalUUIDPattern.MatchString(value) }

func allZero(value []byte) bool {
	zero := make([]byte, len(value))
	return subtle.ConstantTimeCompare(value, zero) == 1
}
