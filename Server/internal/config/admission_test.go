package config

import "testing"

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
	if err != nil || cfg.Admission != (AdmissionConfig{Total: 8, MCPMax: 2}) {
		t.Fatalf("cfg=%+v err=%v", cfg.Admission, err)
	}
	values["BT_ADMISSION_MCP_MAX"] = "8"
	if _, err := Load(mapGetter(values)); err == nil {
		t.Fatal("global loader ignored invalid admission budget")
	}
}
