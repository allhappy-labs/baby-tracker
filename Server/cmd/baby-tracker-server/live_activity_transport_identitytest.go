//go:build identitytest

package main

// The disposable protocol harness's Apple. With BT_IDENTITYTEST_APNS_SINK=1 an
// identitytest image keeps the production APNs transport (provider token,
// headers, payload encoding, classification) but hands each HTTP/2 request to
// this recorder instead of Apple. The harness reads the record through
// /__identitytest/apns and may ask for the next acknowledgment to one token to
// be lost after the request was taken. Tokens are the test's own synthetic
// values; nothing here can reach Apple.

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"

	"babytracker/server/internal/config"
	"babytracker/server/internal/liveactivity"
)

const identityTestAPNSSinkEnv = "BT_IDENTITYTEST_APNS_SINK"

type identityTestAPNSRecord struct {
	Sequence            int               `json:"sequence"`
	Method              string            `json:"method"`
	Host                string            `json:"host"`
	Path                string            `json:"path"`
	Token               string            `json:"token"`
	Headers             map[string]string `json:"headers"`
	ProviderTokenValid  bool              `json:"provider_token_valid"`
	ProviderTokenKeyID  string            `json:"provider_token_key_id"`
	ProviderTokenIssuer string            `json:"provider_token_issuer"`
	Payload             json.RawMessage   `json:"payload"`
	Acknowledged        bool              `json:"acknowledged"`
}

type identityTestAPNSRecorder struct {
	mu       sync.Mutex
	key      *ecdsa.PublicKey
	records  []identityTestAPNSRecord
	loseNext map[string]int
}

var identityTestAPNS struct {
	sync.RWMutex
	recorder *identityTestAPNSRecorder
}

func newLiveActivityTransport(cfg config.LiveActivityConfig) (liveActivityTransport, error) {
	if os.Getenv(identityTestAPNSSinkEnv) != "1" {
		transport, err := liveactivity.NewAPNSTransport(cfg)
		if err != nil {
			return nil, err
		}
		return transport, nil
	}
	key, err := config.ReadAPNSPrivateKey(cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	recorder := &identityTestAPNSRecorder{key: &key.PublicKey, loseNext: map[string]int{}}
	transport, err := liveactivity.NewIdentityTestAPNSTransport(cfg, recorder)
	if err != nil {
		return nil, err
	}
	identityTestAPNS.Lock()
	identityTestAPNS.recorder = recorder
	identityTestAPNS.Unlock()
	return transport, nil
}

func (r *identityTestAPNSRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(io.LimitReader(request.Body, 8192))
	_ = request.Body.Close()
	if err != nil || !json.Valid(body) {
		return nil, errors.New("identitytest APNs recorder: unreadable payload")
	}
	token := strings.TrimPrefix(request.URL.Path, "/3/device/")
	record := identityTestAPNSRecord{Method: request.Method, Host: request.URL.Host, Path: request.URL.Path, Token: token,
		Headers: map[string]string{}, Payload: json.RawMessage(body), Acknowledged: true}
	for _, name := range []string{"apns-push-type", "apns-topic", "apns-id", "apns-priority", "apns-expiration", "content-type"} {
		record.Headers[name] = request.Header.Get(name)
	}
	record.ProviderTokenValid, record.ProviderTokenKeyID, record.ProviderTokenIssuer = r.verify(request.Header.Get("authorization"))
	r.mu.Lock()
	if r.loseNext[token] > 0 {
		r.loseNext[token]--
		record.Acknowledged = false
	}
	record.Sequence = len(r.records) + 1
	if len(r.records) < 4096 {
		r.records = append(r.records, record)
	}
	r.mu.Unlock()
	if !record.Acknowledged {
		// Apple has the request; its answer never reaches the server.
		return nil, errors.New("identitytest APNs recorder: acknowledgment lost")
	}
	header := http.Header{}
	header.Set("apns-id", record.Headers["apns-id"])
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: io.NopCloser(strings.NewReader("")), Request: request, ProtoMajor: 2}, nil
}

// verify checks the ES256 provider token against the mounted key's public half
// and returns its key ID and issuer, so the harness proves the real signer ran.
func (r *identityTestAPNSRecorder) verify(authorization string) (bool, string, string) {
	token, ok := strings.CutPrefix(authorization, "bearer ")
	if !ok {
		return false, "", ""
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false, "", ""
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	rawHeader, headerErr := base64.RawURLEncoding.DecodeString(parts[0])
	rawClaims, claimsErr := base64.RawURLEncoding.DecodeString(parts[1])
	signature, signatureErr := base64.RawURLEncoding.DecodeString(parts[2])
	if headerErr != nil || claimsErr != nil || signatureErr != nil || json.Unmarshal(rawHeader, &header) != nil || json.Unmarshal(rawClaims, &claims) != nil || len(signature) != 64 {
		return false, "", ""
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	valid := header.Algorithm == "ES256" && ecdsa.Verify(r.key, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:]))
	return valid, header.KeyID, claims.Issuer
}

func registerIdentityTestAPNS(mux *http.ServeMux) {
	current := func(response http.ResponseWriter) *identityTestAPNSRecorder {
		identityTestAPNS.RLock()
		recorder := identityTestAPNS.recorder
		identityTestAPNS.RUnlock()
		response.Header().Set("Cache-Control", "no-store")
		if recorder == nil {
			http.Error(response, "unavailable", http.StatusServiceUnavailable)
		}
		return recorder
	}
	mux.HandleFunc("GET /__identitytest/apns", func(response http.ResponseWriter, _ *http.Request) {
		recorder := current(response)
		if recorder == nil {
			return
		}
		recorder.mu.Lock()
		records := append([]identityTestAPNSRecord{}, recorder.records...)
		recorder.mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]any{"requests": records})
	})
	mux.HandleFunc("POST /__identitytest/apns/lose-ack", func(response http.ResponseWriter, request *http.Request) {
		recorder := current(response)
		if recorder == nil {
			return
		}
		var input struct {
			Token string `json:"token"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(response, request.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || input.Token == "" || strings.Trim(input.Token, "0123456789abcdef") != "" {
			http.Error(response, "invalid request", http.StatusBadRequest)
			return
		}
		recorder.mu.Lock()
		recorder.loseNext[input.Token]++
		recorder.mu.Unlock()
		response.WriteHeader(http.StatusNoContent)
	})
}
