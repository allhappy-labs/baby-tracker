package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMCPSecret(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMCPIsDisabledByDefaultWithoutSecrets(t *testing.T) {
	for _, enabled := range []string{"", "false"} {
		got, err := LoadMCP(func(name string) string {
			switch name {
			case "BT_MCP_ENABLED":
				return enabled
			case "BT_MCP_DATABASE_URL_FILE", "BT_MCP_CURSOR_KEY_FILE":
				// Disabled mode never reads (or requires) MCP secrets.
				return "/nonexistent/secret"
			}
			return ""
		})
		if err != nil || got.Enabled || got.DatabaseURL != "" || got.CursorKey != ([32]byte{}) {
			t.Fatalf("enabled=%q: got=%+v err=%v", enabled, got, err)
		}
	}
	if _, err := LoadMCP(nil); err == nil {
		t.Fatal("nil getter accepted")
	}
}

func TestLoadMCPEnabledReadsProtectedSecretFiles(t *testing.T) {
	dir := t.TempDir()
	key := strings.Repeat("0f", 32)
	values := map[string]string{
		"BT_MCP_ENABLED":           "true",
		"BT_MCP_DATABASE_URL_FILE": writeMCPSecret(t, dir, "url", "postgres://mcp:secret@db/baby\n"),
		"BT_MCP_CURSOR_KEY_FILE":   writeMCPSecret(t, dir, "key", key+"\n"),
	}
	got, err := LoadMCP(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.DatabaseURL != "postgres://mcp:secret@db/baby" || got.CursorKey[0] != 0x0f || got.CursorKey[31] != 0x0f {
		t.Fatalf("got=%+v", got)
	}
}

func TestLoadMCPRejectsInvalidEnabledConfiguration(t *testing.T) {
	dir := t.TempDir()
	goodURL := writeMCPSecret(t, dir, "url", "postgres://mcp:secret@db/baby\n")
	goodKey := writeMCPSecret(t, dir, "key", strings.Repeat("ab", 32))
	link := filepath.Join(dir, "link")
	if err := os.Symlink(goodKey, link); err != nil {
		t.Fatal(err)
	}
	cases := map[string]map[string]string{
		"bad flag":            {"BT_MCP_ENABLED": "yes", "BT_MCP_DATABASE_URL_FILE": goodURL, "BT_MCP_CURSOR_KEY_FILE": goodKey},
		"uppercase flag":      {"BT_MCP_ENABLED": "TRUE", "BT_MCP_DATABASE_URL_FILE": goodURL, "BT_MCP_CURSOR_KEY_FILE": goodKey},
		"missing url":         {"BT_MCP_ENABLED": "true", "BT_MCP_CURSOR_KEY_FILE": goodKey},
		"missing key":         {"BT_MCP_ENABLED": "true", "BT_MCP_DATABASE_URL_FILE": goodURL},
		"empty url file":      {"BT_MCP_ENABLED": "true", "BT_MCP_DATABASE_URL_FILE": writeMCPSecret(t, dir, "empty", ""), "BT_MCP_CURSOR_KEY_FILE": goodKey},
		"short key":           {"BT_MCP_ENABLED": "true", "BT_MCP_DATABASE_URL_FILE": goodURL, "BT_MCP_CURSOR_KEY_FILE": writeMCPSecret(t, dir, "short", strings.Repeat("ab", 31))},
		"uppercase key":       {"BT_MCP_ENABLED": "true", "BT_MCP_DATABASE_URL_FILE": goodURL, "BT_MCP_CURSOR_KEY_FILE": writeMCPSecret(t, dir, "upper", strings.Repeat("AB", 32))},
		"zero key":            {"BT_MCP_ENABLED": "true", "BT_MCP_DATABASE_URL_FILE": goodURL, "BT_MCP_CURSOR_KEY_FILE": writeMCPSecret(t, dir, "zero", strings.Repeat("00", 32))},
		"raw 32 bytes":        {"BT_MCP_ENABLED": "true", "BT_MCP_DATABASE_URL_FILE": goodURL, "BT_MCP_CURSOR_KEY_FILE": writeMCPSecret(t, dir, "raw", strings.Repeat("k", 32))},
		"symlinked key":       {"BT_MCP_ENABLED": "true", "BT_MCP_DATABASE_URL_FILE": goodURL, "BT_MCP_CURSOR_KEY_FILE": link},
		"directory key":       {"BT_MCP_ENABLED": "true", "BT_MCP_DATABASE_URL_FILE": goodURL, "BT_MCP_CURSOR_KEY_FILE": dir},
		"missing key file":    {"BT_MCP_ENABLED": "true", "BT_MCP_DATABASE_URL_FILE": goodURL, "BT_MCP_CURSOR_KEY_FILE": filepath.Join(dir, "absent")},
		"inline database url": {"BT_MCP_ENABLED": "true", "BT_MCP_DATABASE_URL": "postgres://inline", "BT_MCP_CURSOR_KEY_FILE": goodKey},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := LoadMCP(func(key string) string { return values[key] })
			if err == nil {
				t.Fatalf("accepted %+v", got)
			}
			for _, secret := range []string{"secret", "abab", dir} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaks %q: %v", secret, err)
				}
			}
		})
	}
}
