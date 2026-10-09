package identity

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	appleJWKSURL       = "https://appleid.apple.com/auth/keys"
	maxJWKSBodyBytes   = 64 << 10
	maxJWKSKeys        = 16
	defaultJWKSTTL     = time.Hour
	maximumJWKSTTL     = 6 * time.Hour
	unknownKidCooldown = 30 * time.Second
	appleHTTPDeadline  = 5 * time.Second
)

type JWKSCache struct {
	http        *http.Client
	now         func() time.Time
	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	expires     time.Time
	lastUnknown time.Time
	refreshing  chan struct{}
}

func newJWKSCache(client *http.Client, now func() time.Time) *JWKSCache {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	clone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &JWKSCache{http: &clone, now: now, keys: make(map[string]*rsa.PublicKey)}
}

func (cache *JWKSCache) Key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if cache == nil || cache.http == nil || cache.now == nil || !validASCIIIdentifier(kid, 128) {
		return nil, errors.New("invalid jwks lookup")
	}
	for {
		now := cache.now()
		cache.mu.Lock()
		fresh := now.Before(cache.expires)
		known := cache.keys[kid] != nil
		if fresh && known {
			if key := cache.keys[kid]; key != nil {
				result := clonePublicKey(key)
				cache.mu.Unlock()
				return result, nil
			}
		}
		if cache.refreshing != nil {
			complete := cache.refreshing
			cache.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-complete:
				continue
			}
		}
		// A previously known key must refresh when its cache expires, even for
		// a zero TTL. A key absent from the last successful fetch remains under
		// the unknown-key cooldown regardless of cache expiry.
		if !known && !cache.lastUnknown.IsZero() && now.Before(cache.lastUnknown.Add(unknownKidCooldown)) {
			cache.mu.Unlock()
			return nil, errors.New("jwks refresh is cooling down")
		}
		complete := make(chan struct{})
		cache.refreshing = complete
		if !known {
			// Record the attempt before network I/O so a failed unknown-key
			// refresh is coalesced and cooled just like a successful miss.
			cache.lastUnknown = now
		}
		cache.mu.Unlock()

		keys, ttl, err := cache.fetch(ctx)

		cache.mu.Lock()
		if err == nil {
			cache.keys = keys
			cache.expires = cache.now().Add(ttl)
		}
		cache.refreshing = nil
		close(complete)
		if err != nil {
			cache.mu.Unlock()
			return nil, err
		}
		key := cache.keys[kid]
		cache.mu.Unlock()
		if key == nil {
			return nil, errors.New("unknown jwks key")
		}
		return clonePublicKey(key), nil
	}
}

func (cache *JWKSCache) fetch(ctx context.Context) (map[string]*rsa.PublicKey, time.Duration, error) {
	requestContext, cancel := context.WithTimeout(ctx, appleHTTPDeadline)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, appleJWKSURL, nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := cache.http.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, 0, errors.New("jwks endpoint returned non-success")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxJWKSBodyBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if len(body) > maxJWKSBodyBytes {
		return nil, 0, errors.New("jwks response exceeds bound")
	}
	keys, err := parseJWKS(body)
	if err != nil {
		return nil, 0, err
	}
	return keys, cacheTTL(response.Header.Values("Cache-Control")), nil
}

func parseJWKS(body []byte) (map[string]*rsa.PublicKey, error) {
	object, err := strictJSONObject(body, 6, 256)
	if err != nil {
		return nil, err
	}
	rawKeys, present := object["keys"]
	if !present {
		return nil, errors.New("jwks keys missing")
	}
	var encodedKeys []json.RawMessage
	if err := json.Unmarshal(rawKeys, &encodedKeys); err != nil || len(encodedKeys) == 0 || len(encodedKeys) > maxJWKSKeys {
		return nil, errors.New("invalid jwks key list")
	}
	keys := make(map[string]*rsa.PublicKey, len(encodedKeys))
	for _, encodedKey := range encodedKeys {
		keyObject, err := strictJSONObject(encodedKey, 3, 16)
		if err != nil {
			return nil, err
		}
		kid, err := exactJSONString(keyObject, "kid")
		if err != nil || !validASCIIIdentifier(kid, 128) {
			return nil, errors.New("invalid jwks kid")
		}
		if _, duplicate := keys[kid]; duplicate {
			return nil, errors.New("duplicate jwks kid")
		}
		keyType, err := exactJSONString(keyObject, "kty")
		if err != nil || keyType != "RSA" {
			return nil, errors.New("invalid jwks key type")
		}
		use, err := exactJSONString(keyObject, "use")
		if err != nil || use != "sig" {
			return nil, errors.New("invalid jwks use")
		}
		algorithm, err := exactJSONString(keyObject, "alg")
		if err != nil || algorithm != "RS256" {
			return nil, errors.New("invalid jwks algorithm")
		}
		if rawOperations, present := keyObject["key_ops"]; present {
			var operations []string
			if err := json.Unmarshal(rawOperations, &operations); err != nil || len(operations) != 1 || operations[0] != "verify" {
				return nil, errors.New("invalid jwks key operations")
			}
		}
		nEncoded, err := exactJSONString(keyObject, "n")
		if err != nil {
			return nil, errors.New("invalid jwks modulus")
		}
		nBytes, err := decodeCanonicalBase64URL(nEncoded)
		if err != nil || len(nBytes) == 0 || nBytes[0] == 0 {
			return nil, errors.New("invalid jwks modulus")
		}
		modulus := new(big.Int).SetBytes(nBytes)
		if modulus.BitLen() < 2048 || modulus.BitLen() > 4096 {
			return nil, errors.New("jwks modulus outside bound")
		}
		eEncoded, err := exactJSONString(keyObject, "e")
		if err != nil {
			return nil, errors.New("invalid jwks exponent")
		}
		eBytes, err := decodeCanonicalBase64URL(eEncoded)
		if err != nil || len(eBytes) == 0 || eBytes[0] == 0 {
			return nil, errors.New("invalid jwks exponent")
		}
		exponentValue := new(big.Int).SetBytes(eBytes)
		if !exponentValue.IsInt64() {
			return nil, errors.New("jwks exponent outside bound")
		}
		exponent := exponentValue.Int64()
		if exponent < 3 || exponent%2 == 0 || uint64(exponent) > uint64(^uint(0)>>1) {
			return nil, errors.New("invalid jwks exponent")
		}
		keys[kid] = &rsa.PublicKey{N: modulus, E: int(exponent)}
	}
	return keys, nil
}

func cacheTTL(headerValues []string) time.Duration {
	if len(headerValues) == 0 {
		return defaultJWKSTTL
	}
	foundMaxAge := false
	ttl := maximumJWKSTTL
	for _, header := range headerValues {
		for _, part := range strings.Split(header, ",") {
			directive := strings.TrimSpace(part)
			lower := strings.ToLower(directive)
			if lower == "no-cache" || lower == "no-store" {
				return 0
			}
			name, value, hasValue := strings.Cut(directive, "=")
			if !hasValue || !strings.EqualFold(strings.TrimSpace(name), "max-age") {
				continue
			}
			foundMaxAge = true
			value = strings.Trim(strings.TrimSpace(value), `"`)
			seconds, err := strconv.ParseUint(value, 10, 63)
			if err != nil || seconds > uint64(maximumJWKSTTL/time.Second) {
				if err != nil {
					return 0
				}
				seconds = uint64(maximumJWKSTTL / time.Second)
			}
			candidate := time.Duration(seconds) * time.Second
			if candidate < ttl {
				ttl = candidate
			}
		}
	}
	if !foundMaxAge {
		return defaultJWKSTTL
	}
	return ttl
}

func clonePublicKey(key *rsa.PublicKey) *rsa.PublicKey {
	return &rsa.PublicKey{N: new(big.Int).Set(key.N), E: key.E}
}
