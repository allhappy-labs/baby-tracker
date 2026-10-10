package scripts

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The legacy importer was a one-time tool: the homelab cut over on
// 2026-10-10 (docs/verification/2026-10-10-homelab-cutover.md) and Phase 4
// Task 29 deleted the importer and all its tooling. Its runbook is kept, as
// the record of what ran, in docs/verification/, which this test does not
// scan. The names are built from halves so this file does not itself match.
var (
	legacyImportPaths = []string{
		"legacy" + "import", "legacy" + "ledger", "import" + "_legacy",
		"legacy" + "-cutover", "legacy" + "_cutover",
		"capture" + "-legacy", "capture" + "_legacy",
		"rehearse" + "-legacy", "legacy" + "-import",
		"homelab" + "-preflight", "homelab" + "_preflight",
	}
	legacyImportStrings = []string{
		"import" + "-legacy", "legacy" + "_public",
		"legacy" + "-cutover", "rehearse" + "-legacy", "homelab" + "-preflight",
	}
)

func TestNoLegacyImportRemains(t *testing.T) {
	command := exec.Command("git", "ls-files", "-z", "--", "Server", "docs/operations")
	command.Dir = filepath.Join("..", "..")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var offenders []string
	sawServer := false
	for _, name := range strings.Split(string(output), "\x00") {
		if name == "" {
			continue
		}
		for _, needle := range legacyImportPaths {
			if strings.Contains(name, needle) {
				offenders = append(offenders, name+": a path naming "+needle)
				break
			}
		}
		if !strings.HasPrefix(name, "Server/") {
			continue
		}
		sawServer = true
		content, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(name)))
		if os.IsNotExist(err) {
			continue // tracked but deleted in the working tree
		}
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, needle := range legacyImportStrings {
			if bytes.Contains(content, []byte(needle)) {
				offenders = append(offenders, name+": contains "+needle)
			}
		}
	}
	if !sawServer {
		t.Fatal("git ls-files listed no Server/ file: the scan would miss tracked files")
	}
	if len(offenders) != 0 {
		t.Fatalf("the one-time legacy importer or its tooling remains:\n%s", strings.Join(offenders, "\n"))
	}
}
