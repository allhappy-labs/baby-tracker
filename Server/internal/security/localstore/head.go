package localstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"babytracker/server/internal/security"
)

type Heads struct {
	dir      string
	path     string
	lockPath string
	mu       sync.Mutex
}

type diskHead struct {
	Generation uint64 `json:"generation"`
	Revision   uint64 `json:"revision"`
	EventHash  string `json:"event_hash"`
}

func (heads *Heads) Load(ctx context.Context) (security.Head, error) {
	if err := ctx.Err(); err != nil {
		return security.Head{}, err
	}
	data, err := readFileBounded(ctx, heads.path, 4096)
	if errors.Is(err, os.ErrNotExist) {
		return security.Head{}, security.ErrTrustedHeadRequired
	}
	if errors.Is(err, errFileTooLarge) {
		return security.Head{}, security.ErrLedgerCorrupt
	}
	if err != nil {
		return security.Head{}, err
	}
	return decodeHead(data)
}

func (heads *Heads) CompareAndSwap(ctx context.Context, expected, next security.Head) (bool, error) {
	heads.mu.Lock()
	defer heads.mu.Unlock()
	lock, err := acquireFileLock(ctx, heads.lockPath, syscall.LOCK_EX)
	if err != nil {
		return false, err
	}
	defer releaseFileLock(lock)
	current, err := heads.Load(ctx)
	if err != nil {
		return false, err
	}
	if current != expected {
		return false, nil
	}
	if next.Generation != expected.Generation || next.Revision != expected.Revision+1 || next.EventHash == ([32]byte{}) {
		return false, security.ErrLedgerCorrupt
	}
	if err := heads.write(next); err != nil {
		return false, err
	}
	return true, nil
}

func (heads *Heads) initialize(ctx context.Context, head security.Head) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	heads.mu.Lock()
	defer heads.mu.Unlock()
	lock, err := acquireFileLock(ctx, heads.lockPath, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer releaseFileLock(lock)
	if _, err := os.Stat(heads.path); err == nil {
		return errors.New("trusted head already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if head != (security.Head{Generation: 1}) {
		return security.ErrLedgerCorrupt
	}
	return heads.write(head)
}

func (heads *Heads) write(head security.Head) error {
	encoded, err := json.Marshal(toDiskHead(head))
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	temporary := filepath.Join(heads.dir, fmt.Sprintf("head-g%d-r%d-%x.tmp", head.Generation, head.Revision, head.EventHash))
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(temporary)
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
	if err := os.Rename(temporary, heads.path); err != nil {
		return err
	}
	remove = false
	return syncDir(heads.dir)
}

func toDiskHead(head security.Head) diskHead {
	return diskHead{Generation: head.Generation, Revision: head.Revision, EventHash: hex.EncodeToString(head.EventHash[:])}
}

func fromDiskHead(head diskHead) (security.Head, error) {
	if head.Generation == 0 || head.Generation > 9007199254740991 || head.Revision > 9007199254740991 || len(head.EventHash) != 64 {
		return security.Head{}, security.ErrLedgerCorrupt
	}
	decoded, err := hex.DecodeString(head.EventHash)
	if err != nil || hex.EncodeToString(decoded) != head.EventHash {
		return security.Head{}, security.ErrLedgerCorrupt
	}
	var eventHash [32]byte
	copy(eventHash[:], decoded)
	if head.Revision == 0 && eventHash != [32]byte{} {
		return security.Head{}, security.ErrLedgerCorrupt
	}
	return security.Head{Generation: head.Generation, Revision: head.Revision, EventHash: eventHash}, nil
}

func decodeHead(data []byte) (security.Head, error) {
	var disk diskHead
	if err := decodeDiskJSON(data, &disk, 4096); err != nil {
		return security.Head{}, security.ErrLedgerCorrupt
	}
	canonical, err := json.Marshal(disk)
	if err != nil || !bytes.Equal(data, append(canonical, '\n')) {
		return security.Head{}, security.ErrLedgerCorrupt
	}
	return fromDiskHead(disk)
}

func acquireFileLock(ctx context.Context, path string, how int) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		if err := syscall.Flock(int(file.Fd()), how|syscall.LOCK_NB); err == nil {
			return file, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func releaseFileLock(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}
