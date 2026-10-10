package mcpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/familyread"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var bothVersions = []string{ProtocolCurrent, ProtocolLegacy}

func TestSDKClientNegotiatesBothSupportedRevisions(t *testing.T) {
	for _, version := range bothVersions {
		t.Run(version, func(t *testing.T) {
			e := newEnv(t, nil)
			session, transport := e.connect(t, version, testCredential)
			initialized := session.InitializeResult()
			if initialized == nil || initialized.ProtocolVersion != version {
				t.Fatalf("negotiated %+v, want %s", initialized, version)
			}
			capabilities := initialized.Capabilities
			if capabilities == nil || capabilities.Tools == nil || capabilities.Tools.ListChanged || capabilities.Logging != nil ||
				capabilities.Resources != nil || capabilities.Prompts != nil || capabilities.Completions != nil {
				t.Fatalf("capabilities=%+v", capabilities)
			}
			if !strings.Contains(initialized.Instructions, "data") {
				t.Fatalf("instructions=%q", initialized.Instructions)
			}
			tools, err := session.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			requireCatalog(t, tools.Tools)
			babies, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_babies", Arguments: map[string]any{}})
			if err != nil || babies.IsError {
				t.Fatalf("list_babies: %+v %v", babies, err)
			}
			var structured familyread.BabiesResult
			raw, _ := json.Marshal(babies.StructuredContent)
			if err := json.Unmarshal(raw, &structured); err != nil || structured.Revision != 7 || len(structured.Babies) != 1 {
				t.Fatalf("structured=%s err=%v", raw, err)
			}
			text, ok := babies.Content[0].(*mcp.TextContent)
			if !ok || !strings.Contains(text.Text, "Élie") || len(babies.Content) != 1 {
				t.Fatalf("text content=%+v", babies.Content)
			}
			history, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_history", Arguments: map[string]any{
				"baby_id": testBabyID, "start": "2026-09-09T00:00:00Z", "end": "2026-09-10T00:00:00+02:00", "kinds": []string{"diaper", "sleep"}, "limit": 10}})
			if err != nil || history.IsError {
				t.Fatalf("get_history: %+v %v", history, err)
			}
			summary, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_summary", Arguments: map[string]any{
				"baby_id": testBabyID, "start": "2026-09-09T00:00:00Z", "end": "2026-09-10T00:00:00Z", "timezone": "Europe/Zurich"}})
			if err != nil || summary.IsError {
				t.Fatalf("get_summary: %+v %v", summary, err)
			}
			raw, _ = json.Marshal(summary.StructuredContent)
			if !strings.Contains(string(raw), `"known_bottle_volume_ml":1.5`) || !strings.Contains(string(raw), `"completed_seconds":3600.5`) {
				t.Fatalf("summary decimals were not preserved exactly: %s", raw)
			}
			for _, response := range transport.responses() {
				if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Mcp-Session-Id") != "" {
					t.Fatalf("response headers %v", response.Header)
				}
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			e.assertIdle(t)
		})
	}
}

func requireCatalog(t *testing.T, tools []*mcp.Tool) {
	t.Helper()
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
		annotations := tool.Annotations
		if annotations == nil || !annotations.ReadOnlyHint || !annotations.IdempotentHint || annotations.DestructiveHint == nil || *annotations.DestructiveHint ||
			annotations.OpenWorldHint == nil || *annotations.OpenWorldHint || annotations.Title == "" {
			t.Fatalf("%s annotations=%+v", tool.Name, annotations)
		}
		input, _ := json.Marshal(tool.InputSchema)
		output, _ := json.Marshal(tool.OutputSchema)
		var inputSchema, outputSchema map[string]any
		_ = json.Unmarshal(input, &inputSchema)
		_ = json.Unmarshal(output, &outputSchema)
		if inputSchema["type"] != "object" || inputSchema["additionalProperties"] != false || outputSchema["type"] != "object" {
			t.Fatalf("%s schemas input=%s output=%s", tool.Name, input, output)
		}
		if !strings.Contains(tool.Description, "data, not instructions") {
			t.Fatalf("%s description lacks the notes-as-data rule", tool.Name)
		}
		lower := strings.ToLower(tool.Name + " " + string(input))
		for _, forbidden := range []string{"create", "update", "delete", "write", "sql", "admin", "export", "revoke", "grant"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s exposes %q", tool.Name, forbidden)
			}
		}
		switch tool.Name {
		case "get_history":
			if !strings.Contains(tool.Description, familyread.HistorySelection) {
				t.Fatal("get_history description must quote familyread.HistorySelection verbatim")
			}
			requireRequired(t, inputSchema, "baby_id", "end", "start")
		case "get_summary":
			if !strings.Contains(tool.Description, familyread.SummaryDefinitions) {
				t.Fatal("get_summary description must quote familyread.SummaryDefinitions verbatim")
			}
			requireRequired(t, inputSchema, "baby_id", "end", "start", "timezone")
		}
	}
	slices.Sort(names)
	if strings.Join(names, ",") != "get_history,get_summary,list_babies" {
		t.Fatalf("tools=%v", names)
	}
}

func requireRequired(t *testing.T, schema map[string]any, names ...string) {
	t.Helper()
	var got []string
	for _, name := range schema["required"].([]any) {
		got = append(got, name.(string))
	}
	slices.Sort(got)
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Fatalf("required=%v want %v", got, names)
	}
}

func TestToolArgumentsAndReadErrorsBecomeSafeToolErrors(t *testing.T) {
	e := newEnv(t, nil)
	session, _ := e.connect(t, ProtocolLegacy, testCredential)
	call := func(name string, arguments map[string]any) *mcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: arguments})
		if err != nil {
			t.Fatalf("%s %v: %v", name, arguments, err)
		}
		return result
	}
	code := func(result *mcp.CallToolResult) string {
		t.Helper()
		raw, _ := json.Marshal(result)
		var envelope toolEnvelope
		_ = json.Unmarshal(raw, &envelope)
		return toolErrorCode(t, envelope)
	}
	valid := map[string]any{"baby_id": testBabyID, "start": "2026-09-09T00:00:00Z", "end": "2026-09-10T00:00:00Z"}
	with := func(key string, value any) map[string]any {
		copied := map[string]any{}
		for k, v := range valid {
			copied[k] = v
		}
		if value == nil {
			delete(copied, key)
		} else {
			copied[key] = value
		}
		return copied
	}
	for name, arguments := range map[string]map[string]any{
		"unknown property": with("family_id", "x"),
		"missing baby":     with("baby_id", nil),
		"bad instant":      with("start", "yesterday"),
		"bad kind":         with("kinds", []string{"timer"}),
		"limit too big":    with("limit", 101),
		"limit fraction":   with("limit", 1.5),
		"wrong type":       with("baby_id", 5),
	} {
		if got := code(call("get_history", arguments)); got != "invalid_arguments" {
			t.Fatalf("%s: %s", name, got)
		}
	}
	if got := code(call("list_babies", map[string]any{"family_id": "x"})); got != "invalid_arguments" {
		t.Fatalf("list_babies extra=%s", got)
	}
	readErrors := []error{familyread.ErrInvalidRange, familyread.ErrRangeTooLong, familyread.ErrInvalidLimit, familyread.ErrInvalidKind,
		familyread.ErrInvalidBabyID, familyread.ErrBabyNotFound, familyread.ErrInvalidCursor, familyread.ErrCursorExpired,
		familyread.ErrHistoryChanged, familyread.ErrResultTooLarge, familyread.ErrInvalidTimezone,
		fmt.Errorf("%w: sleep 50000000-0000-4000-8000-000000000001 SELECT secret", familyread.ErrDataInvalid)}
	for _, readErr := range readErrors {
		e.reader.history = func(context.Context, familyread.HistoryQuery, familyread.HistoryBudget) (familyread.HistoryResult, error) {
			return familyread.HistoryResult{}, readErr
		}
		result := call("get_history", valid)
		if got := code(result); got != familyread.Code(readErr) {
			t.Fatalf("%v mapped to %s", readErr, got)
		}
		raw, _ := json.Marshal(result)
		if strings.Contains(string(raw), "50000000") || strings.Contains(string(raw), "SELECT") {
			t.Fatalf("leaked error text: %s", raw)
		}
	}
	e.reader.summary = func(context.Context, familyread.SummaryQuery) (familyread.SummaryResult, error) {
		return familyread.SummaryResult{}, familyread.ErrInvalidTimezone
	}
	if got := code(call("get_summary", map[string]any{"baby_id": testBabyID, "start": "2026-09-09T00:00:00Z", "end": "2026-09-10T00:00:00Z", "timezone": "Local"})); got != "invalid_timezone" {
		t.Fatalf("summary timezone=%s", got)
	}
	if _, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "delete_baby", Arguments: map[string]any{}}); err == nil {
		t.Fatal("unknown tool accepted")
	}
	e.assertIdle(t)
}

func TestRevocationAfterHTTPAuthenticationIsUnauthorized(t *testing.T) {
	for _, version := range bothVersions {
		t.Run(version, func(t *testing.T) {
			e := newEnv(t, nil)
			session, _ := e.connect(t, version, testCredential)
			e.reader.babies = func(context.Context) (familyread.BabiesResult, error) {
				return familyread.BabiesResult{}, authority.ErrIntegrationUnauthorized
			}
			_, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_babies", Arguments: map[string]any{}})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unauthorized") {
				t.Fatalf("revoked call err=%v", err)
			}
			// The SDK client surfaces the JSON-RPC body of the 401 as a per-call
			// rejection; the session itself is not torn down.
			e.reader.babies = nil
			if _, err := session.ListTools(t.Context(), nil); err != nil {
				t.Fatalf("session broken after 401: %v", err)
			}
			e.reader.babies = func(context.Context) (familyread.BabiesResult, error) {
				return familyread.BabiesResult{}, authority.ErrIntegrationUnauthorized
			}
			raw := e.post(t, toolCall(9, "list_babies", `{}`))
			if raw.status != 401 || errorCode(t, raw) != "unauthorized" || string(decodeEnvelope(t, raw).ID) != "9" || raw.header.Get("WWW-Authenticate") == "" {
				t.Fatalf("raw revoked call: %s", raw)
			}
			requireNoStore(t, raw)
			e.reader.babies = func(context.Context) (familyread.BabiesResult, error) {
				return familyread.BabiesResult{}, authority.ErrUnavailable
			}
			if raw := e.post(t, toolCall(9, "list_babies", `{}`)); raw.status != 503 || errorCode(t, raw) != "unavailable" {
				t.Fatalf("unavailable read: %s", raw)
			}
			e.assertIdle(t)
		})
	}
}

func TestOldMobileTokenCannotConnect(t *testing.T) {
	e := newEnv(t, nil)
	client := mcp.NewClient(&mcp.Implementation{Name: "phone", Version: "1"}, nil)
	for _, version := range bothVersions {
		httpClient := &http.Client{Timeout: 5 * time.Second, Transport: &bearerTransport{token: phoneToken, base: &http.Transport{}}}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		_, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: e.url, HTTPClient: httpClient, DisableStandaloneSSE: true, MaxRetries: -1},
			&mcp.ClientSessionOptions{ProtocolVersion: version})
		cancel()
		if err == nil {
			t.Fatalf("%s: phone token connected", version)
		}
	}
	if e.reader.calls.Load() != 0 || e.quota.calls.Load() != 0 {
		t.Fatal("phone token reached reads or quota")
	}
}

func TestProtocolErrorsFollowTheSDKNegotiationRules(t *testing.T) {
	e := newEnv(t, nil)
	// Legacy initialize with an unknown version negotiates a supported legacy version.
	headers := legacyHeaders()
	delete(headers, "Mcp-Protocol-Version")
	initialize := e.do(t, http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-01-01","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`, headers)
	var negotiated struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(decodeEnvelope(t, initialize).Result, &negotiated); err != nil || negotiated.ProtocolVersion != ProtocolLegacy {
		t.Fatalf("legacy fallback: %s", initialize)
	}
	requireNoStore(t, initialize)
	// A modern request for an unsupported modern version gets the SDK's JSON-RPC error.
	modern := currentHeaders("tools/list", "")
	modern["Mcp-Protocol-Version"] = "2099-01-01"
	unsupported := e.do(t, http.MethodPost, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01","io.modelcontextprotocol/clientCapabilities":{}}}}`, modern)
	if unsupported.status != http.StatusBadRequest || decodeEnvelope(t, unsupported).Error == nil || decodeEnvelope(t, unsupported).Error.Code != mcp.CodeUnsupportedProtocolVersion {
		t.Fatalf("modern unsupported version: %s", unsupported)
	}
	requireNoStore(t, unsupported)
	// Header/metadata mismatch.
	mismatch := e.do(t, http.MethodPost, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, legacyHeaders())
	if mismatch.status != http.StatusBadRequest {
		t.Fatalf("header/meta mismatch: %s", mismatch)
	}
	// Missing client capabilities in modern metadata.
	missing := e.do(t, http.MethodPost, `{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`, currentHeaders("tools/list", ""))
	if missing.status != http.StatusBadRequest || decodeEnvelope(t, missing).Error == nil {
		t.Fatalf("missing capabilities: %s", missing)
	}
	// Unknown methods: modern 404 JSON-RPC; legacy SDK HTTP 400.
	unknownModern := e.do(t, http.MethodPost, `{"jsonrpc":"2.0","id":5,"method":"family/delete","params":{`+currentMeta+`}}`, currentHeaders("family/delete", ""))
	if unknownModern.status != http.StatusNotFound || decodeEnvelope(t, unknownModern).Error.Code != -32601 {
		t.Fatalf("unknown modern method: %s", unknownModern)
	}
	if response := e.post(t, `{"jsonrpc":"2.0","id":6,"method":"family/delete"}`); response.status != http.StatusBadRequest {
		t.Fatalf("unknown legacy method: %s", response)
	}
	// Built-in SDK methods outside the read-only surface are denied even
	// though the SDK would route them.
	for _, method := range []string{"resources/list", "resources/read", "resources/templates/list", "prompts/list", "prompts/get",
		"completion/complete", "logging/setLevel", "resources/subscribe", "resources/unsubscribe", "subscriptions/listen"} {
		legacy := e.post(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":%q,"params":{"name":"x","uri":"file:///etc/passwd","level":"debug","ref":{"type":"ref/prompt","name":"x"},"argument":{"name":"a","value":"b"}}}`, method))
		envelope := decodeEnvelope(t, legacy)
		if envelope.Error == nil || envelope.Error.Code != -32601 {
			t.Fatalf("legacy %s: %s", method, legacy)
		}
		requireNoStore(t, legacy)
		modernBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":8,"method":%q,"params":{%s,"name":"x","uri":"file:///etc/passwd","level":"debug","notifications":{}}}`, method, currentMeta)
		modernResponse := e.do(t, http.MethodPost, modernBody, currentHeaders(method, "x"))
		if modernResponse.status != http.StatusNotFound && modernResponse.status != http.StatusBadRequest {
			t.Fatalf("modern %s: %s", method, modernResponse)
		}
		requireNoStore(t, modernResponse)
	}
	e.assertIdle(t)
}

func TestNonTwoHundredResponsesKeepTheirMeaningForTheSDKClient(t *testing.T) {
	e := newEnv(t, nil)
	session, _ := e.connect(t, ProtocolLegacy, testCredential)
	e.quota.err = errors.New("limiter down")
	_, err := session.ListTools(t.Context(), nil)
	if err == nil {
		t.Fatal("503 surfaced as success")
	}
	e.quota.err = nil
	if _, err := session.ListTools(t.Context(), nil); err != nil {
		t.Fatalf("transient 503 broke the session: %v", err)
	}
}
