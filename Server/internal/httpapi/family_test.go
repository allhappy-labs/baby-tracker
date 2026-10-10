package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
)

type familyServiceStub struct {
	account    func(context.Context, access.Principal) (family.AccountView, error)
	profile    func(context.Context, access.Principal, string, string, bool) (family.AccountView, error)
	create     func(context.Context, access.Principal, string, string) (family.FamilyView, error)
	current    func(context.Context, access.Principal) (family.FamilyView, error)
	invite     func(context.Context, access.Principal, string, string) (family.InvitationSecret, error)
	invitation func(context.Context, access.Principal, string) (family.InvitationView, error)
	pending    func(context.Context, access.Principal) ([]family.InvitationView, error)
	preview    func(context.Context, access.Principal, string) (family.InvitationPreview, error)
	redeem     func(context.Context, access.Principal, string, string, bool) (family.FamilyView, error)
	revoke     func(context.Context, access.Principal, string, string) (authority.Result, error)
	transfer   func(context.Context, access.Principal, string, string) (authority.Result, error)
	changeRole func(context.Context, access.Principal, string, string, string) (authority.Result, error)
	remove     func(context.Context, access.Principal, string, string) (authority.Result, error)
	leave      func(context.Context, access.Principal, string) (authority.Result, error)
	repair     func(context.Context, access.Principal, string, string, bool) (family.FamilyView, error)
	actors     func(context.Context, access.Principal, string, string, int) (family.ActorLabelPage, error)
	schedule   func(context.Context, access.Principal, string, bool) (family.RecoveryStatus, error)
	recovery   func(context.Context, access.Principal) (family.RecoveryStatus, error)
	recover    func(context.Context, access.Principal, string, bool) (authority.Result, error)
	forfeit    func(context.Context, access.Principal, string, bool) (authority.Result, error)
}

func (s familyServiceStub) ChangeRole(ctx context.Context, p access.Principal, commandID, accountID, role string) (authority.Result, error) {
	if s.changeRole == nil {
		return authority.Result{}, nil
	}
	return s.changeRole(ctx, p, commandID, accountID, role)
}

func (s familyServiceStub) Account(ctx context.Context, p access.Principal) (family.AccountView, error) {
	return s.account(ctx, p)
}
func (s familyServiceStub) UpdateProfile(ctx context.Context, p access.Principal, commandID, name string, confirmed bool) (family.AccountView, error) {
	return s.profile(ctx, p, commandID, name, confirmed)
}
func (s familyServiceStub) Create(ctx context.Context, p access.Principal, commandID, name string) (family.FamilyView, error) {
	return s.create(ctx, p, commandID, name)
}
func (s familyServiceStub) Current(ctx context.Context, p access.Principal) (family.FamilyView, error) {
	return s.current(ctx, p)
}
func (s familyServiceStub) Invite(ctx context.Context, p access.Principal, commandID, role string) (family.InvitationSecret, error) {
	return s.invite(ctx, p, commandID, role)
}
func (s familyServiceStub) Invitation(ctx context.Context, p access.Principal, id string) (family.InvitationView, error) {
	return s.invitation(ctx, p, id)
}
func (s familyServiceStub) PendingInvitations(ctx context.Context, p access.Principal) ([]family.InvitationView, error) {
	return s.pending(ctx, p)
}
func (s familyServiceStub) Preview(ctx context.Context, p access.Principal, token string) (family.InvitationPreview, error) {
	return s.preview(ctx, p, token)
}
func (s familyServiceStub) Redeem(ctx context.Context, p access.Principal, commandID, token string, confirmed bool) (family.FamilyView, error) {
	return s.redeem(ctx, p, commandID, token, confirmed)
}
func (s familyServiceStub) RevokeInvitation(ctx context.Context, p access.Principal, commandID, id string) (authority.Result, error) {
	return s.revoke(ctx, p, commandID, id)
}
func (s familyServiceStub) Transfer(ctx context.Context, p access.Principal, commandID, accountID string) (authority.Result, error) {
	return s.transfer(ctx, p, commandID, accountID)
}
func (s familyServiceStub) Remove(ctx context.Context, p access.Principal, commandID, accountID string) (authority.Result, error) {
	return s.remove(ctx, p, commandID, accountID)
}
func (s familyServiceStub) Leave(ctx context.Context, p access.Principal, commandID string) (authority.Result, error) {
	return s.leave(ctx, p, commandID)
}
func (s familyServiceStub) RepairProfile(ctx context.Context, p access.Principal, commandID, displayName string, confirmed bool) (family.FamilyView, error) {
	return s.repair(ctx, p, commandID, displayName, confirmed)
}
func (s familyServiceStub) ListActors(ctx context.Context, p access.Principal, after, highWater string, limit int) (family.ActorLabelPage, error) {
	return s.actors(ctx, p, after, highWater, limit)
}
func (s familyServiceStub) ScheduleDeletion(ctx context.Context, p access.Principal, commandID string, confirmed bool) (family.RecoveryStatus, error) {
	return s.schedule(ctx, p, commandID, confirmed)
}
func (s familyServiceStub) RecoveryStatus(ctx context.Context, p access.Principal) (family.RecoveryStatus, error) {
	return s.recovery(ctx, p)
}
func (s familyServiceStub) Recover(ctx context.Context, p access.Principal, commandID string, confirmed bool) (authority.Result, error) {
	return s.recover(ctx, p, commandID, confirmed)
}
func (s familyServiceStub) Forfeit(ctx context.Context, p access.Principal, commandID string, confirmed bool) (authority.Result, error) {
	return s.forfeit(ctx, p, commandID, confirmed)
}

func TestFamilyRoutesAreRegisteredExactly(t *testing.T) {
	want := []Route{
		{Method: http.MethodGet, Pattern: "/v1/account"},
		{Method: http.MethodPatch, Pattern: "/v1/account"},
		{Method: http.MethodPost, Pattern: "/v1/account/delete"},
		{Method: http.MethodPost, Pattern: "/v1/family"},
		{Method: http.MethodGet, Pattern: "/v1/family"},
		{Method: http.MethodPatch, Pattern: "/v1/family"},
		{Method: http.MethodGet, Pattern: "/v1/family/actors"},
		{Method: http.MethodPost, Pattern: "/v1/family/invitations"},
		{Method: http.MethodGet, Pattern: "/v1/family/invitations"},
		{Method: http.MethodGet, Pattern: "/v1/family/invitations/{id}"},
		{Method: http.MethodPost, Pattern: "/v1/family/invitations/{id}/revoke"},
		{Method: http.MethodPost, Pattern: "/v1/family/invitations/preview"},
		{Method: http.MethodPost, Pattern: "/v1/family/invitations/redeem"},
		{Method: http.MethodPost, Pattern: "/v1/family/members/{account_id}/remove"},
		{Method: http.MethodPost, Pattern: "/v1/family/ownership-transfer"},
		{Method: http.MethodPost, Pattern: "/v1/family/leave"},
		{Method: http.MethodPost, Pattern: "/v1/family/delete"},
		{Method: http.MethodGet, Pattern: "/v1/family/recovery"},
		{Method: http.MethodPost, Pattern: "/v1/family/recover"},
		{Method: http.MethodPost, Pattern: "/v1/family/recovery/forfeit"},
		{Method: http.MethodGet, Pattern: "/invite"},
	}
	got := Routes()
	for _, route := range want {
		if !containsRoute(got, route) {
			t.Errorf("route missing: %+v", route)
		}
	}
}

func TestFamilyProfileRepairAndActorPagesMapStrictWireContracts(t *testing.T) {
	principal := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001"}
	familyID := "30000000-0000-4000-8000-000000000001"
	commandID := "10000000-0000-4000-8000-000000000006"
	highWater := "42"
	after := "20000000-0000-4000-8000-000000000002"
	var repairCalls, actorCalls int
	service := familyServiceStub{
		repair: func(_ context.Context, got access.Principal, gotCommandID, name string, confirmed bool) (family.FamilyView, error) {
			repairCalls++
			if got != principal || gotCommandID != commandID || name != "Recovered family" || !confirmed {
				t.Fatalf("repair principal=%+v command=%q name=%q confirmed=%t", got, gotCommandID, name, confirmed)
			}
			return family.FamilyView{ID: familyID, DisplayName: name, Status: "active", Members: []family.MemberView{}}, nil
		},
		actors: func(_ context.Context, got access.Principal, gotAfter, gotHighWater string, limit int) (family.ActorLabelPage, error) {
			actorCalls++
			if got != principal || gotAfter != after || gotHighWater != highWater || limit != 25 {
				t.Fatalf("actors principal=%+v after=%q high_water=%q limit=%d", got, gotAfter, gotHighWater, limit)
			}
			return family.ActorLabelPage{Items: []family.ActorLabelView{
				{AccountID: principal.AccountID, DisplayName: "Parent"},
				{AccountID: after, DisplayName: "Former caregiver", Former: true},
			}, HighWater: highWater, NextCursor: after}, nil
		},
	}
	handler := New(familyHandlerOptions(t, principal, service, nil))

	repair := httptest.NewRequest(http.MethodPatch, "/v1/family", strings.NewReader(familyBody(commandID, `"display_name":"Recovered family","confirmed":true`)))
	repair.Header.Set("Authorization", "Bearer access")
	repairRecord := httptest.NewRecorder()
	handler.ServeHTTP(repairRecord, repair)
	if repairRecord.Code != http.StatusOK || repairRecord.Header().Get("Cache-Control") != "no-store" || !strings.Contains(repairRecord.Body.String(), `"display_name":"Recovered family"`) {
		t.Fatalf("repair status=%d headers=%v body=%s", repairRecord.Code, repairRecord.Header(), repairRecord.Body.String())
	}

	actors := httptest.NewRequest(http.MethodGet, "/v1/family/actors?after="+after+"&high_water="+highWater+"&limit=25", nil)
	actors.Header.Set("Authorization", "Bearer access")
	actorRecord := httptest.NewRecorder()
	handler.ServeHTTP(actorRecord, actors)
	if actorRecord.Code != http.StatusOK || actorRecord.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(actorRecord.Body.String(), `"display_name":"Former caregiver","former":true`) ||
		!strings.Contains(actorRecord.Body.String(), `"high_water":"`+highWater+`"`) ||
		!strings.Contains(actorRecord.Body.String(), `"next_cursor":"`+after+`"`) {
		t.Fatalf("actors status=%d headers=%v body=%s", actorRecord.Code, actorRecord.Header(), actorRecord.Body.String())
	}
	if repairCalls != 1 || actorCalls != 1 {
		t.Fatalf("repair calls=%d actor calls=%d", repairCalls, actorCalls)
	}
}

func TestFamilyActorPagesDefaultLimitAndRejectAmbiguousQueries(t *testing.T) {
	principal := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001"}
	calls := 0
	service := familyServiceStub{actors: func(_ context.Context, _ access.Principal, after, highWater string, limit int) (family.ActorLabelPage, error) {
		calls++
		if after != "" || highWater != "" || limit != 50 {
			t.Fatalf("default query after=%q high_water=%q limit=%d", after, highWater, limit)
		}
		return family.ActorLabelPage{Items: []family.ActorLabelView{}, HighWater: "42"}, nil
	}}
	handler := New(familyHandlerOptions(t, principal, service, nil))

	valid := httptest.NewRequest(http.MethodGet, "/v1/family/actors", nil)
	valid.Header.Set("Authorization", "Bearer access")
	validRecord := httptest.NewRecorder()
	handler.ServeHTTP(validRecord, valid)
	if validRecord.Code != http.StatusOK || !strings.Contains(validRecord.Body.String(), `"actors":[]`) {
		t.Fatalf("default status=%d body=%s", validRecord.Code, validRecord.Body.String())
	}

	badQueries := []string{
		"?limit=0", "?limit=101", "?limit=01", "?limit=not-a-number", "?limit=50&limit=51",
		"?after=&high_water=",
		"?after=20000000-0000-4000-8000-000000000001", "?high_water=42",
		"?after=20000000-0000-4000-8000-000000000001&high_water=42&extra=true",
		"?after=20000000-0000-4000-8000-00000000000A&high_water=42",
		"?after=00000000-0000-0000-0000-000000000000&high_water=42",
		"?after=20000000-0000-4000-8000-000000000002&high_water=0",
		"?after=20000000-0000-4000-8000-000000000002&high_water=01",
		"?after=20000000-0000-4000-8000-000000000002&high_water=9007199254740992",
	}
	for _, query := range badQueries {
		t.Run(query, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/family/actors"+query, nil)
			req.Header.Set("Authorization", "Bearer access")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest || rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), `"code":"invalid_request"`) {
				t.Fatalf("query=%q status=%d headers=%v body=%s", query, rec.Code, rec.Header(), rec.Body.String())
			}
		})
	}
	if calls != 1 {
		t.Fatalf("service calls=%d want=1", calls)
	}
}

func TestInvitationHandoffIsStaticPrivacySafeAndHashPinned(t *testing.T) {
	var logs strings.Builder
	handler := New(Options{AppleAuthorizationMode: "identity_token_only", Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/invite?token=server-visible-secret", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("privacy headers=%v", recorder.Header())
	}
	body := recorder.Body.String()
	if strings.Contains(body, "server-visible-secret") || strings.Contains(logs.String(), "server-visible-secret") {
		t.Fatalf("query token leaked: body=%s logs=%s", body, logs.String())
	}
	if strings.Contains(body, "fetch(") || strings.Contains(body, "XMLHttpRequest") || strings.Contains(body, "http://") || strings.Contains(body, "https://") ||
		strings.Contains(body, "location.assign") || strings.Contains(body, "location.replace") {
		t.Fatalf("handoff contains network, third-party, or automatic navigation: %s", body)
	}
	for _, contract := range []string{
		`new URLSearchParams(location.hash.slice(1))`,
		`fragment.getAll('token').length !== 1`,
		`history.replaceState(null, '', '/invite')`,
		`/^[A-Za-z0-9_-]{43}$/`,
		`navigator.clipboard.writeText(token)`,
		`babytracker://invite?server=`,
		`encodeURIComponent(location.origin)`,
	} {
		if !strings.Contains(body, contract) {
			t.Errorf("handoff omitted %q", contract)
		}
	}
	if tokenWrite := strings.Index(body, ".value = token"); tokenWrite < 0 || strings.Index(body, "history.replaceState") > tokenWrite {
		t.Fatal("fragment is not cleared before token rendering")
	}
	scriptPattern := regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	match := scriptPattern.FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("expected exactly one embedded script")
	}
	digest := sha256.Sum256([]byte(match[1]))
	wantHash := "'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'"
	csp := recorder.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src "+wantHash) {
		t.Fatalf("CSP=%q want script hash %s", csp, wantHash)
	}
	if strings.Count(body, "<script") != 1 {
		t.Fatalf("expected exactly one script tag, got %d", strings.Count(body, "<script"))
	}
	stylePattern := regexp.MustCompile(`(?s)<style>(.*?)</style>`)
	styles := stylePattern.FindAllStringSubmatch(body, -1)
	if len(styles) != 1 || strings.Count(body, "<style") != 1 {
		t.Fatalf("expected exactly one embedded style block, got %d", len(styles))
	}
	styleDigest := sha256.Sum256([]byte(styles[0][1]))
	wantStyleHash := "'sha256-" + base64.StdEncoding.EncodeToString(styleDigest[:]) + "'"
	for _, directive := range []string{"default-src 'none'", "img-src 'none'", "connect-src 'none'", "style-src " + wantStyleHash} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP=%q missing %q", csp, directive)
		}
	}
	for _, forbidden := range []string{"<img", "url(", "@import", "https://", "http://", "<link"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("handoff contains external-resource vector %q", forbidden)
		}
	}
	for _, required := range []string{
		"prefers-color-scheme",
		"prefers-reduced-motion",
		`aria-live="polite"`,
		"You're invited to a family",
		"Join to log feeds, sleep and diapers together.",
		"Open in Baby Tracker",
		"Copy code",
		"This invitation link isn't complete",
		"Ask for a new invitation.",
		"Each invitation works once and expires after 7 days.",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("handoff omitted %q", required)
		}
	}
}

func TestInvitationHandoffCSPRequiresScriptAndStyle(t *testing.T) {
	for name, document := range map[string]string{
		"missing script": "<style>a{}</style>",
		"missing style":  "<script>1</script>",
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("handoffCSP did not panic")
				}
			}()
			handoffCSP(document)
		})
	}
}

func TestAccountAndFamilyRoutesAuthenticateStrictlyAndMapDTOs(t *testing.T) {
	principal := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001", SessionID: "22000000-0000-4000-8000-000000000001", RecoveryEpoch: 7}
	familyID := "30000000-0000-4000-8000-000000000001"
	invitationID := "40000000-0000-4000-8000-000000000001"
	expires := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	view := family.FamilyView{ID: familyID, DisplayName: "Our family", Status: "active", Members: []family.MemberView{{AccountID: principal.AccountID, DisplayName: "Parent", Role: "owner", Status: "active"}}}
	command := authority.Result{CommandID: "10000000-0000-4000-8000-000000000009", Outcome: "applied", Code: "ok", Revision: 9}
	var calls []string
	service := familyServiceStub{
		account: func(_ context.Context, got access.Principal) (family.AccountView, error) {
			if got != principal {
				t.Fatalf("principal=%+v", got)
			}
			calls = append(calls, "account")
			return family.AccountView{ID: principal.AccountID, DisplayName: "Parent", Status: "active", FamilySlot: &family.FamilySlotView{FamilyID: familyID, State: "active"}}, nil
		},
		profile: func(_ context.Context, _ access.Principal, commandID, name string, confirmed bool) (family.AccountView, error) {
			calls = append(calls, "profile:"+commandID+":"+name)
			if !confirmed {
				t.Fatal("profile confirmation not forwarded")
			}
			return family.AccountView{ID: principal.AccountID, DisplayName: name, Status: "active"}, nil
		},
		create: func(_ context.Context, _ access.Principal, commandID, name string) (family.FamilyView, error) {
			calls = append(calls, "create:"+commandID+":"+name)
			return view, nil
		},
		current: func(context.Context, access.Principal) (family.FamilyView, error) {
			calls = append(calls, "current")
			return view, nil
		},
		invite: func(_ context.Context, _ access.Principal, commandID, role string) (family.InvitationSecret, error) {
			calls = append(calls, "invite:"+commandID+":"+role)
			return family.InvitationSecret{Invitation: family.InvitationView{ID: invitationID, FamilyID: familyID, Status: "active", ExpiresAt: expires}, Token: strings.Repeat("A", 43), Link: "https://sync.example.test/invite#token=" + strings.Repeat("A", 43)}, nil
		},
		invitation: func(_ context.Context, _ access.Principal, id string) (family.InvitationView, error) {
			calls = append(calls, "invitation:"+id)
			return family.InvitationView{ID: id, FamilyID: familyID, Role: "caregiver", Status: "active", ExpiresAt: expires, CreatedAt: expires.Add(-24 * time.Hour)}, nil
		},
		pending: func(_ context.Context, _ access.Principal) ([]family.InvitationView, error) {
			calls = append(calls, "pending")
			return []family.InvitationView{{ID: invitationID, FamilyID: familyID, Role: "viewer", Status: "active", ExpiresAt: expires, CreatedAt: expires.Add(-24 * time.Hour)}}, nil
		},
		preview: func(_ context.Context, _ access.Principal, token string) (family.InvitationPreview, error) {
			calls = append(calls, "preview:"+token)
			return family.InvitationPreview{FamilyDisplayName: "Our family", ExpiresAt: expires}, nil
		},
		redeem: func(_ context.Context, _ access.Principal, commandID, token string, confirmed bool) (family.FamilyView, error) {
			calls = append(calls, "redeem:"+commandID+":"+token)
			if !confirmed {
				t.Fatal("redeem confirmation not forwarded")
			}
			return view, nil
		},
		revoke: func(_ context.Context, _ access.Principal, commandID, id string) (authority.Result, error) {
			calls = append(calls, "revoke:"+commandID+":"+id)
			return command, nil
		},
		transfer: func(_ context.Context, _ access.Principal, commandID, id string) (authority.Result, error) {
			calls = append(calls, "transfer:"+commandID+":"+id)
			return command, nil
		},
		remove: func(_ context.Context, _ access.Principal, commandID, id string) (authority.Result, error) {
			calls = append(calls, "remove:"+commandID+":"+id)
			return command, nil
		},
		// The role route was in the router and in the handler table and was
		// nonetheless unreachable: `MemberRoleRequest` was missing from the
		// capability switch, so every body — owner's, well formed, correct
		// capabilities — came back as invalid_request. Nothing exercised the
		// route with a real body, so nothing noticed.
		changeRole: func(_ context.Context, _ access.Principal, commandID, id, role string) (authority.Result, error) {
			calls = append(calls, "role:"+commandID+":"+id+":"+role)
			return command, nil
		},
		leave: func(_ context.Context, _ access.Principal, commandID string) (authority.Result, error) {
			calls = append(calls, "leave:"+commandID)
			return command, nil
		},
	}
	options := familyHandlerOptions(t, principal, service, nil)
	handler := New(options)
	commandID := "10000000-0000-4000-8000-000000000009"
	token := strings.Repeat("A", 43)
	tests := []struct {
		method, path, body string
		status             int
		contains           string
	}{
		{http.MethodGet, "/v1/account", "", 200, `"display_name":"Parent"`},
		{http.MethodPatch, "/v1/account", familyBody(commandID, `"display_name":"Parent Two","confirmed":true`), 200, `"display_name":"Parent Two"`},
		{http.MethodPost, "/v1/family", familyBody(commandID, `"display_name":"Our family"`), 201, `"display_name":"Our family"`},
		{http.MethodGet, "/v1/family", "", 200, `"role":"owner"`},
		{http.MethodPost, "/v1/family/invitations", familyBody(commandID, ""), 201, `"token":"` + token + `"`},
		{http.MethodGet, "/v1/family/invitations", "", 200, `"invitations":[{"id":"` + invitationID + `"`},
		{http.MethodGet, "/v1/family/invitations/" + invitationID, "", 200, `"id":"` + invitationID + `"`},
		{http.MethodPost, "/v1/family/invitations/preview", familyMeta(`"token":"` + token + `"`), 200, `"family_display_name":"Our family"`},
		{http.MethodPost, "/v1/family/invitations/redeem", familyBody(commandID, `"confirmed":true,"token":"`+token+`"`), 200, `"id":"` + familyID + `"`},
		{http.MethodPost, "/v1/family/invitations/" + invitationID + "/revoke", familyBody(commandID, ""), 200, `"revision":9`},
		{http.MethodPost, "/v1/family/members/20000000-0000-4000-8000-000000000002/remove", familyBody(commandID, ""), 200, `"outcome":"applied"`},
		{http.MethodPost, "/v1/family/members/20000000-0000-4000-8000-000000000002/role", familyBody(commandID, `"role":"viewer"`), 200, `"outcome":"applied"`},
		{http.MethodPost, "/v1/family/ownership-transfer", familyBody(commandID, `"new_owner_account_id":"20000000-0000-4000-8000-000000000002"`), 200, `"command_id":"` + commandID + `"`},
		{http.MethodPost, "/v1/family/leave", familyBody(commandID, ""), 200, `"code":"ok"`},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			req.Header.Set("Authorization", "Bearer access-token")
			req.RemoteAddr = "127.0.0.1:1234"
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != test.status || recorder.Header().Get("Cache-Control") != "no-store" || !strings.Contains(recorder.Body.String(), test.contains) {
				t.Fatalf("status=%d cache=%q body=%s", recorder.Code, recorder.Header().Get("Cache-Control"), recorder.Body.String())
			}
		})
	}
	if len(calls) != len(tests) {
		t.Fatalf("service calls=%v", calls)
	}
}

func TestFamilyRoutesRejectMissingBearerUnknownFieldsAndUnsafeErrors(t *testing.T) {
	secret := "private-family-name"
	service := familyServiceStub{current: func(context.Context, access.Principal) (family.FamilyView, error) {
		return family.FamilyView{}, errors.New(secret)
	}}
	options := familyHandlerOptions(t, access.Principal{}, service, nil)
	handler := New(options)

	for _, test := range []struct {
		name, bearer, body string
		status             int
		code               string
	}{
		{"missing bearer", "", familyBody("10000000-0000-4000-8000-000000000001", `"display_name":"Family"`), 401, "access_expired"},
		{"unknown field", "Bearer access", familyBody("10000000-0000-4000-8000-000000000001", `"display_name":"Family","secret":true`), 400, "invalid_request"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/family", strings.NewReader(test.body))
			if test.bearer != "" {
				req.Header.Set("Authorization", test.bearer)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.status || !strings.Contains(rec.Body.String(), `"code":"`+test.code+`"`) || strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/family", nil)
	req.Header.Set("Authorization", "Bearer access")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("private service error leaked: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestFamilyErrorMatrixIsPrivacySafe(t *testing.T) {
	for _, test := range []struct {
		code   string
		status int
	}{
		{"invalid_request", 400}, {"unsupported_capability", 409}, {"confirmation_required", 400}, {"invalid_display_name", 400},
		{"access_expired", 401}, {"session_revoked", 401}, {"epoch_changed", 401},
		{"owner_required", 403}, {"membership_revoked", 403}, {"entitlement_required", 403},
		{"family_unavailable", 404}, {"invitation_invalid", 404},
		{"family_slot_occupied", 409}, {"caregiver_limit", 409}, {"invitation_limit", 409}, {"command_id_reused", 409}, {"profile_not_required", 409},
		{"invitation_expired", 410}, {"invitation_consumed", 410}, {"invitation_revoked", 410},
		{"service_unavailable", 503}, {"security_busy", 503}, {"authorization_quarantine", 503},
		{"hosted_adapter_unavailable", 503}, {"credential_key_unavailable", 503},
	} {
		t.Run(test.code, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeFamilyCode(rec, httptest.NewRequest(http.MethodGet, "/", nil), test.code)
			var response struct {
				Error struct {
					Code      string `json:"code"`
					Retryable bool   `json:"retryable"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != test.status || response.Error.Code != test.code || response.Error.Retryable != (test.status == 503) {
				t.Fatalf("status=%d response=%+v err=%v body=%s", rec.Code, response, err, rec.Body.String())
			}
		})
	}
}

func TestInvitationRetryReturnsMetadataWithoutManufacturingSecret(t *testing.T) {
	view := family.InvitationView{ID: "40000000-0000-4000-8000-000000000001", FamilyID: "30000000-0000-4000-8000-000000000001", Status: "active", ExpiresAt: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)}
	response := invitationResponse(view, "", "", true)
	encoded, err := json.Marshal(response)
	if err != nil || !response.SecretUnavailable || strings.Contains(string(encoded), `"token"`) || strings.Contains(string(encoded), `"link"`) || !strings.Contains(string(encoded), `"secret_unavailable":true`) {
		t.Fatalf("response=%s value=%+v err=%v", encoded, response, err)
	}
}

func TestInviteResponseCarriesRole(t *testing.T) {
	familyID := "30000000-0000-4000-8000-000000000001"
	invitationID := "40000000-0000-4000-8000-000000000001"
	expires := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	var gotRole string
	service := familyServiceStub{invite: func(_ context.Context, _ access.Principal, _ string, role string) (family.InvitationSecret, error) {
		gotRole = role
		return family.InvitationSecret{Invitation: family.InvitationView{ID: invitationID, FamilyID: familyID, Role: role, Status: "active", ExpiresAt: expires, CreatedAt: expires.Add(-time.Hour)},
			Token: strings.Repeat("A", 43), Link: "https://sync.example.test/invite#token=" + strings.Repeat("A", 43)}, nil
	}}
	options := familyHandlerOptions(t, access.Principal{AccountID: "20000000-0000-4000-8000-000000000001"}, service, nil)
	handler := New(options)
	req := httptest.NewRequest(http.MethodPost, "/v1/family/invitations", strings.NewReader(familyBody("10000000-0000-4000-8000-000000000009", `"role":"viewer"`)))
	req.Header.Set("Authorization", "Bearer access-token")
	req.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusCreated || gotRole != "viewer" || !strings.Contains(recorder.Body.String(), `"role":"viewer"`) {
		t.Fatalf("status=%d role=%q body=%s", recorder.Code, gotRole, recorder.Body.String())
	}
}

func TestInvitationCreateChargesIPAccountAndStructuralLimitsBeforeService(t *testing.T) {
	principal := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001"}
	called := false
	service := familyServiceStub{invite: func(context.Context, access.Principal, string, string) (family.InvitationSecret, error) {
		called = true
		return family.InvitationSecret{}, errors.New("must not be called")
	}}
	options := familyHandlerOptions(t, principal, service, nil)
	var order []string
	options.Limiter = limiterStub{
		take: func(_ context.Context, scope, key string, policy limits.Policy) (limits.Decision, error) {
			order = append(order, scope+":"+key)
			if scope != limits.ScopeInvitationCreateIP || key != "127.0.0.1" || policy.Count != 60 || policy.Window != time.Hour {
				t.Fatalf("IP charge scope=%q key=%q policy=%+v", scope, key, policy)
			}
			return limits.Decision{Allowed: true}, nil
		},
		takeMany: func(_ context.Context, charges []limits.Charge) (limits.Decision, error) {
			order = append(order, "account-and-structural")
			want := []struct{ scope, key string }{
				{limits.ScopeFamilyStructuralAccount, principal.AccountID},
				{limits.ScopeFamilyStructuralDevice, principal.DeviceID},
				{limits.ScopeInvitationCreateAccount, principal.AccountID},
			}
			if len(charges) != len(want) {
				t.Fatalf("charges=%+v", charges)
			}
			for index := range want {
				if charges[index].Scope != want[index].scope || charges[index].Key != want[index].key || charges[index].Policy.Count == 0 {
					t.Fatalf("charge[%d]=%+v want=%+v", index, charges[index], want[index])
				}
			}
			return limits.Decision{Allowed: false, RetryAfter: 61 * time.Second}, nil
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/family/invitations", strings.NewReader(`{"not":"decoded"}`))
	req.Header.Set("Authorization", "Bearer access")
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	New(options).ServeHTTP(rec, req)
	if called || !reflect.DeepEqual(order, []string{limits.ScopeInvitationCreateIP + ":127.0.0.1", "account-and-structural"}) ||
		rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "61" || !strings.Contains(rec.Body.String(), `"retry_after_seconds":61`) {
		t.Fatalf("called=%t order=%v status=%d retry=%q body=%s", called, order, rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}

func TestInvitationPreviewDoesNotConsumeStructuralCommandLimits(t *testing.T) {
	principal := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001"}
	token := strings.Repeat("A", 43)
	service := familyServiceStub{preview: func(context.Context, access.Principal, string) (family.InvitationPreview, error) {
		return family.InvitationPreview{FamilyDisplayName: "Our family", ExpiresAt: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)}, nil
	}}
	options := familyHandlerOptions(t, principal, service, nil)
	options.Limiter = limiterStub{
		take: func(_ context.Context, scope, _ string, _ limits.Policy) (limits.Decision, error) {
			if scope != limits.ScopeInvitationRedeemIP {
				t.Fatalf("IP scope=%q", scope)
			}
			return limits.Decision{Allowed: true}, nil
		},
		takeMany: func(_ context.Context, charges []limits.Charge) (limits.Decision, error) {
			if len(charges) != 1 || charges[0].Scope != limits.ScopeInvitationRedeemAccount || charges[0].Key != principal.AccountID {
				t.Fatalf("preview charges=%+v", charges)
			}
			return limits.Decision{Allowed: true}, nil
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/family/invitations/preview", strings.NewReader(familyMeta(`"token":"`+token+`"`)))
	req.Header.Set("Authorization", "Bearer access")
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	New(options).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func familyHandlerOptions(t *testing.T, principal access.Principal, service FamilyService, takeMany func([]limits.Charge)) Options {
	t.Helper()
	resolver, err := limits.NewClientIPResolver([]netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	if err != nil {
		t.Fatal(err)
	}
	policies := limits.DefaultPolicies()
	return Options{
		Gate:           accountGateFunc(func(context.Context) error { return nil }),
		SessionService: sessionServiceStub{authenticate: func(context.Context, string) (access.Principal, error) { return principal, nil }},
		FamilyService:  service, ClientIPResolver: resolver, RequestAuthorizer: requestAuthorizerStub{principal: principal},
		Limiter: limiterStub{take: func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
			return limits.Decision{Allowed: true}, nil
		}, takeMany: func(_ context.Context, charges []limits.Charge) (limits.Decision, error) {
			if takeMany != nil {
				takeMany(charges)
			}
			return limits.Decision{Allowed: true}, nil
		}},
		FamilyStructuralAccountPolicy: policies[limits.ScopeFamilyStructuralAccount], FamilyStructuralDevicePolicy: policies[limits.ScopeFamilyStructuralDevice],
		InvitationCreateAccountPolicy: policies[limits.ScopeInvitationCreateAccount], InvitationCreateIPPolicy: policies[limits.ScopeInvitationCreateIP],
		InvitationRedeemAccountPolicy: policies[limits.ScopeInvitationRedeemAccount], InvitationRedeemIPPolicy: policies[limits.ScopeInvitationRedeemIP],
		AppleAuthorizationMode: "identity_token_only",
	}
}

func familyMeta(fields string) string {
	if fields != "" {
		fields = "," + fields
	}
	return `{"protocol_version":"v1","capabilities":["family-lifecycle-v1"]` + fields + `}`
}

func familyBody(commandID, fields string) string {
	if fields != "" {
		fields = "," + fields
	}
	return `{"protocol_version":"v1","capabilities":["family-lifecycle-v1"],"command_id":"` + commandID + `"` + fields + `}`
}

func containsRoute(routes []Route, want Route) bool {
	return slicesContains(routes, want)
}

func slicesContains[T comparable](values []T, want T) bool {
	for _, value := range values {
		if reflect.DeepEqual(value, want) {
			return true
		}
	}
	return false
}
