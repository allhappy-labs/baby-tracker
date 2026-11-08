package loadtest

import (
	"math"
	"strings"
	"testing"
)

func TestModel10kAndScale(t *testing.T) {
	m := Model10k()
	if m.RefreshPerSecond != 70 || m.PullPerSecond != 600 || m.WritePerSecond != 10 || m.LiveSockets != 4000 {
		t.Fatalf("Model10k drifted from the spec: %+v", m)
	}
	if m.SnapshotPerSecond != 0.2 || m.MCPPerSecond != 1 || m.PushTokenPerSecond != 1 || m.LiveActivityPerSecond != 0.5 {
		t.Fatalf("Model10k's minor classes drifted from the plan's table: %+v", m)
	}
	x := m.Scale(2)
	if x.PullPerSecond != 1200 || x.LiveSockets != 8000 || x.WritePerSecond != 20 {
		t.Fatalf("Scale(2): %+v", x)
	}
	if x.RefreshPerSecond != 140 || x.SnapshotPerSecond != 0.4 || x.MCPPerSecond != 2 || x.PushTokenPerSecond != 2 || x.LiveActivityPerSecond != 1 {
		t.Fatalf("Scale(2) missed a class: %+v", x)
	}
	if err := x.Validate(40000, 200); err != nil {
		t.Fatalf("2x on 40,000 devices: %v", err)
	}
	// The plan's assertion. At 12x it is the sockets (48,000 on 40,000
	// devices) that fail; pulls are only 10.8/min/device.
	if err := m.Scale(12).Validate(40000, 200); err == nil || !strings.Contains(err.Error(), "sockets") {
		t.Fatalf("12x on 40,000 devices: %v, want the socket rule", err)
	}
	// The pull rule on its own: 120x pulls (108/min/device) with sockets and
	// every other class at 1x.
	pulls := m
	pulls.PullPerSecond = m.PullPerSecond * 120
	if err := pulls.Validate(40000, 200); err == nil || !strings.Contains(err.Error(), "pulls") {
		t.Fatalf("pulls above the device rate limit: %v, want the pull rule", err)
	}
	if err := m.Validate(3000, 200); err == nil {
		t.Fatal("more sockets than devices accepted")
	}
	if got := m.Scale(1.5).LiveSockets; got != 6000 {
		t.Fatalf("Scale(1.5) sockets %d", got)
	}
}

// Each Validate rule on its own, with every other rule satisfied, so a rule
// cannot pass only because another one happened to fail.
func TestModelValidateRules(t *testing.T) {
	base := func() Model { return Model10k().Scale(2) }
	cases := []struct {
		name         string
		m            func() Model
		devices      int
		integrations int
		want         string // "" = valid
	}{
		{"2x is valid", base, 40000, 200, ""},
		{"no devices", base, 0, 200, "devices"},
		{"sockets above devices", func() Model { m := base(); m.LiveSockets = 40001; return m }, 40000, 200, "sockets"},
		{"pulls just under 100/min/device", func() Model { m := base(); m.PullPerSecond = 40000 * 99.9 / 60; return m }, 40000, 200, ""},
		{"pulls above 100/min/device", func() Model { m := base(); m.PullPerSecond = 40000*100/60.0 + 1; return m }, 40000, 200, "pulls"},
		{"refresh just under 25/10min/device", func() Model { m := base(); m.RefreshPerSecond = 40000 * 24.9 / 600; return m }, 40000, 200, ""},
		{"refresh above 25/10min/device", func() Model { m := base(); m.RefreshPerSecond = 40000*25/600.0 + 1; return m }, 40000, 200, "refresh"},
		{"MCP without integrations", base, 40000, 0, "integrations"},
		{"no MCP and no integrations", func() Model { m := base(); m.MCPPerSecond = 0; return m }, 40000, 0, ""},
		{"MCP just under 25/min/integration", func() Model { m := base(); m.MCPPerSecond = 200 * 24.9 / 60; return m }, 40000, 200, ""},
		{"MCP above 25/min/integration", func() Model { m := base(); m.MCPPerSecond = 200*25/60.0 + 1; return m }, 40000, 200, "MCP"},
		{"zero load", func() Model { return Model10k().Scale(0) }, 40000, 200, "load"},
		{"negative rate", func() Model { m := base(); m.WritePerSecond = -1; return m }, 40000, 200, "write"},
		{"NaN rate", func() Model { m := base(); m.SnapshotPerSecond = math.NaN(); return m }, 40000, 200, "snapshot"},
		{"infinite rate", func() Model { m := base(); m.PushTokenPerSecond = math.Inf(1); return m }, 40000, 200, "push"},
		{"negative sockets", func() Model { m := base(); m.LiveSockets = -1; return m }, 40000, 200, "sockets"},
	}
	for _, c := range cases {
		err := c.m().Validate(c.devices, c.integrations)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && err == nil:
			t.Errorf("%s: accepted", c.name)
		case c.want != "" && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.want)):
			t.Errorf("%s: %q does not name %q", c.name, err, c.want)
		}
	}
}

func TestWriteMixSumsToOne(t *testing.T) {
	for _, pump := range []bool{false, true} {
		sum := 0.0
		for _, k := range DefaultWriteMix(pump).Kinds {
			sum += k.Share
		}
		if math.Abs(sum-1) > 1e-9 {
			t.Fatalf("pump=%v kinds sum %v", pump, sum)
		}
	}
}

func TestWriteMixMatchesThePlan(t *testing.T) {
	shares := func(w WriteMix) map[string]float64 {
		out := map[string]float64{}
		for _, k := range w.Kinds {
			if _, dup := out[k.Kind]; dup {
				t.Fatalf("kind %s listed twice", k.Kind)
			}
			out[k.Kind] = k.Share
		}
		return out
	}
	with := shares(DefaultWriteMix(true))
	want := map[string]float64{"feed": 0.33, "diaper": 0.30, "sleep": 0.20, "timer": 0.07, "supplement": 0.03, "measurement": 0.02, "immunization": 0.02, "pump": 0.02, "growth_profile": 0.01}
	if len(with) != len(want) {
		t.Fatalf("with pump: %v", with)
	}
	for k, v := range want {
		if math.Abs(with[k]-v) > 1e-12 {
			t.Errorf("with pump %s = %v, want %v", k, with[k], v)
		}
	}
	without := shares(DefaultWriteMix(false))
	if _, ok := without["pump"]; ok || math.Abs(without["feed"]-0.35) > 1e-12 || len(without) != len(want)-1 {
		t.Fatalf("without pump: %v", without)
	}
	w := DefaultWriteMix(true)
	if math.Abs(w.Create+w.Update+w.Delete-1) > 1e-9 || w.Create != 0.85 || w.Update != 0.12 || w.Delete != 0.03 || w.MultiMutation != 0.15 {
		t.Fatalf("operations: %+v", w)
	}
}

func TestCursorMixMatchesThePlan(t *testing.T) {
	c := DefaultCursorMix()
	if c != (CursorMix{AtHead: 0.80, SmallLag: 0.15, LargeLag: 0.04, FarBehind: 0.01}) {
		t.Fatalf("cursor mix %+v", c)
	}
	if math.Abs(c.AtHead+c.SmallLag+c.LargeLag+c.FarBehind-1) > 1e-9 {
		t.Fatalf("cursor mix sums to %v", c.AtHead+c.SmallLag+c.LargeLag+c.FarBehind)
	}
}
