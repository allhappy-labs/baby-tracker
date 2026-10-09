package main

import (
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Execute the actual shell probes and import probe against one PostgreSQL-backed
// server. Every phase must hand off a cursor that includes all of its writes.
func TestMigrationOverlapHTTPAndImportProbesCarryCursorAcrossThreePhases(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t)
	repo, err := limits.NewRepository(h.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	rate, err := limits.New(limits.Options{Repository: repo, Pepper: [32]byte{1}, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	policies := limits.DefaultPolicies()
	server := httptest.NewServer(httpapi.New(httpapi.Options{
		Gate: h.Security, SessionService: h.Auth, FamilyService: h.Family,
		SyncService: h.Service, ImportService: h.Importer, SyncLimits: &h.Limits,
		InstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, Limiter: rate,
		SyncPullDevicePolicy: policies[limits.ScopeSyncPullDevice], SyncPullAccountPolicy: policies[limits.ScopeSyncPullAccount],
		SyncMutationDevicePolicy: policies[limits.ScopeSyncMutationDevice], SyncMutationAccountPolicy: policies[limits.ScopeSyncMutationAccount],
		SyncMutationFamilyPolicy: policies[limits.ScopeSyncMutationFamily],
	}))
	defer server.Close()
	directory := t.TempDir()
	for subject, name := range map[string]string{"owner": "owner.curl", "caregiver": "caregiver.curl"} {
		if err = os.WriteFile(filepath.Join(directory, name), []byte("silent\nshow-error\nfail\nheader = \"Authorization: Bearer "+h.AccessToken(subject)+"\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.ReadFile("../../scripts/check-migration-overlap.sh")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(source), "probe_feature_sync_http() {")
	end := strings.Index(string(source), "run_gate() {")
	if start < 0 || end <= start {
		t.Fatal("could not locate actual HTTP probe functions")
	}
	origin, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"prior_at_n_minus_1", "prior_at_n", "current_at_n"} {
		command := exec.Command("bash", "-c", `set -euo pipefail
workspace=$1
family_curl_config="$workspace/owner.curl"
caregiver_family_curl_config="$workspace/caregiver.curl"
`+string(source[start:end])+`

probe_feature_sync_http "$2" "$3"
probe_timer_feature_sync_http "$2" "$3"
`, "overlap-http-regression", directory, origin.Port(), phase)
		output, runErr := command.CombinedOutput()
		if runErr != nil {
			// Report useful HTTP evidence without exposing auth or paging tokens.
			responses, _ := filepath.Glob(filepath.Join(directory, phase+"*-response.json"))
			for _, path := range responses {
				data, _ := os.ReadFile(path)
				var response map[string]any
				if json.Unmarshal(data, &response) == nil {
					delete(response, "next_page_token")
					data, _ = json.Marshal(response)
					t.Logf("%s: %s", filepath.Base(path), data)
				}
			}
			t.Fatalf("phase %s real HTTP overlap probes failed: %v\n%s", phase, runErr, output)
		}
		if err = probeImportHTTP(t.Context(), server.URL,
			filepath.Join(directory, "owner.curl"), filepath.Join(directory, "caregiver.curl"), phase,
			func(cursor protocol.Cursor) error {
				return writeOverlapCursor(filepath.Join(directory, "sync-overlap-cursor.json"), cursor)
			}); err != nil {
			t.Fatalf("phase %s real HTTP import probe failed: %v", phase, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(directory, "sync-overlap-cursor.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cursor struct{ Sequence uint64 }
	if err = json.Unmarshal(data, &cursor); err != nil || cursor.Sequence != h.FamilySequence(t) {
		t.Fatalf("final handoff cursor %s does not include all phase writes: sequence=%d err=%v", data, h.FamilySequence(t), err)
	}
}
