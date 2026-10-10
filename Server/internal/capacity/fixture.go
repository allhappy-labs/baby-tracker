// Package capacity contains the synthetic capacity harness's public metadata.
// Fixture construction and private credentials exist only in identitytest builds.
package capacity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type FixtureSpec struct {
	Families           int
	DevicesPerFamily   int
	AuthorityEvents    int
	CareEntriesPerBaby int
	// MCPGrantsPerFamily private MCP grants are issued for every family, in
	// every fixture used by matched cells, so authority history is equal
	// whether or not a cell enables MCP.
	MCPGrantsPerFamily int
}

// FixtureManifest is safe for JSON reports. It contains no credentials or care payloads.
type FixtureManifest struct {
	Version               int
	SourceSHA             string
	InputSHA256           string
	Families              int
	Devices               int
	MCPGrants             int
	ActualAuthorityEvents uint64
	CreatedAt             time.Time
	ExpiresAt             time.Time
}

var ErrFixtureInvalid = errors.New("capacity_fixture_invalid")
var ErrUnsafeTarget = errors.New("capacity_target_not_owned_disposable")
var sourcePattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

func (s FixtureSpec) validate() error {
	// These are finite fixture-construction bounds, not supported capacity claims.
	if s.Families < 1 || s.Families > 4096 || s.DevicesPerFamily < 1 || s.DevicesPerFamily > 64 || s.AuthorityEvents < 0 || s.AuthorityEvents > 100000 || s.CareEntriesPerBaby < 0 || s.CareEntriesPerBaby > 100000 || s.MCPGrantsPerFamily < 0 || s.MCPGrantsPerFamily > 4 {
		return ErrFixtureInvalid
	}
	return nil
}

func fixtureInputDigest(spec FixtureSpec, source string) string {
	raw, _ := json.Marshal(struct {
		Spec      FixtureSpec
		SourceSHA string
	}{spec, source})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (m FixtureManifest) Validate(spec FixtureSpec, source string, now time.Time) error {
	if spec.validate() != nil || m.Version != 1 || !sourcePattern.MatchString(source) || m.SourceSHA != source || m.InputSHA256 != fixtureInputDigest(spec, source) || m.Families != spec.Families || m.Devices != spec.Families*spec.DevicesPerFamily || m.MCPGrants != spec.Families*spec.MCPGrantsPerFamily || m.ActualAuthorityEvents == 0 || m.ActualAuthorityEvents < uint64(spec.AuthorityEvents) || m.CreatedAt.IsZero() || m.CreatedAt.After(now) || !m.ExpiresAt.After(m.CreatedAt) || !now.Before(m.ExpiresAt) {
		return ErrFixtureInvalid
	}
	return nil
}

// A destination is a new child of an existing caller-owned directory. All
// ancestors must be real directories: even an ancestor symlink is refused.
func disposableDestination(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) || strings.Contains(path, "://") {
		return ErrUnsafeTarget
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return ErrUnsafeTarget
	}
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrUnsafeTarget
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return ErrUnsafeTarget
	}
	for part := parent; ; part = filepath.Dir(part) {
		info, err := os.Lstat(part)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeTarget
		}
		if part == filepath.Dir(part) {
			break
		}
	}
	return nil
}
