package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime/pprof"
	"sort"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
)

type perfReady struct{}

func (perfReady) Check(context.Context) error { return nil }

type perfFamily struct {
	p               access.Principal
	id, token, baby string
	cursor          protocol.Cursor
}

// Synthetic only: real authentication, quota repository, family authority,
// RLS and sync service over loopback HTTP. No production credentials or data.
func TestSyntheticSyncPerformanceProbe(t *testing.T) {
	if os.Getenv("BT_SYNC_PERF_PROBE") != "1" {
		t.Skip("opt-in synthetic performance probe")
	}
	h := New(t)
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{1}, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	policies := limits.DefaultPolicies()
	handler := httpapi.New(httpapi.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Readiness: perfReady{},
		InstanceID: h.Info.InstanceID, RecoveryEpoch: 1, CanonicalOrigin: h.Info.CanonicalOrigin, Capabilities: h.Info.Capabilities,
		SyncLimits: &h.Limits, SessionService: h.Auth, FamilyService: h.Family, SyncService: h.Service, Limiter: limiter,
		SyncMutationDevicePolicy: policies[limits.ScopeSyncMutationDevice], SyncMutationAccountPolicy: policies[limits.ScopeSyncMutationAccount], SyncMutationFamilyPolicy: policies[limits.ScopeSyncMutationFamily],
		SyncPullDevicePolicy: policies[limits.ScopeSyncPullDevice], SyncPullAccountPolicy: policies[limits.ScopeSyncPullAccount]})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := server.Client()
	client.Timeout = 30 * time.Second
	families := []perfFamily{{p: h.Owner, id: h.FamilyID, token: h.AccessToken("owner")}}
	seq := 0
	for _, familyCount := range []int{1, 8, 32} {
		for len(families) < familyCount {
			subject := fmt.Sprintf("perf-%d", len(families))
			p := h.signIn(subject, subject)
			view, e := h.Family.Create(t.Context(), p, h.CommandID(), subject)
			if e != nil {
				t.Fatal(e)
			}
			p = h.refresh(subject)
			families = append(families, perfFamily{p: p, id: view.ID, token: h.AccessToken(subject)})
		}
		for i := range families {
			if families[i].baby == "" {
				seq++
				m := MustMutation(t, "baby.json")
				m.MutationID = fmt.Sprintf("01000000-0000-4000-8000-%012x", seq)
				m.EntityID = fmt.Sprintf("10000000-0000-4000-8000-%012x", seq)
				out, e := h.Service.Apply(t.Context(), families[i].p, families[i].id, m)
				if e != nil || out.Status != "applied" {
					t.Fatalf("seed failed %v", e)
				}
				families[i].baby = m.EntityID
				families[i].cursor = protocol.Cursor{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: 1, FamilyID: families[i].id, Sequence: 1}
			}
		}
		revision, e := h.Authority.FamilyRevision(t.Context(), h.FamilyID)
		if e != nil {
			t.Fatal(e)
		}
		if familyCount == 32 && os.Getenv("BT_PERF_PROFILE") != "" {
			file, e := os.Create(os.Getenv("BT_PERF_PROFILE"))
			if e != nil {
				t.Fatal(e)
			}
			if e = pprof.StartCPUProfile(file); e != nil {
				t.Fatal(e)
			}
			defer func() { pprof.StopCPUProfile(); file.Close() }()
		}
		for _, concurrency := range []int{1, 4, 16} {
			if concurrency > familyCount {
				continue
			}
			for _, mode := range []string{"pull", "write-pull"} {
				const rounds = 15
				elapsed := make([]time.Duration, concurrency*rounds)
				errors := make(chan string, len(elapsed))
				var wg sync.WaitGroup
				started := time.Now()
				for worker := 0; worker < concurrency; worker++ {
					worker := worker
					wg.Add(1)
					go func() {
						defer wg.Done()
						f := &families[worker]
						for n := 0; n < rounds; n++ {
							request := protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}}, Cursor: &f.cursor, Mutations: []protocol.Mutation{}}
							if mode == "write-pull" {
								id := familyCount*100000 + concurrency*10000 + worker*100 + n
								m := MustMutation(t, "diaper.json")
								m.MutationID = fmt.Sprintf("01000000-0000-4000-8000-%012x", id)
								m.EntityID = fmt.Sprintf("40000000-0000-4000-8000-%012x", id)
								p := m.Payload.(protocol.DiaperPayload)
								p.BabyID = f.baby
								m.Payload = p
								request.Mutations = append(request.Mutations, m)
							}
							raw, e := json.Marshal(request)
							if e != nil {
								errors <- "encode"
								continue
							}
							req, e := http.NewRequest(http.MethodPost, server.URL+"/v1/sync", bytes.NewReader(raw))
							if e != nil {
								errors <- "request"
								continue
							}
							req.Header.Set("Authorization", "Bearer "+f.token)
							req.Header.Set("Content-Type", "application/json")
							req.Header.Set("X-Baby-Tracker-Protocol", "v1")
							req.Header.Set("X-Baby-Tracker-Capabilities", "records-v1")
							start := time.Now()
							response, e := client.Do(req)
							if e != nil {
								errors <- "transport"
								continue
							}
							body, _ := io.ReadAll(response.Body)
							response.Body.Close()
							elapsed[worker*rounds+n] = time.Since(start)
							if response.StatusCode != 200 {
								errors <- fmt.Sprintf("status=%d body=%s", response.StatusCode, body)
								continue
							}
							var out protocol.SyncResponse
							if json.Unmarshal(body, &out) != nil {
								errors <- "decode"
								continue
							}
							f.cursor = out.HighWater
							if mode == "write-pull" && (len(out.Outcomes) != 1 || out.Outcomes[0].Status != "applied") {
								errors <- "write not applied"
							}
						}
					}()
				}
				wg.Wait()
				wall := time.Since(started)
				close(errors)
				for e := range errors {
					t.Fatal(e)
				}
				sort.Slice(elapsed, func(i, j int) bool { return elapsed[i] < elapsed[j] })
				t.Logf("PERF families=%d family_revision=%d concurrency=%d mode=%s requests=%d p50_ms=%.3f p95_ms=%.3f qps=%.2f wall_s=%.3f", familyCount, revision, concurrency, mode, len(elapsed), float64(elapsed[len(elapsed)/2])/float64(time.Millisecond), float64(elapsed[(len(elapsed)*95-1)/100])/float64(time.Millisecond), float64(len(elapsed))/wall.Seconds(), wall.Seconds())
				h.Now = h.Now.Add(15 * time.Second) // Keep default quota windows realistic without expiring synthetic access tokens.
			}
		}
	}
}
