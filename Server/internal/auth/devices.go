package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"

	"babytracker/server/internal/identity"
)

type verifiedDeviceContextKey struct{}

type verifiedDeviceContext struct {
	accountID string
	identity  identity.VerifiedIdentity
}

func deviceCredentialDigest(key identity.DigestKey, origin, instanceID, accountID, deviceID, credential string) identity.KeyedDigest {
	digest := hmac.New(sha256.New, key.Secret[:])
	digest.Write([]byte("baby-tracker-device-continuity-v1\x00" + origin + "\x00" + instanceID + "\x00" + accountID + "\x00" + deviceID + "\x00" + credential))
	var sum [32]byte
	copy(sum[:], digest.Sum(nil))
	return identity.KeyedDigest{KeyID: key.ID, Sum: sum}
}

// ResolveDevice is intentionally unusable without the private fresh-proof
// context installed by Exchange. A continuity secret is a binding proof, not
// an account credential, and therefore cannot independently mint authority.
func (service *Service) ResolveDevice(ctx context.Context, verified identity.VerifiedIdentity, claim *identity.DeviceContinuityClaim) (DeviceBinding, string, error) {
	proof, ok := ctx.Value(verifiedDeviceContextKey{}).(verifiedDeviceContext)
	if !ok || proof.accountID == "" || proof.identity.Provider != verified.Provider || proof.identity.Audience != verified.Audience ||
		proof.identity.Subject != verified.Subject || proof.identity.IdentityKey != verified.IdentityKey {
		return DeviceBinding{}, "", ErrDeviceContinuityInvalid
	}
	if claim == nil {
		return service.newDevice(proof.accountID)
	}
	binding, err := service.resolveExistingDevice(ctx, proof.accountID, claim)
	return binding, "", err
}

func (service *Service) newDevice(accountID string) (DeviceBinding, string, error) {
	deviceID, err := newUUID()
	if err != nil {
		return DeviceBinding{}, "", ErrUnavailable
	}
	credential, err := newToken()
	if err != nil {
		return DeviceBinding{}, "", ErrUnavailable
	}
	digest := deviceCredentialDigest(service.deviceKeys.Current, service.origin, service.instanceID, accountID, deviceID, credential)
	return DeviceBinding{DeviceID: deviceID, AccountID: accountID, ServerInstanceID: service.instanceID, CanonicalOrigin: service.origin,
		CredentialKeyID: digest.KeyID, CredentialDigest: digest.Sum}, credential, nil
}

func (service *Service) resolveExistingDevice(ctx context.Context, accountID string, claim *identity.DeviceContinuityClaim) (DeviceBinding, error) {
	if service == nil || claim == nil || !validUUID(claim.DeviceID) || !validContinuityCredential(claim.Credential) || !validUUID(accountID) {
		return DeviceBinding{}, ErrDeviceContinuityInvalid
	}
	row, found, err := service.repository.LookupDeviceBinding(ctx, accountID, claim.DeviceID, service.instanceID, service.origin)
	if err != nil {
		return DeviceBinding{}, ErrUnavailable
	}
	keys := append([]identity.DigestKey{service.deviceKeys.Current}, service.deviceKeys.Previous...)
	matches := 0
	for _, key := range keys {
		candidate := deviceCredentialDigest(key, service.origin, service.instanceID, accountID, claim.DeviceID, claim.Credential)
		keyMatch := subtle.ConstantTimeCompare([]byte(candidate.KeyID), []byte(row.ContinuityKeyID))
		digestMatch := subtle.ConstantTimeCompare(candidate.Sum[:], row.ContinuityDigest[:])
		matches += keyMatch & digestMatch
	}
	if !found || row.Revoked || matches != 1 {
		return DeviceBinding{}, ErrDeviceContinuityInvalid
	}
	return DeviceBinding{DeviceID: claim.DeviceID, AccountID: accountID, ServerInstanceID: service.instanceID, CanonicalOrigin: service.origin,
		CredentialKeyID: row.ContinuityKeyID, CredentialDigest: row.ContinuityDigest}, nil
}

func validContinuityCredential(raw string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == raw
}

func authorityKeyedDigest(keyID string, digest [32]byte) string {
	return "hmac-sha256:" + keyID + ":" + hex.EncodeToString(digest[:])
}
