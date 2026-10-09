package localstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"babytracker/server/internal/security"
	"golang.org/x/sys/unix"
)

const maxCommittedRevision = 9007199254740991

var _ security.IntegrityLedger = (*Ledger)(nil)

func validInventoryHead(head security.Head) bool {
	return head.Generation > 0 && head.Generation <= maxCommittedRevision && head.Revision <= maxCommittedRevision &&
		(head.Revision != 0 || head.EventHash == ([32]byte{}))
}

func selectedFilename(revision uint64, hash [32]byte) string {
	return fmt.Sprintf("%020d-%x.json", revision, hash)
}

func (ledger *Ledger) generationRoot(head security.Head) (*os.File, error) {
	baseFD, err := unix.Open(ledger.dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, security.ErrLedgerCorrupt
	}
	base := os.NewFile(uintptr(baseFD), ledger.dir)
	defer base.Close()
	name := fmt.Sprintf("generation-%020d", head.Generation)
	fd, err := unix.Openat(baseFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, security.ErrLedgerCorrupt
	}
	root := os.NewFile(uintptr(fd), name)
	info, err := root.Stat()
	if err != nil || !info.IsDir() {
		_ = root.Close()
		return nil, security.ErrLedgerCorrupt
	}
	return root, nil
}

// readSelected reads one bounded regular envelope through an anchored directory.
// The selected name is always checked against the expected canonical name first.
func (ledger *Ledger) readSelected(ctx context.Context, root *os.File, file security.CommittedFile) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if file.SelectedName != selectedFilename(file.Revision, file.EventHash) {
		return nil, security.ErrLedgerCorrupt
	}
	if ledger.selectedBeforeOpenHook != nil {
		ledger.selectedBeforeOpenHook()
	}
	fd, err := unix.Openat(int(root.Fd()), file.SelectedName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, security.ErrLedgerCorrupt
	}
	opened := os.NewFile(uintptr(fd), file.SelectedName)
	defer opened.Close()
	openedInfo, err := opened.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() > 68<<10 {
		return nil, security.ErrLedgerCorrupt
	}
	var reader io.Reader = opened
	if ledger.selectedChunkHook != nil {
		reader = observedReader{file: opened, afterRead: ledger.selectedChunkHook}
	}
	data, err := io.ReadAll(contextReader{ctx: ctx, reader: &io.LimitedReader{R: reader, N: (68 << 10) + 1}})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) > 68<<10 {
		return nil, security.ErrLedgerCorrupt
	}
	if ledger.selectedReadHook != nil {
		ledger.selectedReadHook()
	}
	return data, nil
}

type observedReader struct {
	file      *os.File
	afterRead func(*os.File)
}

func (reader observedReader) Read(buffer []byte) (int, error) {
	n, err := reader.file.Read(buffer)
	if n > 0 {
		reader.afterRead(reader.file)
	}
	return n, err
}

func (ledger *Ledger) ReadValidatedInventory(ctx context.Context, head security.Head) (security.CommittedInventory, []security.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return security.CommittedInventory{}, nil, err
	}
	if !validInventoryHead(head) {
		return security.CommittedInventory{}, nil, security.ErrLedgerCorrupt
	}
	if head.Revision == 0 {
		return security.CommittedInventory{Head: head}, nil, nil
	}
	root, err := ledger.generationRoot(head)
	if err != nil {
		return security.CommittedInventory{}, nil, err
	}
	defer root.Close()
	var files []security.CommittedFile
	var candidates []security.Candidate
	required := head.EventHash
	for revision := head.Revision; revision > 0; revision-- {
		file := security.CommittedFile{Revision: revision, EventHash: required, SelectedName: selectedFilename(revision, required)}
		data, err := ledger.readSelected(ctx, root, file)
		if err != nil {
			return security.CommittedInventory{}, nil, err
		}
		// The prior is learned from the strictly decoded selected envelope.
		candidate, err := decodeSelectedPrior(data, file)
		if err != nil || candidate.Prior.Generation != head.Generation || candidate.Prior.Revision != revision-1 {
			return security.CommittedInventory{}, nil, security.ErrLedgerCorrupt
		}
		file.Prior, file.RawDigest = candidate.Prior, sha256.Sum256(data)
		files, candidates = append(files, file), append(candidates, candidate)
		required = candidate.Prior.EventHash
	}
	if required != ([32]byte{}) {
		return security.CommittedInventory{}, nil, security.ErrLedgerCorrupt
	}
	reverseFiles(files)
	reverseCandidates(candidates)
	return security.CommittedInventory{Head: head, Files: files}, candidates, nil
}

func decodeSelectedPrior(data []byte, file security.CommittedFile) (security.Candidate, error) {
	var disk diskCandidate
	if err := decodeDiskJSON(data, &disk, 68<<10); err != nil {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	canonical, err := json.Marshal(disk)
	canonical = append(canonical, '\n')
	if err != nil || !bytes.Equal(data, canonical) {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	prior, err := fromDiskHead(disk.Prior)
	if err != nil || disk.EventHash != fmt.Sprintf("%x", file.EventHash) || sha256.Sum256(disk.EventJSON) != file.EventHash {
		return security.Candidate{}, security.ErrLedgerCorrupt
	}
	return security.Candidate{CommandID: disk.CommandID, Prior: prior, EventJSON: append([]byte(nil), disk.EventJSON...), EventHash: file.EventHash}, nil
}

func (ledger *Ledger) VerifyInventory(ctx context.Context, inventory security.CommittedInventory) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validInventoryHead(inventory.Head) || uint64(len(inventory.Files)) != inventory.Head.Revision {
		return security.ErrLedgerCorrupt
	}
	if len(inventory.Files) == 0 {
		return nil
	}
	root, err := ledger.generationRoot(inventory.Head)
	if err != nil {
		return err
	}
	defer root.Close()
	prior := security.Head{Generation: inventory.Head.Generation}
	for i, file := range inventory.Files {
		if file.Revision != uint64(i)+1 || file.Prior != prior || file.EventHash == ([32]byte{}) {
			return security.ErrLedgerCorrupt
		}
		data, err := ledger.readSelected(ctx, root, file)
		if err != nil {
			return err
		}
		if sha256.Sum256(data) != file.RawDigest {
			return security.ErrLedgerCorrupt
		}
		prior = security.Head{Generation: inventory.Head.Generation, Revision: file.Revision, EventHash: file.EventHash}
	}
	if prior != inventory.Head {
		return security.ErrLedgerCorrupt
	}
	return nil
}

func (ledger *Ledger) ReadValidatedExtension(ctx context.Context, inventory security.CommittedInventory, next security.Head) (security.CommittedInventory, []security.Candidate, error) {
	if err := ledger.VerifyInventory(ctx, inventory); err != nil {
		return security.CommittedInventory{}, nil, err
	}
	if !validInventoryHead(next) || next.Generation != inventory.Head.Generation || next.Revision < inventory.Head.Revision {
		return security.CommittedInventory{}, nil, security.ErrLedgerCorrupt
	}
	if next.Revision == inventory.Head.Revision {
		if next != inventory.Head {
			return security.CommittedInventory{}, nil, security.ErrLedgerCorrupt
		}
		return cloneInventory(inventory), nil, nil
	}
	root, err := ledger.generationRoot(next)
	if err != nil {
		return security.CommittedInventory{}, nil, err
	}
	defer root.Close()
	var suffix []security.CommittedFile
	var candidates []security.Candidate
	required := next.EventHash
	for revision := next.Revision; revision > inventory.Head.Revision; revision-- {
		file := security.CommittedFile{Revision: revision, EventHash: required, SelectedName: selectedFilename(revision, required)}
		data, err := ledger.readSelected(ctx, root, file)
		if err != nil {
			return security.CommittedInventory{}, nil, err
		}
		candidate, err := decodeSelectedPrior(data, file)
		if err != nil || candidate.Prior.Generation != next.Generation || candidate.Prior.Revision != revision-1 {
			return security.CommittedInventory{}, nil, security.ErrLedgerCorrupt
		}
		file.Prior, file.RawDigest = candidate.Prior, sha256.Sum256(data)
		suffix, candidates = append(suffix, file), append(candidates, candidate)
		required = candidate.Prior.EventHash
	}
	if required != inventory.Head.EventHash {
		return security.CommittedInventory{}, nil, security.ErrLedgerCorrupt
	}
	reverseFiles(suffix)
	reverseCandidates(candidates)
	files := make([]security.CommittedFile, 0, len(inventory.Files)+len(suffix))
	files = append(files, inventory.Files...)
	files = append(files, suffix...)
	return security.CommittedInventory{Head: next, Files: files}, candidates, nil
}

func cloneInventory(inventory security.CommittedInventory) security.CommittedInventory {
	return security.CommittedInventory{Head: inventory.Head, Files: append([]security.CommittedFile(nil), inventory.Files...)}
}

func reverseFiles(files []security.CommittedFile) {
	for left, right := 0, len(files)-1; left < right; left, right = left+1, right-1 {
		files[left], files[right] = files[right], files[left]
	}
}

func reverseCandidates(candidates []security.Candidate) {
	for left, right := 0, len(candidates)-1; left < right; left, right = left+1, right-1 {
		candidates[left], candidates[right] = candidates[right], candidates[left]
	}
}
