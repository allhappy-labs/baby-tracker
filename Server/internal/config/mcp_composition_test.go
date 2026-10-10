package config

import (
	"strings"
	"testing"
)

func enabledMCPEnvironment(t *testing.T) map[string]string {
	t.Helper()
	values := validSelfHostedEnvironment(t)
	for key, value := range laboratoryPools() {
		values[key] = value
	}
	dir := t.TempDir()
	values["BT_MCP_ENABLED"] = "true"
	values["BT_MCP_DATABASE_URL_FILE"] = writeMCPSecret(t, dir, "mcp-url", "postgres://runtime:secret@db/baby\n")
	values["BT_MCP_CURSOR_KEY_FILE"] = writeMCPSecret(t, dir, "mcp-key", strings.Repeat("5a", 32)+"\n")
	values["BT_DB_MCP_MAX_CONNS"] = "4"
	values["BT_ADMISSION_MCP_MAX"] = "4"
	return values
}

func TestLoadComposesEnabledMCPWithExplicitBudgets(t *testing.T) {
	values := enabledMCPEnvironment(t)
	cfg, err := Load(mapGetter(values))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MCP.Enabled || cfg.MCP.DatabaseURL != "postgres://runtime:secret@db/baby" || cfg.DatabasePools.MCP != 4 || cfg.Admission.MCPMax != 4 {
		t.Fatalf("mcp=%v pools=%+v admission=%+v", cfg.MCP.Enabled, cfg.DatabasePools, cfg.Admission)
	}
	values["BT_DB_MCP_MAX_CONNS"], values["BT_ADMISSION_MCP_MAX"] = "1", "1"
	if cfg, err := Load(mapGetter(values)); err != nil || cfg.DatabasePools.MCP != 1 || cfg.Admission.MCPMax != 1 {
		t.Fatalf("minimum enabled profile: %+v err=%v", cfg.DatabasePools, err)
	}
}

func TestLoadRejectsEnabledMCPWithoutAValidBudget(t *testing.T) {
	cases := map[string]func(map[string]string){
		"legacy database pools": func(v map[string]string) {
			for key := range laboratoryPools() {
				delete(v, key)
			}
		},
		"zero mcp pool":            func(v map[string]string) { v["BT_DB_MCP_MAX_CONNS"] = "0" },
		"mcp pool above four":      func(v map[string]string) { v["BT_DB_MCP_MAX_CONNS"] = "5" },
		"zero mcp admission":       func(v map[string]string) { v["BT_ADMISSION_MCP_MAX"] = "0" },
		"mcp admission above four": func(v map[string]string) { v["BT_ADMISSION_MCP_MAX"] = "5" },
		"mcp admission not below total": func(v map[string]string) {
			v["BT_ADMISSION_TOTAL"], v["BT_ADMISSION_MCP_MAX"] = "4", "4"
		},
		"aggregate budget exceeded": func(v map[string]string) { v["BT_DB_RESERVED_CONNECTIONS"] = "44" },
		"mismatched pool and admission maximum": func(v map[string]string) {
			v["BT_DB_MCP_MAX_CONNS"], v["BT_ADMISSION_MCP_MAX"] = "4", "1"
		},
		"missing cursor key":   func(v map[string]string) { delete(v, "BT_MCP_CURSOR_KEY_FILE") },
		"missing database url": func(v map[string]string) { delete(v, "BT_MCP_DATABASE_URL_FILE") },
		"hosted public mode": func(v map[string]string) {
			hosted := validHostedEnvironment(t)
			for key := range v {
				if _, ok := hosted[key]; !ok && !strings.HasPrefix(key, "BT_MCP_") && !strings.HasPrefix(key, "BT_DB_") && !strings.HasPrefix(key, "BT_ADMISSION_") {
					delete(v, key)
				}
			}
			for key, value := range hosted {
				v[key] = value
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			values := enabledMCPEnvironment(t)
			mutate(values)
			if _, err := Load(mapGetter(values)); err == nil {
				t.Fatal("unsafe enabled MCP configuration accepted")
			}
		})
	}
}

func TestLoadDisabledMCPReservesNothing(t *testing.T) {
	values := validSelfHostedEnvironment(t)
	cfg, err := Load(mapGetter(values))
	if err != nil || cfg.MCP.Enabled || cfg.DatabasePools.MCP != 0 || cfg.Admission.MCPMax != 0 {
		t.Fatalf("default=%+v err=%v", cfg.MCP, err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"mcp pool without mcp": func(v map[string]string) {
			for key, value := range laboratoryPools() {
				v[key] = value
			}
			v["BT_DB_MCP_MAX_CONNS"] = "2"
		},
		"mcp admission without mcp": func(v map[string]string) { v["BT_ADMISSION_MCP_MAX"] = "2" },
	} {
		t.Run(name, func(t *testing.T) {
			values := validSelfHostedEnvironment(t)
			mutate(values)
			if _, err := Load(mapGetter(values)); err == nil {
				t.Fatal("disabled MCP accepted a reserved MCP budget")
			}
		})
	}
}
