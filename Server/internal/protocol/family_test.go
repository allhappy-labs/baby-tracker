package protocol

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestFamilyRequestDTOsAreStrictVersionedCommands(t *testing.T) {
	tests := []struct {
		name string
		body string
		new  func() any
	}{
		{"profile", `{"protocol_version":"v1","capabilities":["family-lifecycle-v1"],"command_id":"10000000-0000-4000-8000-000000000001","display_name":"Parent","confirmed":true}`, func() any { return &UpdateAccountRequest{} }},
		{"family profile repair", `{"protocol_version":"v1","capabilities":["family-lifecycle-v1"],"command_id":"10000000-0000-4000-8000-000000000006","display_name":"Our family","confirmed":true}`, func() any { return &RepairFamilyProfileRequest{} }},
		{"create family", `{"protocol_version":"v1","capabilities":["family-lifecycle-v1"],"command_id":"10000000-0000-4000-8000-000000000002","display_name":"Our family"}`, func() any { return &CreateFamilyRequest{} }},
		{"create invitation", `{"protocol_version":"v1","capabilities":["family-lifecycle-v1"],"command_id":"10000000-0000-4000-8000-000000000003"}`, func() any { return &CommandRequest{} }},
		{"preview", `{"protocol_version":"v1","capabilities":["family-lifecycle-v1"],"token":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`, func() any { return &PreviewInvitationRequest{} }},
		{"redeem", `{"protocol_version":"v1","capabilities":["family-lifecycle-v1"],"command_id":"10000000-0000-4000-8000-000000000004","confirmed":true,"token":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`, func() any { return &RedeemInvitationRequest{} }},
		{"transfer", `{"protocol_version":"v1","capabilities":["family-lifecycle-v1"],"command_id":"10000000-0000-4000-8000-000000000005","new_owner_account_id":"20000000-0000-4000-8000-000000000001"}`, func() any { return &TransferOwnershipRequest{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := DecodeRequest(strings.NewReader(test.body), 64<<10, test.new()); err != nil {
				t.Fatal(err)
			}
			unknown := strings.TrimSuffix(test.body, "}") + `,"unknown":true}`
			if err := DecodeRequest(strings.NewReader(unknown), 64<<10, test.new()); ErrorCodeOf(err) != "invalid_request" {
				t.Fatalf("unknown field code=%q err=%v", ErrorCodeOf(err), err)
			}
		})
	}
}

func TestFamilyActorPageDTOUsesStableSnapshotCursors(t *testing.T) {
	page := FamilyActorPageResponse{
		ProtocolVersion: "v1",
		Actors: []FamilyActorResponse{
			{AccountID: "20000000-0000-4000-8000-000000000001", DisplayName: "Parent", Former: false},
			{AccountID: "20000000-0000-4000-8000-000000000002", DisplayName: "Former caregiver", Former: true},
		},
		HighWater:  "42",
		NextCursor: "20000000-0000-4000-8000-000000000002",
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocol_version":"v1","actors":[{"account_id":"20000000-0000-4000-8000-000000000001","display_name":"Parent","former":false},{"account_id":"20000000-0000-4000-8000-000000000002","display_name":"Former caregiver","former":true}],"high_water":"42","next_cursor":"20000000-0000-4000-8000-000000000002"}`
	if string(encoded) != want {
		t.Fatalf("actor page=%s want=%s", encoded, want)
	}

	page.NextCursor = ""
	encoded, err = json.Marshal(page)
	if err != nil || strings.Contains(string(encoded), "next_cursor") {
		t.Fatalf("terminal actor page=%s err=%v", encoded, err)
	}
}

func TestFamilyCapabilitiesAreAdvertisedWithoutSync(t *testing.T) {
	want := []string{CapabilityIdentityV1, CapabilityFamilyLifecycleV1}
	if got := FamilyCapabilities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("capabilities=%v want=%v", got, want)
	}
	for _, forbidden := range []string{"sync-v1", "family_sync", CapabilityRecoveryV1} {
		if err := ValidateCapabilities([]string{forbidden}, FamilyCapabilities()); ErrorCodeOf(err) != "unsupported_capability" {
			t.Fatalf("capability %q unexpectedly supported: %v", forbidden, err)
		}
	}
}

func TestRecoveryCapabilitiesAreRouteSpecificAndAdvertised(t *testing.T) {
	if got, want := RecoveryCapabilities(), []string{CapabilityRecoveryV1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recovery capabilities=%v want=%v", got, want)
	}
	if got, want := ServerCapabilities(), []string{CapabilityIdentityV1, CapabilityFamilyLifecycleV1, CapabilityRecoveryV1, CapabilityRecordsV1, CapabilitySnapshotV1, CapabilityTimerCommandsV1, CapabilityImportResolverV1, CapabilityLiveHintsV1, CapabilityExportV1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("server capabilities=%v want=%v", got, want)
	}
	request := &ConfirmedCommandRequest{RequestMeta: RequestMeta{ProtocolVersion: "v1", Capabilities: []string{CapabilityRecoveryV1}},
		CommandID: "10000000-0000-4000-8000-000000000009", Confirmed: true}
	if err := ValidateRecoveryRequestCapabilities(request); err != nil {
		t.Fatalf("recovery capability rejected: %v", err)
	}
	for _, capabilities := range [][]string{{}, {CapabilityFamilyLifecycleV1}, {CapabilityRecoveryV1, CapabilityFamilyLifecycleV1}} {
		request.Capabilities = capabilities
		if err := ValidateRecoveryRequestCapabilities(request); ErrorCodeOf(err) != "unsupported_capability" {
			t.Fatalf("capabilities=%v code=%q err=%v", capabilities, ErrorCodeOf(err), err)
		}
	}
}

func TestFamilyFixturesMatchWireDTOs(t *testing.T) {
	data, err := os.ReadFile("../../api/fixtures/family.json")
	if err != nil {
		t.Fatal(err)
	}
	var familyFixture struct {
		CreateRequest  CreateFamilyRequest        `json:"create_request"`
		RepairRequest  RepairFamilyProfileRequest `json:"repair_request"`
		RepairResponse FamilyResponse             `json:"repair_response"`
		Response       FamilyResponse             `json:"response"`
	}
	if err := json.Unmarshal(data, &familyFixture); err != nil {
		t.Fatal(err)
	}
	if familyFixture.Response.ProtocolVersion != "v1" || familyFixture.Response.ID == "" || len(familyFixture.Response.Members) != 2 || familyFixture.Response.Members[0].DisplayName == "" {
		t.Fatalf("family fixture=%+v", familyFixture)
	}
	if !familyFixture.RepairRequest.Confirmed || familyFixture.RepairRequest.DisplayName == "" || familyFixture.RepairResponse.ProfileRequired {
		t.Fatalf("family repair fixture=%+v", familyFixture)
	}
	encoded, _ := json.Marshal(familyFixture.CreateRequest)
	if err := DecodeRequest(strings.NewReader(string(encoded)), 64<<10, &CreateFamilyRequest{}); err != nil {
		t.Fatalf("strict create fixture: %v", err)
	}

	data, err = os.ReadFile("../../api/fixtures/family-invitation.json")
	if err != nil {
		t.Fatal(err)
	}
	var invitationFixture struct {
		CreateResponse  InvitationResponse        `json:"create_response"`
		ReplayResponse  InvitationResponse        `json:"replay_response"`
		PreviewRequest  PreviewInvitationRequest  `json:"preview_request"`
		PreviewResponse InvitationPreviewResponse `json:"preview_response"`
		RedeemRequest   RedeemInvitationRequest   `json:"redeem_request"`
	}
	if err := json.Unmarshal(data, &invitationFixture); err != nil {
		t.Fatal(err)
	}
	if len(invitationFixture.CreateResponse.Token) != 43 || invitationFixture.CreateResponse.SecretUnavailable ||
		!strings.Contains(invitationFixture.CreateResponse.Link, "#token=") || !invitationFixture.ReplayResponse.SecretUnavailable ||
		invitationFixture.ReplayResponse.Token != "" || invitationFixture.ReplayResponse.Link != "" ||
		invitationFixture.PreviewResponse.FamilyDisplayName == "" || !invitationFixture.RedeemRequest.Confirmed {
		t.Fatalf("invitation fixture=%+v", invitationFixture)
	}
	for _, request := range []any{&invitationFixture.PreviewRequest, &invitationFixture.RedeemRequest} {
		encoded, _ := json.Marshal(request)
		if err := DecodeRequest(strings.NewReader(string(encoded)), 64<<10, request); err != nil {
			t.Fatalf("strict invitation fixture: %v", err)
		}
	}

	data, err = os.ReadFile("../../api/fixtures/family-actors.json")
	if err != nil {
		t.Fatal(err)
	}
	var actorFixture struct {
		FirstPage        FamilyActorPageResponse `json:"first_page"`
		ContinuationPage FamilyActorPageResponse `json:"continuation_page"`
	}
	if err := json.Unmarshal(data, &actorFixture); err != nil {
		t.Fatal(err)
	}
	if actorFixture.FirstPage.ProtocolVersion != "v1" || len(actorFixture.FirstPage.Actors) != 2 ||
		actorFixture.FirstPage.HighWater == "" || actorFixture.FirstPage.NextCursor == "" ||
		len(actorFixture.ContinuationPage.Actors) != 1 || !actorFixture.ContinuationPage.Actors[0].Former ||
		actorFixture.ContinuationPage.Actors[0].DisplayName != "Former caregiver" || actorFixture.ContinuationPage.NextCursor != "" {
		t.Fatalf("actor fixture=%+v", actorFixture)
	}

	data, err = os.ReadFile("../../api/fixtures/family-slot-conflict.json")
	if err != nil {
		t.Fatal(err)
	}
	var conflict ErrorResponse
	if err := json.Unmarshal(data, &conflict); err != nil || conflict.Validate() != nil || conflict.Error.Code != "family_slot_occupied" || conflict.Error.Retryable {
		t.Fatalf("slot conflict=%+v err=%v", conflict, err)
	}
}
