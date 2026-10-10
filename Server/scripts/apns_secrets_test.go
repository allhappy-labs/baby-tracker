package scripts

import (
	"bytes"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// Owner decision D4: the production APNs credentials are provided by the
// operator at deploy and never committed. The identifiers are built from
// halves so this file does not itself match.
var forbiddenAPNSIdentifiers = []string{
	"Z894" + "KG4587", // the production key ID
	"9XN7" + "WN8JN2", // the owner's team ID
}

var pemPrivateKeyHeaders = []string{
	"-----BEGIN " + "PRIVATE KEY-----",
	"-----BEGIN " + "EC PRIVATE KEY-----",
}

// apnsSecretFindings reports every tracked file (paths relative to the
// server directory) that is or holds APNs key material.
func apnsSecretFindings(t *testing.T, root string, files []string) []string {
	t.Helper()
	var findings []string
	for _, name := range files {
		base := path.Base(name)
		if strings.HasPrefix(base, "AuthKey_") || strings.HasSuffix(strings.ToLower(base), ".p8") {
			findings = append(findings, name+": an APNs key file name")
		}
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if os.IsNotExist(err) {
			continue // tracked but deleted in the working tree
		}
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, header := range pemPrivateKeyHeaders {
			if bytes.Contains(content, []byte(header)) {
				findings = append(findings, name+": a PEM private key")
			}
		}
		for index, identifier := range forbiddenAPNSIdentifiers {
			if bytes.Contains(content, []byte(identifier)) {
				findings = append(findings, name+": production APNs identifier #"+string(rune('1'+index)))
			}
		}
	}
	return findings
}

func trackedServerFiles(t *testing.T) []string {
	t.Helper()
	command := exec.Command("git", "ls-files", "-z", "--", ".")
	command.Dir = ".."
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, name := range strings.Split(string(output), "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	return files
}

func TestNoProductionAPNsKeyInTheServerTree(t *testing.T) {
	files := trackedServerFiles(t)
	found := map[string]bool{}
	for _, name := range files {
		found[name] = true
	}
	// The scan covers the README and the code beside it.
	for _, required := range []string{"README.md", "go.mod", "internal/config/config.go"} {
		if !found[required] {
			t.Fatalf("git ls-files does not list %s: the scan would miss tracked files", required)
		}
	}
	if findings := apnsSecretFindings(t, "..", files); len(findings) != 0 {
		t.Fatalf("production APNs key material is tracked under Server/:\n%s", strings.Join(findings, "\n"))
	}
}

// The scan itself catches each kind of leak.
func TestAPNsSecretScanCatchesEveryForm(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]string{
		"deploy/AuthKey_ABC.txt": "nothing",
		"deploy/key.P8":          "nothing",
		// Written independently of the lists above, so a weakened list fails.
		"README.md":               "Team: " + "9XN7" + "WN8JN2",
		"internal/x/x.go":         "const keyID = \"" + "Z894" + "KG4587" + "\"",
		"testdata/pkcs8.pem":      "-----BEGIN PRIVATE" + " KEY-----\nAAAA\n",
		"testdata/sec1.pem":       "-----BEGIN EC PRIVATE" + " KEY-----\nAAAA\n",
		"internal/clean/clean.go": "package clean // BEGIN PUBLIC KEY is fine",
	}
	var names []string
	for name, content := range cases {
		write(name, content)
		names = append(names, name)
	}
	findings := apnsSecretFindings(t, root, names)
	joined := strings.Join(findings, "\n")
	for name := range cases {
		if name == "internal/clean/clean.go" {
			if strings.Contains(joined, name) {
				t.Fatalf("a clean file was flagged: %s", joined)
			}
			continue
		}
		if !strings.Contains(joined, name+":") {
			t.Fatalf("%s was not flagged:\n%s", name, joined)
		}
	}
	for _, identifier := range forbiddenAPNSIdentifiers {
		if strings.Contains(joined, identifier) {
			t.Fatal("a finding prints the identifier it found")
		}
	}
}
