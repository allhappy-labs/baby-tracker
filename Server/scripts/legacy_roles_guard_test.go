package scripts

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// legacyRoleName matches any of the six roles of main's deployment the
// cutover's R6 dropped: the projector, Apple-revoker and Live Activity worker
// groups and their _app logins.
var legacyRoleName = regexp.MustCompile(`baby_tracker_(projector|apple_revoker|live_activity)(_app)?\b`)

// No server, deployment or script file names a role R6 dropped: a server
// check that names one (pg_has_role, has_*_privilege, a GRANT) fails with
// 42704 since the homelab's roles are gone, as export_worker.go's
// maintenance pool check did before Task 27. Tests and testdata are exempt.
func TestNoServerFileNamesALegacyRole(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(rel, "_test.go") || !d.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if match := legacyRoleName.Find(raw); match != nil {
			offenders = append(offenders, rel+": "+string(match))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) != 0 {
		t.Fatalf("server files name a role R6 dropped:\n%s", strings.Join(offenders, "\n"))
	}
}
