package main

import (
	"babytracker/server/internal/protocol"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// probeImportHTTP exercises the feature binary through authenticated HTTP only.
// Tokens stay in private curl-config artifacts and never enter diagnostics.
func probeImportHTTP(ctx context.Context, origin, ownerPath, caregiverPath, phase string, persistHighWater func(protocol.Cursor) error) error {
	var next uint64
	var childName string
	switch phase {
	case "prior_at_n_minus_1":
		next, childName = 10000, "Overlap Import Child A"
	case "prior_at_n":
		next, childName = 20000, "Overlap Import Child B"
	case "current_at_n":
		next, childName = 30000, "Overlap Import Child C"
	default:
		return fmt.Errorf("invalid import overlap phase %q", phase)
	}
	if persistHighWater == nil {
		return fmt.Errorf("import overlap cursor persistence is required")
	}
	readToken := func(path string) (string, error) {
		raw, e := os.ReadFile(path)
		if e != nil {
			return "", e
		}
		for _, line := range strings.Split(string(raw), "\n") {
			const prefix = `header = "Authorization: Bearer `
			if strings.HasPrefix(line, prefix) && strings.HasSuffix(line, `"`) {
				return strings.TrimSuffix(strings.TrimPrefix(line, prefix), `"`), nil
			}
		}
		return "", fmt.Errorf("missing private HTTP credential")
	}
	owner, e := readToken(ownerPath)
	if e != nil {
		return e
	}
	caregiver, e := readToken(caregiverPath)
	if e != nil {
		return e
	}
	client := &http.Client{Timeout: 30 * time.Second}
	call := func(token, method, path, caps string, input, output any, want int) error {
		raw, e := json.Marshal(input)
		if e != nil {
			return e
		}
		var body io.Reader
		if input != nil {
			body = bytes.NewReader(raw)
		}
		r, e := http.NewRequestWithContext(ctx, method, origin+path, body)
		if e != nil {
			return e
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Baby-Tracker-Protocol", "v1")
		r.Header.Set("X-Baby-Tracker-Capabilities", caps)
		resp, e := client.Do(r)
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		b, e := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if e != nil {
			return e
		}
		if resp.StatusCode != want {
			return fmt.Errorf("import HTTP %s %s status=%d want=%d", method, path, resp.StatusCode, want)
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			return fmt.Errorf("import HTTP cache policy missing")
		}
		if output != nil {
			return json.Unmarshal(b, output)
		}
		return nil
	}
	id := func() string { next++; return fmt.Sprintf("93000000-0000-4000-8000-%012d", next) }
	meta := protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{"records-v1", "import-resolver-v1"}}
	var highWater protocol.Cursor
	syncCall := func(token string, m protocol.Mutation) (protocol.Outcome, error) {
		req := protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{"records-v1"}}, Mutations: []protocol.Mutation{m}}
		var out protocol.SyncResponse
		e := call(token, "POST", "/v1/sync", "records-v1", req, &out, 200)
		if e != nil {
			return protocol.Outcome{}, e
		}
		if len(out.Outcomes) != 1 {
			return protocol.Outcome{}, fmt.Errorf("missing import sync outcome")
		}
		if e := out.HighWater.Validate(); e != nil {
			return protocol.Outcome{}, fmt.Errorf("invalid import sync high water: %w", e)
		}
		highWater = out.HighWater
		return out.Outcomes[0], nil
	}
	babyID := id()
	baby := protocol.Mutation{MutationID: id(), EntityID: babyID, EntityKind: "baby", Operation: "create", PayloadVersion: 1, ClientMutationTime: "2026-09-20T12:00:00Z", Payload: protocol.BabyPayload{Name: childName, BirthDate: "2026-01-12"}}
	if out, e := syncCall(owner, baby); e != nil || out.Status != "applied" {
		return fmt.Errorf("seed import baby: status=%s error=%v", out.Status, e)
	}
	local, importID, recordID := id(), id(), id()
	contents := "formula"
	payload := protocol.FeedPayload{BabyID: babyID, Kind: "bottle", StartedAt: "2026-09-20T12:00:00.000000Z", TimeZoneID: "Europe/Zurich",
		Components: []protocol.FeedComponent{{ID: id(), Kind: "bottle", VolumeML: protocol.MustParseDecimal("1"), Contents: &contents}}}
	hash, e := protocol.CanonicalContentHash("feed", payload)
	if e != nil {
		return e
	}
	request := protocol.ImportResolveRequest{RequestMeta: meta, ImportID: importID, ImportMode: "match_shared", NormalizationVersion: "nfc-trim-fold-nfc-v1", Babies: []protocol.ImportBaby{{LocalBabyID: local, Name: childName, BirthDate: "2026-01-12"}}, Records: []protocol.RecordManifest{{RecordID: recordID, EntityKind: "feed", LocalBabyID: local, ContentSHA256: hex.EncodeToString(hash[:])}}, TotalRecordCount: 1}
	var stage, status, complete protocol.ImportResolveResponse
	if e = call(caregiver, "POST", "/v1/family/import/resolve", "records-v1,import-resolver-v1", request, &stage, 200); e != nil {
		return e
	}
	if stage.ManifestComplete || stage.MappingComplete || stage.NextChunkIndex != 1 {
		return fmt.Errorf("stage checkpoint invalid")
	}
	if e = call(caregiver, "GET", "/v1/family/import/"+importID, "records-v1,import-resolver-v1", nil, &status, 200); e != nil {
		return e
	}
	if !reflect.DeepEqual(stage, status) {
		return fmt.Errorf("staging resume changed")
	}
	request.ChunkIndex = 1
	request.Babies = []protocol.ImportBaby{}
	request.Records = []protocol.RecordManifest{}
	request.Finalize = true
	if e = call(caregiver, "POST", "/v1/family/import/resolve", "records-v1,import-resolver-v1", request, &complete, 200); e != nil {
		return e
	}
	if !complete.MappingComplete || len(complete.Mappings) != 1 || complete.Mappings[0].SharedBabyID != babyID {
		return fmt.Errorf("exact match did not adopt canonical UUID")
	}
	mutation := protocol.Mutation{MutationID: id(), EntityID: recordID, EntityKind: "feed", Operation: "create", PayloadVersion: 4, ClientMutationTime: "2026-09-20T12:00:00Z", ImportID: &importID, Payload: payload}
	created, e := syncCall(caregiver, mutation)
	if e != nil || created.Status != "applied" || created.CanonicalEntity == nil || created.CanonicalEntity.Meta.ID != recordID {
		return fmt.Errorf("imported record UUID not retained: %v", e)
	}
	mutation.MutationID = id()
	equal, e := syncCall(caregiver, mutation)
	if e != nil || equal.Status != "applied" || !reflect.DeepEqual(created.Cursor, equal.Cursor) || !reflect.DeepEqual(created.CanonicalEntity, equal.CanonicalEntity) {
		return fmt.Errorf("equal import did not retain original outcome: %v", e)
	}
	edit := mutation
	edit.MutationID = id()
	edit.ImportID = nil
	edit.Operation = "update"
	changed := payload
	note := "changed after mapping"
	changed.Note = &note
	edit.Payload = changed
	if out, e := syncCall(owner, edit); e != nil || out.Status != "applied" {
		return fmt.Errorf("edit import target: %v", e)
	}
	mutation.MutationID = id()
	rejected, e := syncCall(caregiver, mutation)
	if e != nil || rejected.Code == nil || *rejected.Code != "record_content_conflict" {
		return fmt.Errorf("import overwrote changed canonical content: %v", e)
	}
	baby.EntityID = id()
	baby.MutationID = id()
	if out, e := syncCall(owner, baby); e != nil || out.Status != "applied" {
		return fmt.Errorf("seed ambiguity: %v", e)
	}
	request.ImportID = id()
	request.ChunkIndex = 0
	request.TotalRecordCount = 0
	request.Babies = []protocol.ImportBaby{{LocalBabyID: id(), Name: childName, BirthDate: "2026-01-12"}}
	if e = call(caregiver, "POST", "/v1/family/import/resolve", "records-v1,import-resolver-v1", request, &status, 200); e != nil {
		return e
	}
	if status.MappingComplete || len(status.Conflicts) != 1 || status.Conflicts[0].Kind != "ambiguous_baby" {
		return fmt.Errorf("owner ambiguity missing")
	}
	choice := protocol.ImportConflictRequest{RequestMeta: meta, CommandID: id(), ResolverRevision: status.ResolverRevision, ConflictID: status.Conflicts[0].ConflictID, Choice: "use_existing", CanonicalBabyID: &babyID}
	if e = call(caregiver, "POST", "/v1/family/import/"+request.ImportID+"/resolve-conflict", "records-v1,import-resolver-v1", choice, nil, 403); e != nil {
		return e
	}
	if e = call(owner, "POST", "/v1/family/import/"+request.ImportID+"/resolve-conflict", "records-v1,import-resolver-v1", choice, &complete, 200); e != nil {
		return e
	}
	if !complete.MappingComplete || len(complete.Mappings) != 1 || complete.Mappings[0].SharedBabyID != babyID {
		return fmt.Errorf("explicit owner choice not published")
	}
	return persistHighWater(highWater)
}

func writeOverlapCursor(path string, cursor protocol.Cursor) error {
	if path == "" {
		return fmt.Errorf("overlap cursor path is required")
	}
	if e := cursor.Validate(); e != nil {
		return fmt.Errorf("invalid overlap cursor: %w", e)
	}
	value, e := json.Marshal(cursor)
	if e != nil {
		return e
	}
	file, e := os.CreateTemp(filepath.Dir(path), ".sync-overlap-cursor-*")
	if e != nil {
		return e
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if e = file.Chmod(0o600); e == nil {
		_, e = file.Write(append(value, '\n'))
	}
	if e == nil {
		e = file.Sync()
	}
	if closeErr := file.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	return os.Rename(temporary, path)
}
