package loadtest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
)

// LedgerEntry is one write the server acknowledged as applied: the entity,
// the version it reached and the SHA-256 of the wire form the generator sent.
// The audit (Task 11) checks the database holds exactly the latest of each.
type LedgerEntry struct {
	FamilyID   string `json:"family_id"`
	EntityID   string `json:"entity_id"`
	Kind       string `json:"kind"`
	Version    uint64 `json:"version"`
	Deleted    bool   `json:"deleted,omitempty"`
	WireSHA256 string `json:"wire_sha256"`
}

// LedgerWriter appends entries as JSON lines. It is safe for concurrent use.
type LedgerWriter struct {
	mu     sync.Mutex
	f      *os.File
	w      *bufio.Writer
	closed bool
}

// CreateLedger creates a new private (0600) ledger. It never opens an
// existing file: a ledger from another run must not be extended or replaced.
func CreateLedger(path string) (*LedgerWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &LedgerWriter{f: f, w: bufio.NewWriterSize(f, 1<<16)}, nil
}

func (l *LedgerWriter) Append(e LedgerEntry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("ledger: append after close")
	}
	_, err = l.w.Write(line)
	return err
}

// Close flushes, syncs and closes the file.
func (l *LedgerWriter) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	return errors.Join(l.w.Flush(), l.f.Sync(), l.f.Close())
}

// ReadLedger returns the latest version of every (family, entity), ordered by
// family then entity. Two entries for one version must be identical: a
// version acknowledged with two different contents is an error, not a
// choice, whatever order the concurrent appends landed in.
func ReadLedger(path string) ([]LedgerEntry, error) {
	entries, _, err := ReadLedgerCounted(path)
	return entries, err
}

// ReadLedgerCounted is ReadLedger that also returns how many entries the
// file holds: every acknowledged write, a retried acknowledgement included,
// where the entries returned are one per entity.
func ReadLedgerCounted(path string) ([]LedgerEntry, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	type key struct{ family, entity string }
	// Every version seen per entity: appends from concurrent writers can
	// land out of order (v, v+1, then v again), so a repeated version is
	// checked against every earlier one, not only the latest.
	versions := map[key]map[uint64]LedgerEntry{}
	latest := map[key]LedgerEntry{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		var e LedgerEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, 0, fmt.Errorf("ledger %s line %d: %w", path, n, err)
		}
		k := key{e.FamilyID, e.EntityID}
		seen := versions[k]
		if seen == nil {
			seen = map[uint64]LedgerEntry{}
			versions[k] = seen
		}
		if prev, ok := seen[e.Version]; ok && prev != e {
			return nil, 0, fmt.Errorf("ledger %s line %d: family %s entity %s version %d acknowledged with two contents", path, n, e.FamilyID, e.EntityID, e.Version)
		}
		seen[e.Version] = e
		if prev, ok := latest[k]; !ok || e.Version > prev.Version {
			latest[k] = e
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("ledger %s: %w", path, err)
	}
	out := make([]LedgerEntry, 0, len(latest))
	for _, e := range latest {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b LedgerEntry) int {
		if c := strings.Compare(a.FamilyID, b.FamilyID); c != 0 {
			return c
		}
		return strings.Compare(a.EntityID, b.EntityID)
	})
	return out, n, nil
}
