package loadtest

import (
	"errors"
	"fmt"
	"math"
)

// Model is the offered load: open-loop rates per request class and the live
// sockets held. Model10k is the spec's 1x at 10,000 families; the harness
// drives Scale(2).
type Model struct {
	RefreshPerSecond      float64 `json:"refresh_per_second"`
	PullPerSecond         float64 `json:"pull_per_second"`
	WritePerSecond        float64 `json:"write_per_second"`
	SnapshotPerSecond     float64 `json:"snapshot_per_second"`
	MCPPerSecond          float64 `json:"mcp_per_second"`
	PushTokenPerSecond    float64 `json:"push_token_per_second"`
	LiveActivityPerSecond float64 `json:"live_activity_per_second"`
	LiveSockets           int     `json:"live_sockets"`
}

// Model10k is the plan's load-model table at 1x.
func Model10k() Model {
	return Model{
		RefreshPerSecond:      70,
		PullPerSecond:         600,
		WritePerSecond:        10,
		SnapshotPerSecond:     0.2,
		MCPPerSecond:          1,
		PushTokenPerSecond:    1,
		LiveActivityPerSecond: 0.5,
		LiveSockets:           4000,
	}
}

// Scale multiplies every rate by load and rounds the socket count.
func (m Model) Scale(load float64) Model {
	return Model{
		RefreshPerSecond:      m.RefreshPerSecond * load,
		PullPerSecond:         m.PullPerSecond * load,
		WritePerSecond:        m.WritePerSecond * load,
		SnapshotPerSecond:     m.SnapshotPerSecond * load,
		MCPPerSecond:          m.MCPPerSecond * load,
		PushTokenPerSecond:    m.PushTokenPerSecond * load,
		LiveActivityPerSecond: m.LiveActivityPerSecond * load,
		LiveSockets:           int(math.Round(float64(m.LiveSockets) * load)),
	}
}

// Per-device and per-integration ceilings the model must stay under, so a
// run measures capacity and never the server's own rate limits
// (internal/limits: sync pull 120/min/device, auth-refresh-device 30/10 min,
// MCP 30/min/integration).
const (
	maxPullsPerDevicePerMinute         = 100
	maxRefreshesPerDevicePer10Minutes  = 25
	maxMCPCallsPerIntegrationPerMinute = 25
)

// Validate checks that the model is runnable against a fleet of devices and
// MCP integrations: positive load, rates finite and non-negative, sockets no
// more than devices, and every per-device or per-integration rate under the
// server's limit with margin.
func (m Model) Validate(devices, integrations int) error {
	if devices <= 0 {
		return fmt.Errorf("model: %d devices, want at least one", devices)
	}
	rates := []struct {
		name string
		v    float64
	}{
		{"refresh", m.RefreshPerSecond}, {"pull", m.PullPerSecond}, {"write", m.WritePerSecond},
		{"snapshot", m.SnapshotPerSecond}, {"MCP", m.MCPPerSecond}, {"push-token", m.PushTokenPerSecond},
		{"live-activity", m.LiveActivityPerSecond},
	}
	positive := m.LiveSockets > 0
	for _, r := range rates {
		if math.IsNaN(r.v) || math.IsInf(r.v, 0) || r.v < 0 {
			return fmt.Errorf("model: %s rate %v is not a finite non-negative number", r.name, r.v)
		}
		positive = positive || r.v > 0
	}
	if !positive {
		return errors.New("model: zero load")
	}
	if m.LiveSockets < 0 {
		return fmt.Errorf("model: %d live sockets", m.LiveSockets)
	}
	if m.LiveSockets > devices {
		return fmt.Errorf("model: %d live sockets need more than %d devices", m.LiveSockets, devices)
	}
	if per := m.PullPerSecond * 60 / float64(devices); per > maxPullsPerDevicePerMinute {
		return fmt.Errorf("model: %.1f pulls per device per minute exceeds %d (the device limit is 120)", per, maxPullsPerDevicePerMinute)
	}
	if per := m.RefreshPerSecond * 600 / float64(devices); per > maxRefreshesPerDevicePer10Minutes {
		return fmt.Errorf("model: %.1f refreshes per device per 10 minutes exceeds %d (auth-refresh-device is 30)", per, maxRefreshesPerDevicePer10Minutes)
	}
	if m.MCPPerSecond > 0 {
		if integrations <= 0 {
			return fmt.Errorf("model: MCP calls with %d integrations", integrations)
		}
		if per := m.MCPPerSecond * 60 / float64(integrations); per > maxMCPCallsPerIntegrationPerMinute {
			return fmt.Errorf("model: %.1f MCP calls per integration per minute exceeds %d (the quota is 30)", per, maxMCPCallsPerIntegrationPerMinute)
		}
	}
	return nil
}

// CursorMix is the share of pulls by how far the device's cursor is rewound:
// at its own head, 1–50 changes, 51–1,000 changes, or to 0.
type CursorMix struct {
	AtHead    float64 `json:"at_head"`
	SmallLag  float64 `json:"small_lag"`
	LargeLag  float64 `json:"large_lag"`
	FarBehind float64 `json:"far_behind"`
}

func DefaultCursorMix() CursorMix {
	return CursorMix{AtHead: 0.80, SmallLag: 0.15, LargeLag: 0.04, FarBehind: 0.01}
}

// KindShare is one care entity kind (the protocol's entity_kind) and its
// share of write mutations.
type KindShare struct {
	Kind  string  `json:"kind"`
	Share float64 `json:"share"`
}

// WriteMix is the write requests' shape: kind shares, operation shares, and
// the share of requests carrying 2–3 mutations.
type WriteMix struct {
	Kinds         []KindShare `json:"kinds"`
	Create        float64     `json:"create"`
	Update        float64     `json:"update"`
	Delete        float64     `json:"delete"`
	MultiMutation float64     `json:"multi_mutation"`
}

// DefaultWriteMix is the plan's write table. Without pumping (kind 10) its
// 2% goes to feeds.
func DefaultWriteMix(withPump bool) WriteMix {
	feed := 0.35
	if withPump {
		feed = 0.33
	}
	kinds := []KindShare{
		{"feed", feed}, {"diaper", 0.30}, {"sleep", 0.20}, {"timer", 0.07}, {"supplement", 0.03},
		{"measurement", 0.02}, {"immunization", 0.02},
	}
	if withPump {
		kinds = append(kinds, KindShare{"pump", 0.02})
	}
	kinds = append(kinds, KindShare{"growth_profile", 0.01})
	return WriteMix{Kinds: kinds, Create: 0.85, Update: 0.12, Delete: 0.03, MultiMutation: 0.15}
}
