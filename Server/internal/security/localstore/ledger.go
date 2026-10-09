package localstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"babytracker/server/internal/security"
)

type Store struct {
	Ledger security.SecurityLedger
	Heads  security.LedgerHeadStore
	Fence  security.Fence

	ledger *Ledger
	heads  *Heads
	fence  *FileFence
}

type Ledger struct {
	dir                    string
	mu                     sync.Mutex
	selectedReadHook       func()         // test observation after one selected envelope read
	selectedBeforeOpenHook func()         // test substitution at selected-file open boundary
	selectedChunkHook      func(*os.File) // test cancellation while reading an open descriptor
}

type diskCandidate struct {
	CommandID string          `json:"command_id"`
	Prior     diskHead        `json:"prior"`
	EventJSON json.RawMessage `json:"event"`
	EventHash string          `json:"event_hash"`
}

func Open(ledgerDir, headDir string) (Store, error) {
	ledgerPath, err := prepareDirectory(ledgerDir)
	if err != nil {
		return Store{}, err
	}
	headPath, err := prepareDirectory(headDir)
	if err != nil {
		return Store{}, err
	}
	ledger := &Ledger{dir: ledgerPath}
	heads := &Heads{dir: headPath, path: filepath.Join(headPath, "head.json"), lockPath: filepath.Join(headPath, "head.lock")}
	fence := &FileFence{path: filepath.Join(headPath, "fence.lock")}
	return Store{Ledger: ledger, Heads: heads, Fence: fence, ledger: ledger, heads: heads, fence: fence}, nil
}

func prepareDirectory(input string) (string, error) {
	if input == "" {
		return "", errors.New("security store directory is required")
	}
	abs, err := filepath.Abs(input)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("security store path must be a directory")
	}
	return abs, nil
}

func (store Store) InitializeEmpty(ctx context.Context) error {
	if store.ledger == nil || store.heads == nil || store.fence == nil {
		return errors.New("invalid security store")
	}
	release, err := store.fence.Write(ctx)
	if err != nil {
		return err
	}
	defer release()
	return store.InitializeEmptyUnderFence(ctx)
}

// InitializeEmptyUnderFence initializes or verifies the exact empty authority
// state for the recovery initializer that already holds this store's write
// fence. Callers must not use it outside that coordinated operation.
func (store Store) InitializeEmptyUnderFence(ctx context.Context) error {
	if store.ledger == nil || store.heads == nil || store.fence == nil {
		return errors.New("invalid security store")
	}
	entries, err := os.ReadDir(store.ledger.dir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("security ledger is not empty")
	}
	headEntries, err := os.ReadDir(store.heads.dir)
	if err != nil {
		return err
	}
	genesisTemporary := fmt.Sprintf("head-g%d-r%d-%x.tmp", 1, 0, [32]byte{})
	for _, entry := range headEntries {
		if entry.Name() == genesisTemporary {
			info, infoErr := entry.Info()
			if infoErr != nil || !info.Mode().IsRegular() {
				return errors.New("security head store contains invalid genesis temporary artifact")
			}
			if err := os.Remove(filepath.Join(store.heads.dir, genesisTemporary)); err != nil {
				return err
			}
			continue
		}
		if entry.Name() != "fence.lock" && entry.Name() != "head.lock" && entry.Name() != "head.json" {
			return errors.New("security head store is not empty")
		}
	}
	if _, err := os.Stat(store.heads.path); err == nil {
		head, loadErr := store.heads.Load(ctx)
		if loadErr != nil || head != (security.Head{Generation: 1}) {
			return errors.New("trusted head already exists with non-genesis state")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return store.heads.initialize(ctx, security.Head{Generation: 1})
}

func (ledger *Ledger) AppendCandidate(ctx context.Context, candidate security.Candidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if candidate.Prior.Generation == 0 || candidate.CommandID == "" || len(candidate.EventJSON) == 0 || sha256.Sum256(candidate.EventJSON) != candidate.EventHash {
		return security.ErrLedgerCorrupt
	}
	disk := diskCandidate{candidate.CommandID, toDiskHead(candidate.Prior), json.RawMessage(candidate.EventJSON), hex.EncodeToString(candidate.EventHash[:])}
	encoded, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	generationDir := filepath.Join(ledger.dir, fmt.Sprintf("generation-%020d", candidate.Prior.Generation))
	filename := fmt.Sprintf("%020d-%s.json", candidate.Prior.Revision+1, disk.EventHash)
	path := filepath.Join(generationDir, filename)

	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if err := os.MkdirAll(generationDir, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := readFileBounded(ctx, path, 68<<10)
		if readErr != nil {
			if errors.Is(readErr, errFileTooLarge) {
				return security.ErrLedgerCorrupt
			}
			return readErr
		}
		if !bytes.Equal(existing, encoded) {
			return security.ErrLedgerCorrupt
		}
		return nil
	}
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(encoded); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := syncDir(generationDir); err != nil {
		return err
	}
	if err := syncDir(ledger.dir); err != nil {
		return err
	}
	remove = false
	return nil
}

func (ledger *Ledger) ReadCommitted(ctx context.Context, head security.Head) ([]security.Candidate, error) {
	result := make([]security.Candidate, 0, head.Revision)
	err := ledger.WalkCommitted(ctx, head, func(candidate security.Candidate) error {
		result = append(result, candidate)
		return nil
	})
	return result, err
}

func (ledger *Ledger) WalkCommitted(ctx context.Context, head security.Head, visit func(security.Candidate) error) error {
	if head.Generation == 0 || visit == nil {
		return security.ErrLedgerCorrupt
	}
	if head.Revision == 0 {
		if head.EventHash != [32]byte{} {
			return security.ErrLedgerCorrupt
		}
		return nil
	}
	generationDir := filepath.Join(ledger.dir, fmt.Sprintf("generation-%020d", head.Generation))
	if head.Revision > 9007199254740991 || head.Revision > uint64(^uint64(0)>>1)/32 {
		return security.ErrLedgerCorrupt
	}
	index, err := os.CreateTemp(ledger.dir, ".walk-*.tmp")
	if err != nil {
		return err
	}
	indexPath := index.Name()
	defer func() { _ = index.Close(); _ = os.Remove(indexPath) }()
	if err := index.Chmod(0o600); err != nil {
		return err
	}
	requiredHash := head.EventHash
	for revision := head.Revision; revision >= 1; revision-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(generationDir, fmt.Sprintf("%020d-%x.json", revision, requiredHash))
		selected, err := readCandidate(ctx, path)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if err != nil || selected.EventHash != requiredHash || selected.Prior.Generation != head.Generation || selected.Prior.Revision+1 != revision {
			return security.ErrLedgerCorrupt
		}
		offset := int64((revision - 1) * 32)
		if written, err := index.WriteAt(requiredHash[:], offset); err != nil || written != 32 {
			return security.ErrLedgerCorrupt
		}
		requiredHash = selected.Prior.EventHash
		if revision == 1 {
			break
		}
	}
	if requiredHash != ([32]byte{}) {
		return security.ErrLedgerCorrupt
	}
	var encodedHash [32]byte
	for revision := uint64(1); revision <= head.Revision; revision++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if read, err := index.ReadAt(encodedHash[:], int64((revision-1)*32)); err != nil || read != 32 {
			return security.ErrLedgerCorrupt
		}
		path := filepath.Join(generationDir, fmt.Sprintf("%020d-%x.json", revision, encodedHash))
		candidate, err := readCandidate(ctx, path)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if err != nil || candidate.EventHash != encodedHash {
			return security.ErrLedgerCorrupt
		}
		if err := visit(candidate); err != nil {
			return err
		}
	}
	return nil
}

func readCandidate(ctx context.Context, path string) (security.Candidate, error) {
	data, err := readFileBounded(ctx, path, 68<<10)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return security.Candidate{}, err
		}
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	var disk diskCandidate
	if err := decodeDiskJSON(data, &disk, 68<<10); err != nil {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	canonical, err := json.Marshal(disk)
	if err != nil || !bytes.Equal(data, append(canonical, '\n')) {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	prior, err := fromDiskHead(disk.Prior)
	if err != nil {
		return security.Candidate{}, err
	}
	hashBytes, err := hex.DecodeString(disk.EventHash)
	if err != nil || len(hashBytes) != 32 || strings.ToLower(disk.EventHash) != disk.EventHash {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	var eventHash [32]byte
	copy(eventHash[:], hashBytes)
	base := filepath.Base(path)
	if !strings.HasSuffix(base, "-"+disk.EventHash+".json") {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	return security.Candidate{CommandID: disk.CommandID, Prior: prior, EventJSON: append([]byte(nil), disk.EventJSON...), EventHash: eventHash}, nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
