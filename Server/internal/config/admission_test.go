package config

import (
	"testing"
	"time"

	"babytracker/server/internal/admission"
)

// BT_ADMISSION_WAIT bounds the wait for an admission slot: unset is 250ms,
// 0 is no queue, a Go duration up to 1s otherwise.
func TestAdmissionWaitBounds(t *testing.T) {
	for _, tt := range []struct {
		raw   string
		want  time.Duration
		valid bool
	}{
		{"", 250 * time.Millisecond, true}, {"0", 0, true}, {"0s", 0, true}, {"250ms", 250 * time.Millisecond, true}, {"1s", time.Second, true},
		{"1001ms", 0, false}, {"-1ms", 0, false}, {"250", 0, false}, {"soon", 0, false},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			cfg, err := LoadAdmission(func(key string) string {
				if key == "BT_ADMISSION_WAIT" {
					return tt.raw
				}
				return ""
			})
			if (err == nil) != tt.valid {
				t.Fatalf("cfg=%+v err=%v", cfg, err)
			}
			if tt.valid && cfg != (AdmissionConfig{Total: 32, AuthMax: 8, Wait: tt.want}) {
				t.Fatalf("cfg=%+v", cfg)
			}
		})
	}
}

func TestAdmissionConfigBounds(t *testing.T) {
	for _, tt := range []struct {
		total, mcp         string
		wantTotal, wantMCP int
		valid              bool
	}{
		{"", "", 32, 0, true}, {"32", "4", 32, 4, true}, {"1", "0", 1, 0, true}, {"1024", "1023", 1024, 1023, true},
		{"0", "0", 0, 0, false}, {"1025", "0", 0, 0, false}, {"32", "32", 0, 0, false}, {"32", "-1", 0, 0, false}, {"032", "0", 0, 0, false}, {"32", "04", 0, 0, false}, {"garbage", "0", 0, 0, false},
	} {
		t.Run(tt.total+"/"+tt.mcp, func(t *testing.T) {
			cfg, err := LoadAdmission(func(key string) string {
				if key == "BT_ADMISSION_TOTAL" {
					return tt.total
				}
				if key == "BT_ADMISSION_MCP_MAX" {
					return tt.mcp
				}
				return ""
			})
			if (err == nil) != tt.valid {
				t.Fatalf("cfg=%+v err=%v", cfg, err)
			}
			if tt.valid && (cfg.Total != tt.wantTotal || cfg.MCPMax != tt.wantMCP) {
				t.Fatalf("cfg=%+v", cfg)
			}
		})
	}
	if _, err := LoadAdmission(nil); err == nil {
		t.Fatal("nil getter accepted")
	}
}

func TestAdmissionConfigLoadedByServer(t *testing.T) {
	values := enabledMCPEnvironment(t)
	values["BT_ADMISSION_TOTAL"] = "8"
	values["BT_ADMISSION_MCP_MAX"] = "2"
	values["BT_DB_MCP_MAX_CONNS"] = "2"
	cfg, err := Load(mapGetter(values))
	if err != nil || cfg.Admission != (AdmissionConfig{Total: 8, MCPMax: 2, AuthMax: 8, Wait: DefaultAdmissionWait}) {
		t.Fatalf("cfg=%+v err=%v", cfg.Admission, err)
	}
	values["BT_ADMISSION_AUTH_MAX"] = "3"
	if cfg, err := Load(mapGetter(values)); err != nil || cfg.Admission.AuthMax != 3 {
		t.Fatalf("cfg=%+v err=%v", cfg.Admission, err)
	}
	values["BT_ADMISSION_AUTH_MAX"] = "0"
	if _, err := Load(mapGetter(values)); err == nil {
		t.Fatal("global loader ignored an invalid auth lane")
	}
	values["BT_ADMISSION_AUTH_MAX"] = ""
	values["BT_ADMISSION_MCP_MAX"] = "8"
	if _, err := Load(mapGetter(values)); err == nil {
		t.Fatal("global loader ignored invalid admission budget")
	}
}

// BT_ADMISSION_AUTH_MAX bounds the auth lane, alongside BT_ADMISSION_TOTAL
// and independent of it: a canonical integer from 1 through 1024, default 8.
func TestAdmissionAuthLaneBounds(t *testing.T) {
	for _, tt := range []struct {
		total, auth string
		want        int
		valid       bool
	}{
		{"", "", 8, true}, {"", "1", 1, true}, {"", "1024", 1024, true}, {"1", "64", 64, true}, {"1024", "8", 8, true},
		{"", "0", 0, false}, {"", "-1", 0, false}, {"", "1025", 0, false}, {"", "08", 0, false}, {"", "eight", 0, false}, {"", " 8", 0, false},
	} {
		t.Run(tt.total+"/"+tt.auth, func(t *testing.T) {
			cfg, err := LoadAdmission(func(key string) string {
				switch key {
				case "BT_ADMISSION_TOTAL":
					return tt.total
				case "BT_ADMISSION_AUTH_MAX":
					return tt.auth
				}
				return ""
			})
			if (err == nil) != tt.valid {
				t.Fatalf("cfg=%+v err=%v", cfg, err)
			}
			if tt.valid && cfg.AuthMax != tt.want {
				t.Fatalf("cfg=%+v", cfg)
			}
		})
	}
	if DefaultAdmission().AuthMax != admission.DefaultAuth {
		t.Fatalf("default auth lane %d", DefaultAdmission().AuthMax)
	}
}
