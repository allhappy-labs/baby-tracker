//go:build identitytest

// Synthetic HTTP/server timing only. This client never measures a Swift store or UI.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"babytracker/server/internal/identity"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"github.com/coder/websocket"
)

type client struct {
	base string
	http *http.Client
}
type fixture struct {
	Owner, Caregiver protocol.SessionResponse
	Cursor           protocol.Cursor
	BabyID           string
}
type result struct {
	Kind                    string  `json:"kind"`
	Success                 int     `json:"success"`
	Refusals                int     `json:"refusals"`
	RequestErrors           int     `json:"request_errors"`
	DeadlineExpirations     int     `json:"deadline_expirations"`
	DurationSeconds         float64 `json:"duration_seconds"`
	RequestP50MS            float64 `json:"request_p50_ms"`
	RequestP95MS            float64 `json:"request_p95_ms"`
	TwoAuthenticatedSockets bool    `json:"two_authenticated_sockets"`
	HintGate                bool    `json:"hint_gate"`
	RecoveryAdmitted        bool    `json:"recovery_admitted"`
}

func main() {
	base := flag.String("base", "", "owned localhost TLS origin")
	cert := flag.String("cert", "", "owned TLS certificate")
	duration := flag.Int("duration", 30, "measurement seconds (30-60)")
	proof := flag.Bool("proof-only", false, "bounded correctness proof, no timing")
	state := flag.String("state", "", "optional private fixture state for composition tests")
	manifest := flag.String("cell-manifest", "", "owned Swift mode0600 credential manifest")
	concurrency := flag.Int("concurrency", 1, "maximum pending operations")
	flag.Parse()
	u, err := url.Parse(*base)
	if err != nil || u.Scheme != "https" || u.Hostname() != "localhost" || u.Port() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || *cert == "" || *duration < 30 || *duration > 60 {
		fail("invalid_owned_target")
	}
	pem, err := os.ReadFile(*cert)
	if err != nil {
		fail("certificate_unavailable")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		fail("certificate_invalid")
	}
	c := client{*base, &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect_refused") }}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*duration+90)*time.Second)
	defer cancel()
	if *manifest != "" {
		if err := c.serveCell(ctx, *manifest, *concurrency); err != nil {
			fail("cell_protocol_failed")
		}
		return
	}
	f, err := c.setup(ctx)
	if err != nil {
		fail(err.Error())
	}
	sockets := make([]*websocket.Conn, 0, 2)
	for _, token := range []string{f.Owner.AccessToken, f.Caregiver.AccessToken} {
		ws, e := c.live(ctx, token)
		if e != nil {
			fail("socket_admission_failed")
		}
		sockets = append(sockets, ws)
		defer ws.CloseNow()
	}
	response, err := c.createFeed(ctx, f, 900)
	if err != nil {
		fail("proof_mutation_failed")
	}
	for _, ws := range sockets {
		if !hintGate(ctx, ws, response.HighWater) {
			fail("hint_gate_failed")
		}
	}
	f.Cursor = response.HighWater
	if *state != "" {
		raw, _ := json.Marshal(f)
		file, e := os.OpenFile(*state, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			fail("state_write_failed")
		}
		_, e = file.Write(raw)
		closeErr := file.Close()
		if e != nil || closeErr != nil {
			fail("state_write_failed")
		}
	}
	r := result{Kind: "composition", TwoAuthenticatedSockets: true, HintGate: true, RecoveryAdmitted: true}
	if *proof {
		json.NewEncoder(os.Stdout).Encode(r)
		return
	}
	// Continuously consume socket traffic while measuring requests. The durations
	// below deliberately exclude peer durable application and visible publication.
	for _, ws := range sockets {
		go func(w *websocket.Conn) {
			for {
				if _, _, e := w.Read(ctx); e != nil {
					return
				}
			}
		}(ws)
	}
	// Warm the complete request path before collecting the bounded window.
	for i := 0; i < 5; i++ {
		if response, e := c.createFeed(ctx, f, 1000+i); e != nil {
			fail("warmup_failed")
		} else {
			f.Cursor = response.HighWater
		}
	}
	start := time.Now()
	deadline := start.Add(time.Duration(*duration) * time.Second)
	measurementContext, cancelMeasurement := context.WithDeadline(ctx, deadline)
	defer cancelMeasurement()
	var samples []float64
	for i := 2000; time.Now().Before(deadline); i++ {
		began := time.Now()
		response, e := c.createFeed(measurementContext, f, i)
		if e != nil {
			if r.recordFailure(e, measurementContext.Err()) {
				break
			}
			select {
			case <-measurementContext.Done():
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		f.Cursor = response.HighWater
		samples = append(samples, float64(time.Since(began))/float64(time.Millisecond))
	}
	r.Kind = "server_request_timing"
	r.Success = len(samples)
	r.DurationSeconds = time.Since(start).Seconds()
	sort.Float64s(samples)
	if len(samples) > 0 {
		r.RequestP50MS = samples[(len(samples)-1)/2]
		r.RequestP95MS = samples[(len(samples)-1)*95/100]
	}
	json.NewEncoder(os.Stdout).Encode(r)
	if r.Success == 0 {
		os.Exit(1)
	}
}

// Keep quota refusals separate from transport/protocol errors and the bounded
// measurement deadline. No response bodies or arbitrary error text are retained.
func (r *result) recordFailure(err, windowError error) bool {
	if errors.Is(windowError, context.DeadlineExceeded) {
		r.DeadlineExpirations++
		return true
	}
	if windowError != nil {
		r.RequestErrors++
		return true
	}
	if err.Error() == "/v1/sync http_429" {
		r.Refusals++
	} else {
		r.RequestErrors++
	}
	return false
}

func fail(code string) { fmt.Fprintln(os.Stderr, code); os.Exit(1) }
func id(n int) string  { return fmt.Sprintf("71000000-0000-4000-8000-%012d", n) }
func meta(caps ...string) protocol.RequestMeta {
	return protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: caps}
}
func (c client) setup(ctx context.Context) (fixture, error) {
	var f fixture
	var info protocol.ServerInfo
	if e := c.request(ctx, "GET", "/v1/server", "", nil, &info); e != nil {
		return f, e
	}
	if info.AppleAuthorizationMode != "identity_token_only" {
		return f, errors.New("selfhosted_required")
	}
	found := false
	for _, v := range info.Capabilities {
		if v == protocol.CapabilityRefreshRecoveryV1 {
			found = true
		}
	}
	if !found {
		return f, errors.New("recovery_capability_missing")
	}
	// A malformed body must reach the enabled route's parser, not its 503 gate.
	if e := c.request(ctx, "POST", "/v1/auth/refresh-recoverable", "", map[string]any{}, nil); e == nil || !strings.HasSuffix(e.Error(), "http_400") {
		return f, errors.New("recovery_route_not_admitted")
	}
	var err error
	f.Owner, err = c.signIn(ctx, 1)
	if err != nil {
		return f, err
	}
	f.Caregiver, err = c.signIn(ctx, 2)
	if err != nil {
		return f, err
	}
	ownerCredential, caregiverCredential := f.Owner.DeviceCredential, f.Caregiver.DeviceCredential
	if f.Owner.Account.ID == f.Caregiver.Account.ID || f.Owner.DeviceID == f.Caregiver.DeviceID {
		return f, errors.New("distinct_principals_required")
	}
	var family protocol.FamilyResponse
	command := func(n int) protocol.CommandRequest {
		return protocol.CommandRequest{RequestMeta: meta(protocol.CapabilityFamilyLifecycleV1), CommandID: id(n)}
	}
	if err = c.request(ctx, "POST", "/v1/family", f.Owner.AccessToken, protocol.CreateFamilyRequest{CommandRequest: command(10), DisplayName: "Synthetic"}, &family); err != nil {
		return f, err
	}
	if err = c.ready(ctx); err != nil {
		return f, err
	}
	if err = c.request(ctx, "POST", "/v1/auth/refresh", "", protocol.RefreshRequest{RequestMeta: meta(protocol.CapabilityIdentityV1), RefreshToken: f.Owner.RefreshToken}, &f.Owner); err != nil {
		return f, err
	}
	var invite protocol.InvitationResponse
	if err = c.request(ctx, "POST", "/v1/family/invitations", f.Owner.AccessToken, command(11), &invite); err != nil {
		return f, err
	}
	if err = c.request(ctx, "POST", "/v1/family/invitations/redeem", f.Caregiver.AccessToken, protocol.RedeemInvitationRequest{ConfirmedCommandRequest: protocol.ConfirmedCommandRequest{RequestMeta: meta(protocol.CapabilityFamilyLifecycleV1), CommandID: id(12), Confirmed: true}, Token: invite.Token}, &family); err != nil {
		return f, err
	}
	if err = c.ready(ctx); err != nil {
		return f, err
	}
	if err = c.request(ctx, "POST", "/v1/auth/refresh", "", protocol.RefreshRequest{RequestMeta: meta(protocol.CapabilityIdentityV1), RefreshToken: f.Caregiver.RefreshToken}, &f.Caregiver); err != nil {
		return f, err
	}
	f.Owner.DeviceCredential, f.Caregiver.DeviceCredential = ownerCredential, caregiverCredential
	if len(ownerCredential) != 43 || len(caregiverCredential) != 43 {
		return f, errors.New("continuity_credential_missing")
	}
	f.BabyID = id(20)
	var response protocol.SyncResponse
	err = c.request(ctx, "POST", "/v1/sync", f.Owner.AccessToken, protocol.SyncRequest{RequestMeta: meta(protocol.CapabilityRecordsV1), Mutations: []protocol.Mutation{{MutationID: id(21), EntityID: f.BabyID, EntityKind: "baby", Operation: "create", PayloadVersion: 1, ClientMutationTime: time.Now().UTC().Format(time.RFC3339Nano), Payload: protocol.BabyPayload{Name: "Synthetic", BirthDate: "2026-01-01"}}}}, &response)
	if err != nil || len(response.Outcomes) != 1 || response.Outcomes[0].Status != "applied" {
		if err != nil {
			return f, err
		}
		return f, errors.New("baby_seed_failed")
	}
	f.Cursor = response.HighWater
	// Deterministic existing care history, always through the actual sync route.
	for i := 100; i < 110; i++ {
		response, err = c.createFeed(ctx, f, i)
		if err != nil {
			return f, err
		}
		f.Cursor = response.HighWater
	}
	f.Cursor = response.HighWater
	var pull protocol.SyncResponse
	err = c.request(ctx, "POST", "/v1/sync", f.Caregiver.AccessToken, protocol.SyncRequest{RequestMeta: meta(protocol.CapabilityRecordsV1), Mutations: []protocol.Mutation{}}, &pull)
	if err != nil || pull.HighWater != f.Cursor || len(pull.Changes) < 11 {
		return f, errors.New("seed_pull_failed")
	}
	return f, nil
}
func (c client) signIn(ctx context.Context, n int) (protocol.SessionResponse, error) {
	var out protocol.SessionResponse
	var challenge protocol.ChallengeResponse
	if e := c.request(ctx, "POST", "/v1/auth/challenge", "", protocol.ChallengeRequest{RequestMeta: meta(protocol.CapabilityIdentityV1)}, &challenge); e != nil {
		return out, e
	}
	nonce, e := identity.AppleNonce(challenge.CanonicalOrigin, challenge.ServerInstanceID, challenge.Challenge)
	if e != nil {
		return out, errors.New("nonce_failed")
	}
	var proof struct {
		IdentityToken string `json:"identity_token"`
	}
	if e = c.request(ctx, "POST", "/__identitytest/proof", "", map[string]string{"subject": fmt.Sprintf("10000000-0000-4000-8000-%012d", n), "nonce": nonce}, &proof); e != nil {
		return out, e
	}
	e = c.request(ctx, "POST", "/v1/auth/apple", "", protocol.AppleExchangeRequest{RequestMeta: meta(protocol.CapabilityIdentityV1), CommandID: id(n + 30), Challenge: challenge.Challenge, IdentityToken: proof.IdentityToken, InstallationID: "synthetic-" + id(n), Profile: &protocol.ProfileInput{DisplayName: "Synthetic", Confirmed: true}}, &out)
	return out, e
}
func (c client) createFeed(ctx context.Context, f fixture, n int) (protocol.SyncResponse, error) {
	var out protocol.SyncResponse
	contents := "formula"
	e := c.request(ctx, "POST", "/v1/sync", f.Owner.AccessToken, protocol.SyncRequest{RequestMeta: meta(protocol.CapabilityRecordsV1), Cursor: &f.Cursor, Mutations: []protocol.Mutation{{MutationID: id(n + 100000), EntityID: id(n), EntityKind: "feed", Operation: "create", PayloadVersion: 4, ClientMutationTime: time.Now().UTC().Format(time.RFC3339Nano), Payload: protocol.FeedPayload{BabyID: f.BabyID, Kind: "bottle", StartedAt: "2026-09-20T12:00:00.000000Z", TimeZoneID: "Europe/Zurich", Components: []protocol.FeedComponent{{ID: id(n + 200000), Kind: "bottle", VolumeML: protocol.MustParseDecimal("100"), Contents: &contents}}}}}}, &out)
	if e == nil && (len(out.Outcomes) != 1 || out.Outcomes[0].Status != "applied") {
		e = errors.New("mutation_refused")
	}
	return out, e
}
func (c client) ready(ctx context.Context) error {
	for i := 0; i < 100; i++ {
		if e := c.request(ctx, "GET", "/readyz", "", nil, nil); e == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("readiness_timeout")
}
func (c client) request(ctx context.Context, method, path, token string, input, output any) error {
	var raw []byte
	if input != nil {
		var e error
		raw, e = json.Marshal(input)
		if e != nil {
			return errors.New("encode_failed")
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(raw))
	if e != nil {
		return errors.New("request_invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Baby-Tracker-Protocol", "v1")
	var requestMeta protocol.RequestMeta
	_ = json.Unmarshal(raw, &requestMeta)
	req.Header.Set("X-Baby-Tracker-Capabilities", strings.Join(requestMeta.Capabilities, ","))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, e := c.http.Do(req)
	if e != nil {
		return errors.New("transport_failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s http_%d", path, response.StatusCode)
	}
	if output == nil {
		_, e = io.Copy(io.Discard, response.Body)
	} else {
		e = json.NewDecoder(response.Body).Decode(output)
	}
	if e != nil {
		return errors.New("decode_failed")
	}
	return nil
}
func (c client) live(ctx context.Context, token string) (*websocket.Conn, error) {
	ws, _, err := websocket.Dial(ctx, c.base+"/v1/live", &websocket.DialOptions{HTTPClient: c.http, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}, "X-Baby-Tracker-Protocol": []string{"v1"}, "X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}, Subprotocols: []string{realtime.Subprotocol}})
	return ws, err
}
func hintGate(ctx context.Context, ws *websocket.Conn, want protocol.Cursor) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for {
		_, raw, e := ws.Read(ctx)
		if e != nil {
			return false
		}
		var hint realtime.Hint
		if json.Unmarshal(raw, &hint) != nil {
			return false
		}
		if hint.Cursor == want {
			return true
		}
	}
}

// No sockets or independent load loop: Swift is the sole complete-operation
// scheduler. All protocol traffic stays in the owned child pipes.
func (c client) serveCell(ctx context.Context, path string, concurrency int) error {
	return c.serveCellIO(ctx, path, concurrency, os.Stdin, os.Stdout)
}

func (c client) serveCellIO(ctx context.Context, path string, concurrency int, input io.Reader, output io.Writer) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || concurrency < 1 || concurrency > 64 {
		return errors.New("manifest_refused")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var fixtures []struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &fixtures) != nil || len(fixtures) == 0 || len(fixtures) > 128 {
		return errors.New("manifest_invalid")
	}
	enc := json.NewEncoder(output)
	var outputLock sync.Mutex
	emit := func(v any) { outputLock.Lock(); defer outputLock.Unlock(); _ = enc.Encode(v) }
	emit(map[string]any{"kind": "cell_start", "concurrency": concurrency})
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 65536)
	slots := make(chan struct{}, concurrency)
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	seen := map[int]bool{}
	var activity sync.Mutex
	active, peak := 0, 0
	for scanner.Scan() {
		var command struct {
			Operation int             `json:"operation"`
			Family    int             `json:"family"`
			Kind      string          `json:"kind"`
			Cursor    protocol.Cursor `json:"cursor"`
		}
		if json.Unmarshal(scanner.Bytes(), &command) != nil || command.Operation < 1 || seen[command.Operation] || command.Family < 0 || command.Family >= len(fixtures) || (command.Kind != "empty_pull" && command.Kind != "caught_up_read") {
			return errors.New("invalid_command")
		}
		seen[command.Operation] = true
		if len(seen) > 100000 {
			return errors.New("operation_bound")
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			activity.Lock()
			active++
			if active > peak {
				peak = active
			}
			activity.Unlock()
			defer func() { activity.Lock(); active--; activity.Unlock() }()
			defer func() { <-slots }()
			began := time.Now()
			requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			var response protocol.SyncResponse
			err := c.request(requestCtx, "POST", "/v1/sync", fixtures[command.Family].Token, protocol.SyncRequest{RequestMeta: meta(protocol.CapabilityRecordsV1), Cursor: &command.Cursor, Mutations: []protocol.Mutation{}}, &response)
			status := "success"
			if err != nil {
				status = "error"
				if requestCtx.Err() != nil {
					status = "timeout"
				} else if strings.HasSuffix(err.Error(), "http_429") {
					status = "rate_limit"
				}
			}
			emit(map[string]any{"kind": "operation_complete", "operation": command.Operation, "category": command.Kind, "status": status, "seconds": time.Since(began).Seconds(), "changes": len(response.Changes), "cursor": response.HighWater})
		}()
	}
	wg.Wait()
	if scanner.Err() != nil {
		return errors.New("input_failed")
	}
	emit(map[string]any{"kind": "cell_finish", "peak_active_requests": peak})
	return nil
}
