//go:build identitytest

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"

	"babytracker/server/internal/identity"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"github.com/coder/websocket"
)

type savedState struct {
	Recovery                   *recoveryExpectation
	Owner, Caregiver, Outsider protocol.SessionResponse
	// An account that is never put in a family here. The Swift
	// two-caregiver gate signs in to it again from a new phone, through the
	// app's own Apple sign-in path, and joins that phone to a family another
	// phone creates. Not Outsider, whom the single-client Swift test uses to
	// create a family of its own.
	Outsider2         protocol.SessionResponse
	FamilyID          string
	Cursor            protocol.Cursor
	FrozenTimerStart  []byte
	DroppedTimerStart protocol.SyncResponse
}

type client struct {
	base string
	http *http.Client
}

func main() {
	base := flag.String("base", "", "identity-test server base URL")
	statePath := flag.String("state", "", "private state file")
	phase := flag.String("phase", "", "bootstrap, overlap, verify, recovery-bootstrap, recovery-verify, or recovery-reuse")
	recoveryFault := flag.String("recovery-fault", "lost_response", "identitytest server recovery boundary")
	flag.Parse()
	if *base == "" || *statePath == "" {
		fatal(errors.New("base and state are required"))
	}
	c := client{base: strings.TrimRight(*base, "/"), http: &http.Client{Timeout: 10 * time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var err error
	switch *phase {
	case "bootstrap":
		err = c.bootstrap(ctx, *statePath, false)
	case "overlap":
		err = c.bootstrap(ctx, *statePath, true)
	case "verify":
		err = c.verify(ctx, *statePath)
	case "recovery-bootstrap":
		err = c.recoveryBootstrap(ctx, *statePath, *recoveryFault)
	case "recovery-verify":
		err = c.recoveryVerify(ctx, *statePath)
	case "recovery-reuse":
		err = c.recoveryReuse(ctx, *statePath)
	default:
		err = errors.New("phase must be bootstrap, overlap, verify, recovery-bootstrap, recovery-verify, or recovery-reuse")
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "protocol-e2e:", err)
	os.Exit(1)
}

func (c client) bootstrap(ctx context.Context, path string, includeImport bool) error {
	owner, err := c.signIn(ctx, "10000000-0000-4000-8000-000000000001", "Owner")
	if err != nil {
		return err
	}
	ownerCredential := owner.DeviceCredential
	caregiver, err := c.signIn(ctx, "10000000-0000-4000-8000-000000000002", "Caregiver")
	if err != nil {
		return err
	}
	caregiverCredential := caregiver.DeviceCredential
	outsider, err := c.signIn(ctx, "10000000-0000-4000-8000-000000000003", "Outsider")
	if err != nil {
		return err
	}
	outsider2, err := c.signIn(ctx, "10000000-0000-4000-8000-000000000004", "Outsider2")
	if err != nil {
		return err
	}
	var family protocol.FamilyResponse
	if err = c.post(ctx, "/v1/family", owner.AccessToken, protocol.CreateFamilyRequest{
		CommandRequest: protocol.CommandRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityFamilyLifecycleV1}},
			CommandID: "20000000-0000-4000-8000-000000000001"}, DisplayName: "Family",
	}, &family); err != nil {
		return err
	}
	if err = c.waitReady(ctx); err != nil {
		return err
	}
	owner, err = c.refresh(ctx, owner.RefreshToken)
	if err != nil {
		return err
	}
	owner.DeviceCredential = ownerCredential
	var invitation protocol.InvitationResponse
	if err = c.post(ctx, "/v1/family/invitations", owner.AccessToken, protocol.CommandRequest{
		RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityFamilyLifecycleV1}},
		CommandID:   "20000000-0000-4000-8000-000000000002",
	}, &invitation); err != nil {
		return err
	}
	if len(invitation.Token) != 43 {
		return fmt.Errorf("invitation token length=%d", len(invitation.Token))
	}
	var preview protocol.InvitationPreviewResponse
	if err = c.post(ctx, "/v1/family/invitations/preview", caregiver.AccessToken, protocol.PreviewInvitationRequest{
		RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityFamilyLifecycleV1}}, Token: invitation.Token,
	}, &preview); err != nil {
		return fmt.Errorf("invitation preview: %w", err)
	}
	if err = c.post(ctx, "/v1/family/invitations/redeem", caregiver.AccessToken, protocol.RedeemInvitationRequest{
		ConfirmedCommandRequest: protocol.ConfirmedCommandRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityFamilyLifecycleV1}}, CommandID: "20000000-0000-4000-8000-000000000003", Confirmed: true},
		Token:                   invitation.Token,
	}, &family); err != nil {
		return err
	}
	if err = c.waitReady(ctx); err != nil {
		return err
	}
	caregiver, err = c.refresh(ctx, caregiver.RefreshToken)
	if err != nil {
		return err
	}
	caregiver.DeviceCredential = caregiverCredential
	if len(owner.DeviceCredential) != 43 || len(caregiver.DeviceCredential) != 43 {
		return errors.New("device continuity credential was not retained across refresh")
	}
	connection, err := c.live(ctx, caregiver.AccessToken)
	if err != nil {
		return err
	}
	defer connection.CloseNow()
	response, err := c.sync(ctx, owner.AccessToken, nil, "01000000-0000-4000-8000-000000000101", "11000000-0000-4000-8000-000000000101")
	if err != nil || len(response.Outcomes) != 1 || response.Outcomes[0].Cursor == nil {
		return fmt.Errorf("initial mutation outcome count=%d: %w", len(response.Outcomes), err)
	}
	hint, err := readHint(ctx, connection)
	if err != nil || hint.Cursor != *response.Outcomes[0].Cursor || hint.FamilyID != family.ID {
		return fmt.Errorf("initial hint mismatch: %w", err)
	}
	finalCursor := response.HighWater
	var frozenStart []byte
	var dropped protocol.SyncResponse
	if !includeImport {
		frozenStart, err = json.Marshal(protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{
			protocol.CapabilityRecordsV1, protocol.CapabilityTimerCommandsV1}}, Cursor: &response.HighWater, Mutations: []protocol.Mutation{{
			MutationID: "01000000-0000-4000-8000-000000000201", EntityID: "51000000-0000-4000-8000-000000000201",
			EntityKind: "timer", Operation: "timer_start", PayloadVersion: 5, ClientMutationTime: "2026-09-20T15:00:00.000000Z",
			Payload: protocol.TimerStartPayload{BabyID: "11000000-0000-4000-8000-000000000101", TimerKind: "feed",
				StartedAt: "2026-09-20T15:00:00.000000Z", TimeZoneID: "Europe/Zurich", CompletedComponents: []protocol.FeedComponent{},
				CurrentSide: stringPointer("left"), CurrentSideStartedAt: stringPointer("2026-09-20T15:00:00.000000Z")},
		}}})
		if err != nil {
			return err
		}
		dropped, err = c.dropCommittedSyncResponse(ctx, owner.AccessToken, frozenStart)
		if err != nil {
			return err
		}
		if len(dropped.Outcomes) != 1 || dropped.Outcomes[0].Status != "applied" || dropped.Outcomes[0].Cursor == nil || dropped.Outcomes[0].CanonicalEntity == nil {
			return fmt.Errorf("dropped timer response was not committed: %+v", dropped)
		}
		finalCursor = dropped.HighWater
	}
	if includeImport {
		const localBabyID = "12000000-0000-4000-8000-000000000201"
		var imported protocol.ImportResolveResponse
		if err = c.request(ctx, http.MethodPost, "/v1/family/import/resolve", owner.AccessToken, "records-v1,import-resolver-v1", protocol.ImportResolveRequest{
			RequestMeta:          protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1, protocol.CapabilityImportResolverV1}},
			ImportID:             "21000000-0000-4000-8000-000000000201",
			ImportMode:           "preserve_local_ids",
			NormalizationVersion: "nfc-trim-fold-nfc-v1",
			Babies:               []protocol.ImportBaby{{LocalBabyID: localBabyID, Name: "Imported Baby", BirthDate: "2026-02-13", SortOrder: 1}},
			Records:              []protocol.RecordManifest{}, TotalRecordCount: 0, ChunkIndex: 0, Finalize: true,
		}, &imported); err != nil {
			return fmt.Errorf("overlap import: %w", err)
		}
		if !imported.MappingComplete || !imported.ManifestComplete || len(imported.Mappings) != 1 ||
			imported.Mappings[0].LocalBabyID != localBabyID || imported.Mappings[0].SharedBabyID != localBabyID {
			return fmt.Errorf("overlap import response mismatch: mappings=%d mapping_complete=%t manifest_complete=%t", len(imported.Mappings), imported.MappingComplete, imported.ManifestComplete)
		}
		importHint, hintErr := readHint(ctx, connection)
		if hintErr != nil || importHint.FamilyID != family.ID || importHint.Cursor.Sequence <= response.HighWater.Sequence {
			return fmt.Errorf("overlap import hint mismatch: %w", hintErr)
		}
		pulled, pullErr := c.pull(ctx, caregiver.AccessToken, &response.HighWater)
		if pullErr != nil || len(pulled.Changes) != 1 || pulled.NextPageToken != nil || pulled.HighWater != importHint.Cursor ||
			pulled.Changes[0].EntityKind != "baby" || pulled.Changes[0].EntityID != imported.Mappings[0].SharedBabyID {
			return fmt.Errorf("overlap import reconciliation changes=%d continuation=%t: %w", len(pulled.Changes), pulled.NextPageToken != nil, pullErr)
		}
		finalCursor = pulled.HighWater
	}
	state := savedState{Owner: owner, Caregiver: caregiver, Outsider: outsider, Outsider2: outsider2, FamilyID: family.ID, Cursor: finalCursor,
		FrozenTimerStart: frozenStart, DroppedTimerStart: dropped}
	if err = writeState(path, state); err != nil {
		return err
	}
	phase := "bootstrap"
	if includeImport {
		phase = "overlap"
	}
	fmt.Printf("%s route=/v1/live family_id=%s cursor=%d\n", phase, family.ID, finalCursor.Sequence)
	return nil
}

func (c client) waitReady(ctx context.Context) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/readyz", nil)
		if err != nil {
			return err
		}
		response, err := c.http.Do(request)
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c client) verify(ctx context.Context, path string) error {
	state, err := readState(path)
	if err != nil {
		return err
	}
	if len(state.FrozenTimerStart) == 0 || len(state.DroppedTimerStart.Outcomes) != 1 {
		return errors.New("restart state is missing the dropped timer exchange")
	}
	consumedRefresh := state.Owner.RefreshToken
	refreshed, err := c.refresh(ctx, consumedRefresh)
	if err != nil {
		return fmt.Errorf("consume refresh: %w", err)
	}
	if _, err = c.refresh(ctx, consumedRefresh); err == nil || !strings.Contains(err.Error(), "refresh_reused") {
		return fmt.Errorf("refresh reuse was not denied: %v", err)
	}
	if _, err = c.syncRaw(ctx, refreshed.AccessToken, state.FrozenTimerStart); err == nil {
		return errors.New("refresh successor remained usable after lineage reuse")
	}
	ownerClaim := &protocol.DeviceContinuityClaim{DeviceID: state.Owner.DeviceID, Credential: state.Owner.DeviceCredential}
	owner, err := c.signedExchange(ctx, "10000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000106", "installation-10000000-0000-4000-8000-000000000001", ownerClaim)
	if err != nil || owner.DeviceID != state.Owner.DeviceID || owner.Account.ID != state.Owner.Account.ID {
		return fmt.Errorf("owner continuity device=%q account=%q: %w", owner.DeviceID, owner.Account.ID, err)
	}
	if err = c.waitReady(ctx); err != nil {
		return fmt.Errorf("owner continuity projection: %w", err)
	}
	owner, err = c.refresh(ctx, owner.RefreshToken)
	if err != nil || owner.DeviceID != state.Owner.DeviceID || owner.Account.ID != state.Owner.Account.ID || owner.FamilySlot == nil || owner.FamilySlot.FamilyID != state.FamilyID {
		return fmt.Errorf("owner continuity refresh device=%q account=%q family_slot=%+v: %w", owner.DeviceID, owner.Account.ID, owner.FamilySlot, err)
	}
	owner.DeviceCredential = state.Owner.DeviceCredential
	connection, err := c.live(ctx, state.Caregiver.AccessToken)
	if err != nil {
		return err
	}
	defer connection.CloseNow()
	replay, err := c.syncRaw(ctx, owner.AccessToken, state.FrozenTimerStart)
	if err != nil || len(replay.Outcomes) != 1 || replay.Outcomes[0].Status != "already_applied" || replay.Outcomes[0].OriginalStatus == nil || *replay.Outcomes[0].OriginalStatus != "applied" {
		return fmt.Errorf("timer receipt replay=%+v: %w", replay, err)
	}
	original, recovered := state.DroppedTimerStart.Outcomes[0], replay.Outcomes[0]
	if original.Cursor == nil || recovered.Cursor == nil || *original.Cursor != *recovered.Cursor || original.CanonicalEntity == nil || recovered.CanonicalEntity == nil ||
		original.CanonicalEntity.Meta.AcceptedAt != recovered.CanonicalEntity.Meta.AcceptedAt {
		return fmt.Errorf("timer receipt changed cursor or accepted_at original=%+v recovered=%+v", original, recovered)
	}
	stopRequest := protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{
		protocol.CapabilityRecordsV1, protocol.CapabilityTimerCommandsV1}}, Cursor: &replay.HighWater, Mutations: []protocol.Mutation{{
		MutationID: "01000000-0000-4000-8000-000000000202", EntityID: "51000000-0000-4000-8000-000000000201",
		EntityKind: "timer", Operation: "timer_stop", PayloadVersion: 5, ClientMutationTime: "2026-09-20T15:10:00.000000Z",
		Payload: protocol.TimerStopPayload{BabyID: "11000000-0000-4000-8000-000000000101", CompletedRecordID: "21000000-0000-4000-8000-000000000201",
			StoppedAt: "2026-09-20T15:10:00.000000Z", ComponentID: "21100000-0000-4000-8000-000000000201"},
	}}}
	stopped, err := c.syncRequest(ctx, owner.AccessToken, stopRequest)
	if err != nil || len(stopped.Outcomes) != 1 || stopped.Outcomes[0].Status != "applied" || stopped.Outcomes[0].Cursor == nil {
		return fmt.Errorf("timer stop=%+v: %w", stopped, err)
	}
	started, ended := "2026-09-20T15:00:00.000000Z", "2026-09-20T15:10:00.000000Z"
	side, seconds := "left", uint32(600)
	expectedFeed := protocol.FeedPayload{BabyID: "11000000-0000-4000-8000-000000000101", Kind: "breast",
		StartedAt: started, EndedAt: &ended, TimeZoneID: "Europe/Zurich",
		Components: []protocol.FeedComponent{{
			ID:   "21100000-0000-4000-8000-000000000201",
			Kind: "breast", Side: &side, Seconds: &seconds, StartedAt: &started, EndedAt: &ended,
		}},
	}
	hint, err := readHint(ctx, connection)
	if err != nil || hint.Cursor != *stopped.Outcomes[0].Cursor || hint.FamilyID != state.FamilyID {
		return fmt.Errorf("stop hint mismatch: %w", err)
	}
	pulled, err := c.pull(ctx, state.Caregiver.AccessToken, &state.Cursor)
	if err != nil {
		return err
	}
	if err = validateCompletedFeed(stopped.Outcomes[0], pulled.Changes, "51000000-0000-4000-8000-000000000201",
		"21000000-0000-4000-8000-000000000201", expectedFeed); err != nil || pulled.HighWater != stopped.HighWater {
		return fmt.Errorf("caregiver completed-feed reconciliation high_water=%+v: %w", pulled.HighWater, err)
	}
	after, err := c.pull(ctx, state.Caregiver.AccessToken, &pulled.HighWater)
	if err != nil || len(after.Changes) != 0 {
		return fmt.Errorf("caregiver repeated stop changes=%d: %w", len(after.Changes), err)
	}
	if err = c.verifyImportIdentity(ctx, owner.AccessToken, state.Caregiver.AccessToken); err != nil {
		return err
	}
	beforeNegatives, err := c.pull(ctx, state.Caregiver.AccessToken, nil)
	if err != nil {
		return err
	}
	if _, err = c.signedExchange(ctx, "10000000-0000-4000-8000-000000000003", "30000000-0000-4000-8000-000000000103", "installation-outsider-continuity", ownerClaim); err == nil || !strings.Contains(err.Error(), "device_continuity_invalid") {
		return fmt.Errorf("cross-account continuity was not denied: %v", err)
	}
	missing := &protocol.DeviceContinuityClaim{DeviceID: state.Owner.DeviceID}
	if _, err = c.signedExchange(ctx, "10000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000104", "installation-10000000-0000-4000-8000-000000000001", missing); err == nil || !strings.Contains(err.Error(), "device_continuity_invalid") {
		return fmt.Errorf("missing continuity was not denied: %v", err)
	}
	corrupt := &protocol.DeviceContinuityClaim{DeviceID: state.Owner.DeviceID, Credential: strings.Repeat("A", 43)}
	if _, err = c.signedExchange(ctx, "10000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000105", "installation-10000000-0000-4000-8000-000000000001", corrupt); err == nil || !strings.Contains(err.Error(), "device_continuity_invalid") {
		return fmt.Errorf("corrupt continuity was not denied: %v", err)
	}
	afterNegatives, err := c.pull(ctx, state.Caregiver.AccessToken, &beforeNegatives.HighWater)
	if err != nil || len(afterNegatives.Changes) != 0 || afterNegatives.HighWater != beforeNegatives.HighWater {
		return fmt.Errorf("continuity negatives mutated family data changes=%d: %w", len(afterNegatives.Changes), err)
	}
	fmt.Printf("restart route=/v1/live family_id=%s cursor=%d continuity=restored receipt=byte-stable completed_feed=exactly-once imports=local-ids-and-dependent-records-preserved\n", state.FamilyID, stopped.HighWater.Sequence)
	return nil
}

func (c client) signedExchange(ctx context.Context, subject, commandID, installationID string, continuity *protocol.DeviceContinuityClaim) (protocol.SessionResponse, error) {
	var challenge protocol.ChallengeResponse
	if err := c.post(ctx, "/v1/auth/challenge", "", protocol.ChallengeRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: protocol.IdentityCapabilities()}}, &challenge); err != nil {
		return protocol.SessionResponse{}, err
	}
	nonce, err := identity.AppleNonce(challenge.CanonicalOrigin, challenge.ServerInstanceID, challenge.Challenge)
	if err != nil {
		return protocol.SessionResponse{}, err
	}
	var proof struct {
		IdentityToken string `json:"identity_token"`
	}
	if err = c.post(ctx, "/__identitytest/proof", "", map[string]string{"subject": subject, "nonce": nonce}, &proof); err != nil {
		return protocol.SessionResponse{}, err
	}
	return c.exchange(ctx, protocol.AppleExchangeRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: protocol.IdentityCapabilities()},
		CommandID: commandID, Challenge: challenge.Challenge, IdentityToken: proof.IdentityToken, AuthorizationCode: "identitytest-code-" + commandID,
		InstallationID: installationID, DeviceContinuity: continuity})
}

func (c client) verifyImportIdentity(ctx context.Context, ownerToken, caregiverToken string) error {
	baseline, err := c.pull(ctx, caregiverToken, nil)
	if err != nil {
		return fmt.Errorf("import reconciliation baseline: %w", err)
	}
	localIDs := []string{"12000000-0000-4000-8000-000000000201", "12000000-0000-4000-8000-000000000202"}
	recordIDs := []string{"22000000-0000-4000-8000-000000000211", "22000000-0000-4000-8000-000000000212"}
	preserve := protocol.ImportResolveRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1, protocol.CapabilityImportResolverV1}},
		ImportID: "22000000-0000-4000-8000-000000000201", ImportMode: "preserve_local_ids", NormalizationVersion: "nfc-trim-fold-nfc-v1",
		Babies: []protocol.ImportBaby{{LocalBabyID: localIDs[0], Name: "Same", BirthDate: "2026-02-03", SortOrder: 0},
			{LocalBabyID: localIDs[1], Name: "Same", BirthDate: "2026-02-03", SortOrder: 1}},
		Records: []protocol.RecordManifest{}, TotalRecordCount: 2, ChunkIndex: 0, Finalize: true}
	payloads := []protocol.FeedPayload{
		{BabyID: localIDs[0], Kind: "bottle", StartedAt: "2026-09-20T16:00:00.000000Z", TimeZoneID: "Europe/Zurich",
			Components: []protocol.FeedComponent{{ID: "22100000-0000-4000-8000-000000000211", Kind: "bottle", VolumeML: protocol.MustParseDecimal("1"), Contents: stringPointer("formula")}}},
		{BabyID: localIDs[1], Kind: "bottle", StartedAt: "2026-09-20T16:01:00.000000Z", TimeZoneID: "Europe/Zurich",
			Components: []protocol.FeedComponent{{ID: "22100000-0000-4000-8000-000000000212", Kind: "bottle", VolumeML: protocol.MustParseDecimal("2"), Contents: stringPointer("formula")}}},
	}
	for index, payload := range payloads {
		hash, hashErr := protocol.CanonicalContentHash("feed", payload)
		if hashErr != nil {
			return hashErr
		}
		preserve.Records = append(preserve.Records, protocol.RecordManifest{RecordID: recordIDs[index], EntityKind: "feed",
			LocalBabyID: localIDs[index], ContentSHA256: hex.EncodeToString(hash[:])})
	}
	var siblings protocol.ImportResolveResponse
	if err = c.request(ctx, http.MethodPost, "/v1/family/import/resolve", ownerToken, "records-v1,import-resolver-v1", preserve, &siblings); err != nil {
		return fmt.Errorf("equal-looking siblings collapsed mappings=%+v: %w", siblings.Mappings, err)
	}
	if err = validatePreservedMappings(siblings, localIDs); err != nil {
		return err
	}
	mutations := make([]protocol.Mutation, 0, len(recordIDs))
	for index, recordID := range recordIDs {
		mutations = append(mutations, protocol.Mutation{MutationID: fmt.Sprintf("23000000-0000-4000-8000-%012d", index+1),
			EntityID: recordID, EntityKind: "feed", Operation: "create", PayloadVersion: 4,
			ClientMutationTime: "2026-09-20T16:02:00.000000Z", ImportID: &preserve.ImportID, Payload: payloads[index]})
	}
	created, err := c.syncRequest(ctx, ownerToken, protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1",
		Capabilities: []string{protocol.CapabilityRecordsV1}}, Mutations: mutations})
	if err != nil || len(created.Outcomes) != len(mutations) || created.Outcomes[0].Status != "applied" || created.Outcomes[1].Status != "applied" {
		return fmt.Errorf("preserved dependent records outcomes=%+v: %w", created.Outcomes, err)
	}
	reconciled, err := c.pull(ctx, caregiverToken, &baseline.HighWater)
	if err != nil {
		return fmt.Errorf("caregiver import reconciliation: %w", err)
	}
	references := map[string]string{}
	for _, change := range reconciled.Changes {
		if change.EntityKind != "feed" {
			continue
		}
		payload, ok := change.SnapshotOrTombstone.Payload.(protocol.FeedPayload)
		if !ok {
			return fmt.Errorf("caregiver dependent record payload=%T", change.SnapshotOrTombstone.Payload)
		}
		references[change.EntityID] = payload.BabyID
	}
	for index, recordID := range recordIDs {
		if references[recordID] != localIDs[index] {
			return fmt.Errorf("caregiver dependent record %q baby=%q want=%q", recordID, references[recordID], localIDs[index])
		}
	}
	unique := preserve
	unique.ImportID = "22000000-0000-4000-8000-000000000202"
	unique.ImportMode = "match_shared"
	unique.Babies = []protocol.ImportBaby{{LocalBabyID: "12000000-0000-4000-8000-000000000203", Name: "Unique", BirthDate: "2026-03-04"}}
	unique.Records = []protocol.RecordManifest{}
	unique.TotalRecordCount = 0
	var owner protocol.ImportResolveResponse
	if err := c.request(ctx, http.MethodPost, "/v1/family/import/resolve", ownerToken, "records-v1,import-resolver-v1", unique, &owner); err != nil || len(owner.Mappings) != 1 {
		return fmt.Errorf("unique owner mapping=%+v: %w", owner.Mappings, err)
	}
	unique.ImportID = "22000000-0000-4000-8000-000000000203"
	unique.Babies[0].LocalBabyID = "12000000-0000-4000-8000-000000000204"
	var caregiver protocol.ImportResolveResponse
	if err := c.request(ctx, http.MethodPost, "/v1/family/import/resolve", caregiverToken, "records-v1,import-resolver-v1", unique, &caregiver); err != nil ||
		len(caregiver.Mappings) != 1 || caregiver.Mappings[0].SharedBabyID != owner.Mappings[0].SharedBabyID {
		return fmt.Errorf("cross-caregiver unique mismatch owner=%+v caregiver=%+v: %w", owner.Mappings, caregiver.Mappings, err)
	}
	return nil
}

func validatePreservedMappings(response protocol.ImportResolveResponse, localIDs []string) error {
	if !response.MappingComplete || len(response.Mappings) != len(localIDs) {
		return fmt.Errorf("preserved mapping count=%d complete=%t", len(response.Mappings), response.MappingComplete)
	}
	want := make(map[string]struct{}, len(localIDs))
	for _, id := range localIDs {
		want[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(response.Mappings))
	for _, mapping := range response.Mappings {
		if _, ok := want[mapping.LocalBabyID]; !ok || mapping.SharedBabyID != mapping.LocalBabyID {
			return fmt.Errorf("preserve_local_ids remapped local=%q shared=%q", mapping.LocalBabyID, mapping.SharedBabyID)
		}
		if _, duplicate := seen[mapping.LocalBabyID]; duplicate {
			return fmt.Errorf("duplicate preserved local baby %q", mapping.LocalBabyID)
		}
		seen[mapping.LocalBabyID] = struct{}{}
	}
	return nil
}

func validateCompletedFeed(outcome protocol.Outcome, changes []protocol.JournalChange, timerID, recordID string, expected protocol.FeedPayload) error {
	if len(outcome.RelatedEntities) != 1 {
		return fmt.Errorf("completed-feed outcome related count=%d", len(outcome.RelatedEntities))
	}
	completion := outcome.RelatedEntities[0]
	// Compared on the wire: a decoded v4 feed also carries the legacy
	// projection fields, which the v4 shape never sends.
	got, err := json.Marshal(completion.Payload)
	if err != nil {
		return err
	}
	want, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	if completion.EntityKind != "feed" || completion.Meta.ID != recordID || !bytes.Equal(got, want) {
		return fmt.Errorf("completed-feed outcome mismatch entity=%+v", completion)
	}
	timerStops, feeds := 0, 0
	for _, change := range changes {
		if change.EntityID == timerID && change.EntityKind == "timer" && change.Operation == "timer_stop" {
			timerStops++
		}
		if change.EntityID == recordID && change.EntityKind == "feed" && change.Operation == "timer_stop" {
			feeds++
			if !reflect.DeepEqual(change.SnapshotOrTombstone, completion) {
				return fmt.Errorf("caregiver completed feed differs from outcome: change=%+v outcome=%+v", change.SnapshotOrTombstone, completion)
			}
		}
	}
	if timerStops != 1 || feeds != 1 {
		return fmt.Errorf("timer-stop materializations=%d completed-feed materializations=%d", timerStops, feeds)
	}
	return nil
}

func (c client) signIn(ctx context.Context, subject, displayName string) (protocol.SessionResponse, error) {
	var challenge protocol.ChallengeResponse
	if err := c.post(ctx, "/v1/auth/challenge", "", protocol.ChallengeRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityIdentityV1}}}, &challenge); err != nil {
		return protocol.SessionResponse{}, err
	}
	nonce, err := identity.AppleNonce(challenge.CanonicalOrigin, challenge.ServerInstanceID, challenge.Challenge)
	if err != nil {
		return protocol.SessionResponse{}, err
	}
	var proof struct {
		IdentityToken string `json:"identity_token"`
	}
	if err = c.post(ctx, "/__identitytest/proof", "", map[string]string{"subject": subject, "nonce": nonce}, &proof); err != nil {
		return protocol.SessionResponse{}, err
	}
	return c.exchange(ctx, protocol.AppleExchangeRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityIdentityV1}},
		CommandID: strings.Replace(subject, "10000000", "30000000", 1), Challenge: challenge.Challenge, IdentityToken: proof.IdentityToken,
		AuthorizationCode: "identitytest-code-" + subject, InstallationID: "installation-" + subject,
		Profile: &protocol.ProfileInput{DisplayName: displayName, Confirmed: true}})
}

func (c client) exchange(ctx context.Context, request protocol.AppleExchangeRequest) (protocol.SessionResponse, error) {
	var info protocol.ServerInfo
	if err := c.requestRaw(ctx, http.MethodGet, "/v1/server", "", "", nil, &info); err != nil {
		return protocol.SessionResponse{}, err
	}
	switch info.AppleAuthorizationMode {
	case "identity_token_only":
		request.AuthorizationCode = ""
	case "code_exchange_required":
	default:
		return protocol.SessionResponse{}, errors.New("unknown Apple authorization mode")
	}
	var response protocol.SessionResponse
	err := c.post(ctx, "/v1/auth/apple", "", request, &response)
	return response, err
}

func (c client) refresh(ctx context.Context, token string) (protocol.SessionResponse, error) {
	var response protocol.SessionResponse
	err := c.post(ctx, "/v1/auth/refresh", "", protocol.RefreshRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityIdentityV1}}, RefreshToken: token}, &response)
	return response, err
}

func (c client) sync(ctx context.Context, token string, cursor *protocol.Cursor, mutationID, entityID string) (protocol.SyncResponse, error) {
	mutation := protocol.Mutation{MutationID: mutationID, EntityID: entityID, EntityKind: "baby", Operation: "create", PayloadVersion: 1,
		ClientMutationTime: time.Now().UTC().Format(time.RFC3339Nano), Payload: protocol.BabyPayload{Name: "Baby", BirthDate: "2026-01-12"}}
	return c.syncRequest(ctx, token, protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}}, Cursor: cursor, Mutations: []protocol.Mutation{mutation}})
}

func (c client) pull(ctx context.Context, token string, cursor *protocol.Cursor) (protocol.SyncResponse, error) {
	return c.syncRequest(ctx, token, protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityRecordsV1}}, Cursor: cursor, Mutations: []protocol.Mutation{}})
}

func (c client) syncRequest(ctx context.Context, token string, input protocol.SyncRequest) (protocol.SyncResponse, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return protocol.SyncResponse{}, err
	}
	return c.syncRaw(ctx, token, raw)
}

func (c client) syncRaw(ctx context.Context, token string, raw []byte) (protocol.SyncResponse, error) {
	var response protocol.SyncResponse
	var envelope protocol.SyncRequest
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return response, err
	}
	err := c.requestRaw(ctx, http.MethodPost, "/v1/sync", token, strings.Join(envelope.Capabilities, ","), raw, &response)
	return response, err
}

func (c client) dropCommittedSyncResponse(ctx context.Context, token string, raw []byte) (protocol.SyncResponse, error) {
	target, err := url.Parse(c.base)
	if err != nil {
		return protocol.SyncResponse{}, err
	}
	captured := make(chan protocol.SyncResponse, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		out := request.Clone(request.Context())
		out.URL.Scheme, out.URL.Host, out.Host, out.RequestURI = target.Scheme, target.Host, target.Host, ""
		response, roundTripErr := http.DefaultTransport.RoundTrip(out)
		if roundTripErr != nil {
			return
		}
		defer response.Body.Close()
		body, readErr := io.ReadAll(response.Body)
		if readErr != nil || response.StatusCode != http.StatusOK {
			return
		}
		var committed protocol.SyncResponse
		if json.Unmarshal(body, &committed) != nil {
			return
		}
		captured <- committed
		connection, _, hijackErr := writer.(http.Hijacker).Hijack()
		if hijackErr == nil {
			_ = connection.Close()
		}
	}))
	defer proxy.Close()
	dropClient := c
	dropClient.base = proxy.URL
	if _, requestErr := dropClient.syncRaw(ctx, token, raw); requestErr == nil {
		return protocol.SyncResponse{}, errors.New("committed timer response was not dropped")
	}
	select {
	case committed := <-captured:
		return committed, nil
	case <-ctx.Done():
		return protocol.SyncResponse{}, ctx.Err()
	}
}

func (c client) live(ctx context.Context, token string) (*websocket.Conn, error) {
	header := http.Header{"Authorization": []string{"Bearer " + token}, "X-Baby-Tracker-Protocol": []string{"v1"}, "X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}
	connection, response, err := websocket.Dial(ctx, c.base+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{realtime.Subprotocol}})
	if err != nil {
		status := 0
		body := ""
		if response != nil {
			status = response.StatusCode
			raw, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
			body = string(raw)
		}
		return nil, fmt.Errorf("live status=%d body=%s: %w", status, body, err)
	}
	return connection, nil
}

func readHint(ctx context.Context, connection *websocket.Conn) (realtime.Hint, error) {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, raw, err := connection.Read(readCtx)
	if err != nil {
		return realtime.Hint{}, err
	}
	var hint realtime.Hint
	if err = json.Unmarshal(raw, &hint); err != nil {
		return realtime.Hint{}, err
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) != 2 {
		return realtime.Hint{}, errors.New("hint was not cursor-only")
	}
	return hint, nil
}

func (c client) post(ctx context.Context, path, token string, input, output any) error {
	return c.request(ctx, http.MethodPost, path, token, "", input, output)
}

func (c client) request(ctx context.Context, method, path, token, capabilities string, input, output any) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return c.requestRaw(ctx, method, path, token, capabilities, raw, output)
}

func (c client) requestRaw(ctx context.Context, method, path, token, capabilities string, raw []byte, output any) error {
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if capabilities != "" {
		request.Header.Set("X-Baby-Tracker-Protocol", "v1")
		request.Header.Set("X-Baby-Tracker-Capabilities", capabilities)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("%s %s status=%d body=%s", method, path, response.StatusCode, body)
	}
	return json.NewDecoder(response.Body).Decode(output)
}

func stringPointer(value string) *string { return &value }

func writeState(path string, state savedState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(raw); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func readState(path string) (savedState, error) {
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		return savedState{}, errors.New("state file is unavailable or not private")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return savedState{}, err
	}
	var state savedState
	err = json.Unmarshal(raw, &state)
	return state, err
}
