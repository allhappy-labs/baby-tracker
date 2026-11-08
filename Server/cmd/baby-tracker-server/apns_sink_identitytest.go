//go:build identitytest

package main

// The disposable protocol harness's Apple (main's live_activity_transport_
// identitytest.go, for wakes and Live Activities). With
// BT_IDENTITYTEST_APNS_SINK=1 an identitytest image keeps the production APNs
// client (provider token, headers, payload, classification) but hands each
// HTTP/2 request to this recorder instead of Apple: the recorder is the
// client's whole transport and never dials, and the hosts cannot resolve.
// Wakes and Live Activity pushes share the one client and recorder; each
// record keeps apns-push-type and apns-topic, so the harness tells them
// apart. The harness reads the record through GET /__identitytest/apns and
// may ask for the next answer to one token to be lost after the request was
// taken. It reads the wake worker's count of successful ticks through GET
// /__identitytest/wake, so it waits for whole ticks instead of sleeping past
// the tick interval. Tokens are the test's own synthetic values; nothing
// here can reach Apple. Without the flag an identitytest build with wakes or
// Live Activities enabled refuses to start (main's fell back to Apple's
// hosts).
//
// For load runs, BT_IDENTITYTEST_APNS_SINK_LATENCY_MS (0 to 2,000, default
// 0; anything else refuses to start) delays every answer, as Apple's round
// trip would; a push whose context ends inside the delay is recorded
// unacknowledged. The recorder keeps the latest 4,096 requests, numbered
// from the first; the record also reports the total taken and the most
// requests it held at once (the shared client's peak in-flight sends), and
// ?after=N returns only the requests numbered past N.

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptrace"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/config"
	"babytracker/server/internal/wake"
)

const (
	identityTestAPNSSinkEnv        = "BT_IDENTITYTEST_APNS_SINK"
	identityTestAPNSSinkLatencyEnv = "BT_IDENTITYTEST_APNS_SINK_LATENCY_MS"
	// identityTestAPNSMaxLatency bounds the answer delay.
	identityTestAPNSMaxLatency = 2 * time.Second
	// identityTestAPNSRecords is how many requests the recorder keeps.
	identityTestAPNSRecords = 4096
)

// Hosts that cannot resolve: the recorder answers before any dial.
const (
	identityTestAPNsSandboxURL    = "https://sandbox.apns.identitytest.invalid"
	identityTestAPNsProductionURL = "https://production.apns.identitytest.invalid"
)

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
	// ReceivedAt is when the recorder took the request (RFC 3339 with
	// nanoseconds, the server's clock): the load runner judges duplicate
	// pushes by it.
	ReceivedAt string `json:"received_at"`
}

type identityTestAPNSRecorder struct {
	mu      sync.Mutex
	key     *ecdsa.PublicKey
	latency time.Duration // before every answer
	// records is a ring of the latest identityTestAPNSRecords requests;
	// total counts every request taken (the next one is numbered total+1).
	records  []identityTestAPNSRecord
	total    int
	inFlight int
	peak     int // the most requests in RoundTrip at once
	loseNext map[string]int
}

// errIdentityTestAPNSLatency refuses an out-of-range or malformed latency.
var errIdentityTestAPNSLatency = errors.New("BT_IDENTITYTEST_APNS_SINK_LATENCY_MS must be whole milliseconds from 0 to 2000")

// identityTestAPNSLatency reads BT_IDENTITYTEST_APNS_SINK_LATENCY_MS: whole
// milliseconds from 0 to 2,000, 0 when unset.
func identityTestAPNSLatency() (time.Duration, error) {
	value := os.Getenv(identityTestAPNSSinkLatencyEnv)
	if value == "" {
		return 0, nil
	}
	ms, err := strconv.Atoi(value)
	if err != nil || strings.Trim(value, "0123456789") != "" || ms < 0 || time.Duration(ms)*time.Millisecond > identityTestAPNSMaxLatency {
		return 0, errIdentityTestAPNSLatency
	}
	return time.Duration(ms) * time.Millisecond, nil
}

var identityTestAPNS struct {
	sync.RWMutex
	recorder *identityTestAPNSRecorder
}

// identityTestWake is the wake worker GET /__identitytest/wake reports on:
// the one newWakeWorker composed last (the server composes exactly one).
var identityTestWake struct {
	sync.RWMutex
	worker *wake.Worker
}

// identityTestWakeWorker hands the composed wake worker to the identitytest
// routes, as newAPNSClient hands them its recorder.
func identityTestWakeWorker(worker *wake.Worker) {
	identityTestWake.Lock()
	identityTestWake.worker = worker
	identityTestWake.Unlock()
}

// newAPNSClient is the identitytest build's one APNs client, for wakes, Live
// Activities and apns-preflight: the production client over the recorder.
func newAPNSClient(creds config.APNSCredentials) (*apns.Client, error) {
	if os.Getenv(identityTestAPNSSinkEnv) != "1" {
		// An identitytest build never dials Apple (unlike main's, which fell
		// back to the production client): pushes need the recorder.
		return nil, errors.New("identitytest builds push only to the recorder: set " + identityTestAPNSSinkEnv + "=1 with BT_APNS_WAKES_ENABLED=true or BT_LIVE_ACTIVITY_PUSH_ENABLED=true")
	}
	latency, err := identityTestAPNSLatency()
	if err != nil {
		return nil, err
	}
	key, err := config.ReadAPNSPrivateKey(creds.KeyFile)
	if err != nil {
		return nil, err
	}
	recorder := &identityTestAPNSRecorder{key: &key.PublicKey, latency: latency, loseNext: map[string]int{}}
	client, err := apns.NewWithTransport(creds, recorder, identityTestAPNsSandboxURL, identityTestAPNsProductionURL, time.Now)
	if err != nil {
		return nil, err
	}
	identityTestAPNS.Lock()
	identityTestAPNS.recorder = recorder
	identityTestAPNS.Unlock()
	return client, nil
}

func (r *identityTestAPNSRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.inFlight++
	r.peak = max(r.peak, r.inFlight)
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inFlight--
		r.mu.Unlock()
	}()
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
	r.total++
	record.Sequence = r.total
	record.ReceivedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if len(r.records) < identityTestAPNSRecords {
		r.records = append(r.records, record)
	} else {
		r.records[(record.Sequence-1)%identityTestAPNSRecords] = record
	}
	r.mu.Unlock()
	// The recorder has the request, as Apple would once a transport wrote
	// it: report the write, so a lost answer is Ambiguous, never "not sent".
	if trace := httptrace.ContextClientTrace(request.Context()); trace != nil {
		if trace.WroteHeaders != nil {
			trace.WroteHeaders()
		}
		if trace.WroteRequest != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
	}
	if r.latency > 0 {
		timer := time.NewTimer(r.latency)
		select {
		case <-timer.C:
		case <-request.Context().Done():
			// The client gave up before the answer: Apple had the request.
			timer.Stop()
			r.unacknowledge(record.Sequence)
			return nil, request.Context().Err()
		}
	}
	if !record.Acknowledged {
		// Apple has the request; its answer never reaches the server.
		return nil, errors.New("identitytest APNs recorder: acknowledgment lost")
	}
	header := http.Header{}
	header.Set("apns-id", record.Headers["apns-id"])
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: io.NopCloser(strings.NewReader("")), Request: request, ProtoMajor: 2}, nil
}

// unacknowledge marks a kept request's answer as never delivered.
func (r *identityTestAPNSRecorder) unacknowledge(sequence int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.total-sequence < identityTestAPNSRecords {
		r.records[(sequence-1)%identityTestAPNSRecords].Acknowledged = false
	}
}

// snapshot is the kept requests numbered past after, oldest first, the total
// taken and the in-flight peak.
func (r *identityTestAPNSRecorder) snapshot(after int) ([]identityTestAPNSRecord, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := 0
	if r.total > identityTestAPNSRecords {
		start = r.total % identityTestAPNSRecords
	}
	records := []identityTestAPNSRecord{}
	for _, part := range [][]identityTestAPNSRecord{r.records[start:], r.records[:start]} {
		for _, record := range part {
			if record.Sequence > after {
				records = append(records, record)
			}
		}
	}
	return records, r.total, r.peak
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
	mux.HandleFunc("GET /__identitytest/apns", func(response http.ResponseWriter, request *http.Request) {
		recorder := current(response)
		if recorder == nil {
			return
		}
		after := 0
		if query := request.URL.Query(); query.Has("after") {
			value := query.Get("after")
			n, err := strconv.Atoi(value)
			if err != nil || value == "" || strings.Trim(value, "0123456789") != "" {
				http.Error(response, "invalid request", http.StatusBadRequest)
				return
			}
			after = n
		}
		records, total, peak := recorder.snapshot(after)
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]any{"requests": records, "total": total, "in_flight_peak": peak})
	})
	// The wake worker's successful ticks so far (Totals().TicksSucceeded: a
	// failed tick restores its work and sends nothing, so it proves
	// nothing). A tick counts when it returns, so once the count has grown
	// by two since a read, a whole tick that began after the read has
	// scanned and sent what was committed before it.
	mux.HandleFunc("GET /__identitytest/wake", func(response http.ResponseWriter, _ *http.Request) {
		identityTestWake.RLock()
		worker := identityTestWake.worker
		identityTestWake.RUnlock()
		response.Header().Set("Cache-Control", "no-store")
		if worker == nil {
			http.Error(response, "unavailable", http.StatusServiceUnavailable)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]uint64{"ticks": worker.Totals().TicksSucceeded})
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
