package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLoadAuthReadsStrictDistinctPepperFiles(t *testing.T) {
	directory := t.TempDir()
	values := fileAuthEnvironment(t, directory)
	got, err := LoadAuth(mapGetter(values), ModeHosted)
	if err != nil {
		t.Fatal(err)
	}
	if got.IdentityKeys.Current.ID != "identity.v1" || got.IdentityKeyID != "identity.v1" || got.IdentityPepper == got.RateLimitPepper {
		t.Fatalf("auth config=%+v", got)
	}
}

func TestSixDigestKeyringsLoadWithoutImportCycle(t *testing.T) {
	values := developmentAuthEnvironment(t)
	got, err := LoadAuth(mapGetter(values), ModeDevelopment)
	if err != nil {
		t.Fatal(err)
	}
	for name, ring := range map[string]DigestKeyringConfig{
		"identity": got.IdentityKeys, "session": got.SessionTokenKeys,
		"device": got.DeviceContinuityKeys, "notification": got.AppleNotificationKeys,
		"invitation": got.InvitationKeys, "page": got.PageTokenKeys,
	} {
		if ring.Current.ID == "" || len(ring.Previous) != 0 {
			t.Fatalf("%s ring=%+v", name, ring)
		}
	}
}

func TestPageTokenKeyRequiresExactMountedFileName(t *testing.T) {
	for _, alias := range []string{"BT_PAGE_TOKEN_KEY", "BT_PAGE_TOKEN_PEPPER_FILE"} {
		t.Run(alias, func(t *testing.T) {
			values := developmentAuthEnvironment(t)
			delete(values, "BT_PAGE_TOKEN_KEY_FILE")
			values[alias] = strings.Repeat("ab", 32)
			if _, err := LoadAuth(mapGetter(values), ModeDevelopment); err == nil || !strings.Contains(err.Error(), "BT_PAGE_TOKEN_KEY_FILE") {
				t.Fatalf("wrong alias accepted or unsafe error: %v", err)
			}
		})
	}
}

func TestDigestKeyringsWriteCurrentReadPrevious(t *testing.T) {
	directory := t.TempDir()
	newerSecret := writePepperFile(t, directory, "identity-newer", "07")
	olderSecret := writePepperFile(t, directory, "identity-older", "08")
	manifest := filepath.Join(directory, "identity-previous.json")
	encoded, err := json.Marshal([]map[string]string{
		{"key_id": "identity.v0", "pepper_file": newerSecret},
		{"key_id": "identity.v00", "pepper_file": olderSecret},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	values := developmentAuthEnvironment(t)
	values["BT_IDENTITY_PREVIOUS_KEYS_FILE"] = manifest
	got, err := LoadAuth(mapGetter(values), ModeDevelopment)
	if err != nil {
		t.Fatal(err)
	}
	if got.IdentityKeys.Current.ID != "identity.v1" || len(got.IdentityKeys.Previous) != 2 ||
		got.IdentityKeys.Previous[0].ID != "identity.v0" || got.IdentityKeys.Previous[1].ID != "identity.v00" {
		t.Fatalf("identity ring=%+v", got.IdentityKeys)
	}
}

func TestInvitationKeyringReadsRetainedKeys(t *testing.T) {
	directory := t.TempDir()
	previousSecret := writePepperFile(t, directory, "invitation-previous", "09")
	manifest := filepath.Join(directory, "invitation-previous.json")
	writeJSONFile(t, manifest, []map[string]string{{
		"key_id": "invitation.v0", "pepper_file": previousSecret,
	}})
	values := developmentAuthEnvironment(t)
	values["BT_INVITATION_PREVIOUS_KEYS_FILE"] = manifest

	got, err := LoadAuth(mapGetter(values), ModeDevelopment)
	if err != nil {
		t.Fatal(err)
	}
	if got.InvitationKeys.Current.ID != "invitation.v1" || len(got.InvitationKeys.Previous) != 1 || got.InvitationKeys.Previous[0].ID != "invitation.v0" {
		t.Fatalf("invitation ring=%+v", got.InvitationKeys)
	}
}

// Catches accepting manifests that can exceed the eight-candidate lookup
// bound, smuggle secret material, or identify previous secret files ambiguously.
func TestDigestKeyringPreviousManifestIsStrictAndBounded(t *testing.T) {
	directory := t.TempDir()
	manifest := filepath.Join(directory, "previous.json")
	values := developmentAuthEnvironment(t)
	values["BT_IDENTITY_PREVIOUS_KEYS_FILE"] = manifest

	entries := make([]map[string]string, 0, 8)
	for index := 0; index < 8; index++ {
		path := writePepperFile(t, directory, fmt.Sprintf("pepper-%d", index), fmt.Sprintf("%02x", index+7))
		entries = append(entries, map[string]string{
			"key_id":      fmt.Sprintf("identity.old%d", index),
			"pepper_file": path,
		})
	}
	writeJSONFile(t, manifest, entries[:7])
	if got, err := LoadAuth(mapGetter(values), ModeDevelopment); err != nil || len(got.IdentityKeys.Previous) != 7 {
		t.Fatalf("seven previous keys rejected: ring=%+v err=%v", got.IdentityKeys, err)
	}

	tests := map[string][]map[string]string{
		"eight entries": entries,
		"current ID":    {{"key_id": "identity.v1", "pepper_file": entries[0]["pepper_file"]}},
		"relative path": {{"key_id": "identity.old", "pepper_file": "relative/pepper"}},
		"secret field":  {{"key_id": "identity.old", "pepper_file": entries[0]["pepper_file"], "secret": strings.Repeat("09", 32)}},
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			writeJSONFile(t, manifest, body)
			if _, err := LoadAuth(mapGetter(values), ModeDevelopment); err == nil {
				t.Fatal("invalid previous-key manifest accepted")
			}
		})
	}

	t.Run("oversized manifest", func(t *testing.T) {
		if err := os.WriteFile(manifest, []byte("["+strings.Repeat(" ", 16*1024)+"]"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadAuth(mapGetter(values), ModeDevelopment); err == nil {
			t.Fatal("oversized previous-key manifest accepted")
		}
	})
}

// Catches previous-key files bypassing the same strict parser used by current,
// permanent actor-ID, and rate-limit peppers.
func TestDigestKeyringPreviousPepperFilesAreStrict(t *testing.T) {
	directory := t.TempDir()
	valid := writePepperFile(t, directory, "valid", "07")
	manifest := filepath.Join(directory, "previous.json")
	values := developmentAuthEnvironment(t)
	values["BT_IDENTITY_PREVIOUS_KEYS_FILE"] = manifest

	mutations := map[string]func() string{
		"uppercase": func() string {
			path := filepath.Join(directory, "uppercase")
			if err := os.WriteFile(path, []byte(strings.Repeat("AB", 32)), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		},
		"extra newline": func() string {
			path := filepath.Join(directory, "extra-newline")
			if err := os.WriteFile(path, []byte(strings.Repeat("07", 32)+"\n\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		},
		"symlink": func() string {
			path := filepath.Join(directory, "symlink")
			if err := os.Symlink(valid, path); err != nil {
				t.Fatal(err)
			}
			return path
		},
		"directory": func() string { return directory },
	}
	for name, create := range mutations {
		t.Run(name, func(t *testing.T) {
			writeJSONFile(t, manifest, []map[string]string{{"key_id": "identity.old", "pepper_file": create()}})
			if _, err := LoadAuth(mapGetter(values), ModeDevelopment); err == nil {
				t.Fatal("unsafe previous pepper accepted")
			}
		})
	}
}

func TestActorIDPepperIsPermanentAndDistinct(t *testing.T) {
	for name, mutate := range map[string]func(map[string]string){
		"digest key": func(values map[string]string) {
			values["BT_ACTOR_ID_PEPPER"] = values["BT_IDENTITY_PEPPER"]
		},
		"rate limit pepper": func(values map[string]string) {
			values["BT_ACTOR_ID_PEPPER"] = values["BT_RATE_LIMIT_PEPPER"]
		},
	} {
		t.Run(name, func(t *testing.T) {
			values := developmentAuthEnvironment(t)
			mutate(values)
			if _, err := LoadAuth(mapGetter(values), ModeDevelopment); err == nil {
				t.Fatal("duplicate actor ID pepper accepted")
			}
		})
	}
}

func TestDigestKeyringsRejectDuplicateIDsAndSecrets(t *testing.T) {
	for name, mutate := range map[string]func(map[string]string){
		"cross-ring secret": func(v map[string]string) { v["BT_SESSION_TOKEN_PEPPER"] = v["BT_IDENTITY_PEPPER"] },
		"invitation secret": func(v map[string]string) { v["BT_INVITATION_PEPPER"] = v["BT_IDENTITY_PEPPER"] },
		"rate-limit secret": func(v map[string]string) { v["BT_RATE_LIMIT_PEPPER"] = v["BT_DEVICE_CONTINUITY_PEPPER"] },
		"actor secret":      func(v map[string]string) { v["BT_ACTOR_ID_PEPPER"] = v["BT_APPLE_NOTIFICATION_PEPPER"] },
	} {
		t.Run(name, func(t *testing.T) {
			values := developmentAuthEnvironment(t)
			mutate(values)
			if _, err := LoadAuth(mapGetter(values), ModeDevelopment); err == nil {
				t.Fatal("duplicate secret accepted")
			}
		})
	}

	t.Run("previous duplicate ID and secret", func(t *testing.T) {
		directory := t.TempDir()
		first := filepath.Join(directory, "first")
		second := filepath.Join(directory, "second")
		if err := os.WriteFile(first, []byte(strings.Repeat("07", 32)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(second, []byte(strings.Repeat("08", 32)), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest := filepath.Join(directory, "previous.json")
		values := developmentAuthEnvironment(t)
		values["BT_IDENTITY_PREVIOUS_KEYS_FILE"] = manifest
		for _, body := range []string{
			`[{"key_id":"identity.v0","pepper_file":"` + first + `"},{"key_id":"identity.v0","pepper_file":"` + second + `"}]`,
			`[{"key_id":"identity.v0","pepper_file":"` + first + `"},{"key_id":"identity.v00","pepper_file":"` + first + `"}]`,
			`[{"key_id":"identity.v0","key_id":"identity.v00","pepper_file":"` + first + `"}]`,
		} {
			if err := os.WriteFile(manifest, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadAuth(mapGetter(values), ModeDevelopment); err == nil {
				t.Fatalf("duplicate manifest accepted: %s", body)
			}
		}
	})
}

func TestLegacySessionSigningAlias(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "session")
	if err := os.WriteFile(path, []byte(strings.Repeat("02", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	values := developmentAuthEnvironment(t)
	delete(values, "BT_SESSION_TOKEN_PEPPER")
	values["BT_SESSION_SIGNING_KEY_FILE"] = path
	if _, err := LoadAuth(mapGetter(values), ModeDevelopment); err != nil {
		t.Fatalf("legacy alias rejected: %v", err)
	}
	values["BT_SESSION_TOKEN_PEPPER_FILE"] = path
	if _, err := LoadAuth(mapGetter(values), ModeDevelopment); err == nil {
		t.Fatal("canonical and legacy session paths accepted together")
	}
}

// Catches accidentally making the deprecated name mandatory after the
// canonical session-token pepper source became the primary configuration.
func TestCanonicalSessionTokenPepperDoesNotRequireLegacyAlias(t *testing.T) {
	values := validHostedEnvironment(t)
	values["BT_SESSION_TOKEN_PEPPER_FILE"] = values["BT_SESSION_SIGNING_KEY_FILE"]
	delete(values, "BT_SESSION_SIGNING_KEY_FILE")

	got, err := Load(mapGetter(values))
	if err != nil {
		t.Fatalf("canonical session-token source rejected: %v", err)
	}
	if got.Auth.SessionTokenKeys.Current.ID != "session.v1" {
		t.Fatalf("session token ring=%+v", got.Auth.SessionTokenKeys)
	}
}

func TestLoadAuthRejectsUnsafeSecretSources(t *testing.T) {
	directory := t.TempDir()
	valid := filepath.Join(directory, "valid")
	if err := os.WriteFile(valid, []byte(strings.Repeat("01", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, "link")
	if err := os.Symlink(valid, symlink); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"uppercase":   func(v map[string]string) { v["BT_IDENTITY_PEPPER"] = strings.Repeat("AB", 32) },
		"whitespace":  func(v map[string]string) { v["BT_IDENTITY_PEPPER"] = " " + strings.Repeat("01", 32) },
		"both":        func(v map[string]string) { v["BT_IDENTITY_PEPPER_FILE"] = valid },
		"symlink":     func(v map[string]string) { delete(v, "BT_IDENTITY_PEPPER"); v["BT_IDENTITY_PEPPER_FILE"] = symlink },
		"same keys":   func(v map[string]string) { v["BT_RATE_LIMIT_PEPPER"] = v["BT_IDENTITY_PEPPER"] },
		"bad key id":  func(v map[string]string) { v["BT_IDENTITY_KEY_ID"] = "Identity-v1" },
		"test issuer": func(v map[string]string) { v["BT_TEST_ISSUER"] = "enabled" },
	} {
		t.Run(name, func(t *testing.T) {
			values := developmentAuthEnvironment(t)
			mutate(values)
			if _, err := LoadAuth(mapGetter(values), ModeDevelopment); err == nil {
				t.Fatal("unsafe auth config accepted")
			}
		})
	}
}

func TestLoadAuthProductionAcceptsFilesOnly(t *testing.T) {
	values := developmentAuthEnvironment(t)
	if _, err := LoadAuth(mapGetter(values), ModeSelfHosted); err == nil {
		t.Fatal("production direct secrets accepted")
	}
}

func TestLoadAuthRejectsFIFOWithoutBlocking(t *testing.T) {
	directory := t.TempDir()
	fifo := filepath.Join(directory, "identity-fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	values := developmentAuthEnvironment(t)
	delete(values, "BT_IDENTITY_PEPPER")
	values["BT_IDENTITY_PEPPER_FILE"] = fifo
	result := make(chan error, 1)
	go func() {
		_, err := LoadAuth(mapGetter(values), ModeDevelopment)
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("FIFO secret source accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO secret source blocked configuration loading")
	}
}

func developmentAuthEnvironment(t *testing.T) map[string]string {
	t.Helper()
	pageToken := writePepperFile(t, t.TempDir(), "page-token", "aa")
	return map[string]string{
		"BT_IDENTITY_KEY_ID": "identity.v1", "BT_IDENTITY_PEPPER": strings.Repeat("01", 32),
		"BT_SESSION_TOKEN_KEY_ID": "session.v1", "BT_SESSION_TOKEN_PEPPER": strings.Repeat("02", 32),
		"BT_DEVICE_CONTINUITY_KEY_ID": "device.v1", "BT_DEVICE_CONTINUITY_PEPPER": strings.Repeat("03", 32),
		"BT_APPLE_NOTIFICATION_KEY_ID": "notification.v1", "BT_APPLE_NOTIFICATION_PEPPER": strings.Repeat("04", 32),
		"BT_INVITATION_KEY_ID": "invitation.v1", "BT_INVITATION_PEPPER": strings.Repeat("05", 32),
		"BT_PAGE_TOKEN_KEY_ID": "page.v1", "BT_PAGE_TOKEN_KEY_FILE": pageToken,
		"BT_ACTOR_ID_PEPPER":   strings.Repeat("ee", 32),
		"BT_RATE_LIMIT_PEPPER": strings.Repeat("ff", 32),
	}
}

func writePepperFile(t *testing.T, directory, name, pair string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(strings.Repeat(pair, 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileAuthEnvironment(t *testing.T, directory string) map[string]string {
	t.Helper()
	values := map[string]string{}
	for index, item := range []struct{ prefix, id string }{
		{"BT_IDENTITY", "identity.v1"}, {"BT_SESSION_TOKEN", "session.v1"},
		{"BT_DEVICE_CONTINUITY", "device.v1"}, {"BT_APPLE_NOTIFICATION", "notification.v1"},
		{"BT_INVITATION", "invitation.v1"}, {"BT_PAGE_TOKEN", "page.v1"},
	} {
		path := filepath.Join(directory, strings.ToLower(item.id))
		ending := "\n"
		if index%2 == 1 {
			ending = "\r\n"
		}
		if err := os.WriteFile(path, []byte(strings.Repeat(string("123458"[index]), 64)+ending), 0o600); err != nil {
			t.Fatal(err)
		}
		values[item.prefix+"_KEY_ID"] = item.id
		if item.prefix == "BT_PAGE_TOKEN" {
			values["BT_PAGE_TOKEN_KEY_FILE"] = path
		} else {
			values[item.prefix+"_PEPPER_FILE"] = path
		}
	}
	actor := filepath.Join(directory, "actor")
	rate := filepath.Join(directory, "rate")
	if err := os.WriteFile(actor, []byte(strings.Repeat("6", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rate, []byte(strings.Repeat("7", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	values["BT_ACTOR_ID_PEPPER_FILE"], values["BT_RATE_LIMIT_PEPPER_FILE"] = actor, rate
	return values
}

func TestProductionRejectsTestIssuerConfig(t *testing.T) {
	values := validHostedEnvironment(t)
	values["BT_TEST_ISSUER"] = "https://issuer.invalid"
	if _, err := Load(mapGetter(values)); err == nil {
		t.Fatal("production accepted a test issuer")
	}
}
