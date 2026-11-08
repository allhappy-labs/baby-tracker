package loadtest

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"babytracker/server/internal/protocol"
)

// sendWithRaces sends req, retrying a lost race per Retry-After (each
// attempt recorded under route), and returns the last result. A request
// with a device's session renews it first when it must (renewBeforeUse);
// one whose renewal fails is not sent (outcome access_not_renewed, not
// recorded under route).
func (g *generator) sendWithRaces(ctx context.Context, route string, intended time.Time, req request, out any) result {
	if d := req.session; d != nil {
		renewed, ok := g.renewBeforeUse(ctx, d, intended)
		if !ok {
			g.skip(skipAccessNotRenewed, intended)
			return result{Outcome: skipAccessNotRenewed}
		}
		intended = intended.Add(renewed)
	}
	for races := 0; ; races++ {
		res := g.c.do(ctx, req, out)
		retrying := res.lostRace() && races < refreshRetries && ctx.Err() == nil
		if req.session != nil {
			g.noteSessionWait(route, res.SessionWait)
		}
		g.record(route, intended.Add(res.SessionWait), settled(res, retrying))
		if !retrying {
			if res.Outcome == OutcomeAccessExpired && req.device != nil {
				g.expired(ctx, req.device)
			}
			return res
		}
		wait := retryDelay(res)
		intended = time.Now().Add(wait)
		if !sleepFor(ctx, wait) {
			return res
		}
	}
}

// opPushToken is one arrival of the push-token class: a device registers
// its APNs token again (PUT /v1/devices/push-token). A device the seed gave
// none registers a new throwaway token, kept in the fleet.
func (g *generator) opPushToken(ctx context.Context, intended time.Time) {
	d := g.randomDevice(g.devices)
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.PushToken == "" {
		d.PushToken = randomHex(32)
	}
	token := d.PushToken
	d.mu.Unlock()
	body := protocol.PushTokenRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: capabilitiesPushToken},
		Token: token, Environment: "sandbox"}
	g.sendWithRaces(ctx, RoutePushToken, intended, request{method: http.MethodPut, path: "/v1/devices/push-token", device: d,
		session: d, capabilities: capabilitiesPushToken, body: body}, nil)
}

// opLiveActivity is one arrival of the Live Activity class: a device
// registers its Live Activity installation again (PUT
// /v1/live-activities/device) at a higher registration version. The seed
// registered LiveActivityInstallationID(device) at version 1; versions here
// are microseconds since the epoch, so every run's are higher than the
// last's.
func (g *generator) opLiveActivity(ctx context.Context, intended time.Time) {
	d := g.randomDevice(g.devices)
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.LiveActivityToken == "" {
		d.LiveActivityToken = randomHex(32)
	}
	version := max(uint64(time.Now().UnixMicro()), d.laVersion+1)
	d.laVersion = version
	token := d.LiveActivityToken
	d.mu.Unlock()
	body := protocol.LiveActivityDeviceRegistration{
		RequestMeta:    protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: capabilitiesLiveActivity},
		InstallationID: LiveActivityInstallationID(d.DeviceID), RegistrationVersion: version, APNsEnvironment: "sandbox",
		PayloadVersion: protocol.LiveActivityPayloadVersion, PushToStartToken: token, FeedBreakMinutes: 30}
	g.sendWithRaces(ctx, RouteLiveActivity, intended, request{method: http.MethodPut, path: "/v1/live-activities/device", device: d,
		session: d, capabilities: capabilitiesLiveActivity, body: body}, nil)
}

// opMCP is one arrival of the MCP class: a device holding its family's
// integration credential makes one stateless tool call (the read tools in
// turn, as internal/capacity/mcp.go:174-220 does). MCP's own refusals
// classify like the sync routes': 503 server_busy is server_busy, 503
// timeout is server_timeout, 429 is rate_limited.
func (g *generator) opMCP(ctx context.Context, intended time.Time) {
	d := g.randomDevice(g.mcp)
	if d == nil {
		return
	}
	call := g.rng.IntN(1 << 20)
	baby := d.BabyIDs[g.rng.IntN(len(d.BabyIDs))]
	res := g.c.do(ctx, request{method: http.MethodPost, path: "/mcp", device: d, bearer: d.MCPCredential,
		body:   mcpToolCall(call, baby, time.Now()),
		header: map[string]string{"Accept": "application/json, text/event-stream", "Mcp-Protocol-Version": mcpProtocol}}, &mcpEnvelope{})
	g.record(RouteMCP, intended, res.Outcome)
}

// mcpEnvelope decodes an MCP JSON-RPC answer; a tool error or an error
// object in a 200 is a protocol failure.
type mcpEnvelope struct{}

func (*mcpEnvelope) UnmarshalJSON(raw []byte) error {
	var envelope struct {
		Result *struct {
			IsError bool `json:"isError"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if envelope.Result == nil || envelope.Result.IsError || len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return errMCPToolError
	}
	return nil
}

type mcpError string

func (e mcpError) Error() string { return string(e) }

const errMCPToolError = mcpError("mcp tool error")

// mcpToolCall is one tools/call request: list_babies, get_history (the last
// day) or get_summary (the last week) for the device's baby.
func mcpToolCall(call int, babyID string, now time.Time) []byte {
	var name string
	var arguments any
	switch call % 3 {
	case 0:
		name, arguments = "list_babies", map[string]any{}
	case 1:
		name, arguments = "get_history", map[string]any{"baby_id": babyID, "start": now.Add(-24 * time.Hour).UTC().Format(time.RFC3339),
			"end": now.UTC().Format(time.RFC3339), "limit": 50}
	default:
		name, arguments = "get_summary", map[string]any{"baby_id": babyID, "start": now.Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"end": now.UTC().Format(time.RFC3339), "timezone": "UTC"}
	}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call + 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": arguments}})
	return raw
}
