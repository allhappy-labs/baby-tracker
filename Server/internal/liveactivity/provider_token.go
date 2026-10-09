package liveactivity

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const providerTokenRefreshAge = 40 * time.Minute

type providerToken struct {
	mu            sync.Mutex
	teamID, keyID string
	key           *ecdsa.PrivateKey
	now           func() time.Time
	cached        string
	issuedAt      time.Time
}

func newProviderToken(teamID, keyID string, key *ecdsa.PrivateKey, now func() time.Time) *providerToken {
	return &providerToken{teamID: teamID, keyID: keyID, key: key, now: now}
}

func (p *providerToken) token() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now().UTC()
	if p.cached != "" && !now.Before(p.issuedAt) && now.Sub(p.issuedAt) < providerTokenRefreshAge {
		return p.cached, nil
	}
	if p.key == nil || p.key.Curve == nil || p.key.Curve.Params().BitSize != 256 {
		return "", errors.New("APNs provider signing key is invalid")
	}
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": p.keyID})
	claims, _ := json.Marshal(map[string]any{"iss": p.teamID, "iat": now.Unix()})
	encode := base64.RawURLEncoding.EncodeToString
	unsigned := encode(header) + "." + encode(claims)
	digest := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, p.key, digest[:])
	if err != nil {
		return "", errors.New("APNs provider signing failed")
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	p.cached = unsigned + "." + encode(signature)
	p.issuedAt = now
	return p.cached, nil
}
