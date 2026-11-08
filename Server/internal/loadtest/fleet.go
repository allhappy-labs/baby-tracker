package loadtest

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// The fleet file is the seeded devices' private state: every credential a
// load run needs to act as the devices the seeder provisioned, and the
// progress each device has made. It is written by `baby-tracker-load seed`
// and checkpointed by `baby-tracker-load run`, always into a private
// (0700) directory, as one 0600 file replaced atomically.
//
// Format (JSON lines): the FleetHeader on the first line, then one
// DeviceState per line, Devices of them. The credentials are throwaway
// material minted against a disposable stack's throwaway peppers; the file
// is never copied into the repository or a result directory's public part.

// FleetVersion is the fleet file format version WriteFleet writes and
// ReadFleet accepts.
const FleetVersion = 1

// DeviceState is one seeded device, as a load run drives it.
type DeviceState struct {
	FamilyID, AccountID, DeviceID, Role string // role: owner|caregiver|viewer
	BabyIDs                             []string
	AccessToken, RefreshToken           string
	DeviceCredential                    string // device continuity, for refresh-recoverable and ack
	AccessExpiresAt, ReauthenticateAt   time.Time
	Cursor                              uint64 // last acknowledged family sequence
	ClientIP                            string // 100.64.0.0/10, unique per device
	PendingAttemptID                    string `json:",omitempty"` // a refresh whose answer was lost
	MCPCredential                       string `json:",omitempty"`
	LiveActivityToken, PushToken        string `json:",omitempty"`
	// PendingImport is an import this device started and has not finished
	// (checkpointed before its first chunk): the next import cell resumes
	// it, as the iOS importer resumes its frozen manifest.
	PendingImport *PendingImport `json:",omitempty"`
	// ImportBlockedUntil: the account holds an unfinished import this fleet
	// cannot resume (import_unfinished_limit on a new import), so the import
	// cell passes the device over until the server's staging TTL ends it.
	ImportBlockedUntil *time.Time `json:",omitempty"`
}

// PendingImport names a frozen import: its plan is a function of the two
// (newImportPlan).
type PendingImport struct {
	ImportID string
	FrozenAt time.Time
}

// FleetHeader describes the seed a fleet came from. SeedSpecSHA256 is the
// SHA-256 of the seed spec's JSON, AsOf the seed's reference time.
// HotFamilyID names the seed's hot family (`seed --hot-family`: 5 accounts
// × 2 devices, the hot-family scenario's writers), counted in Families.
type FleetHeader struct {
	Version          int
	SeedSpecSHA256   string
	AsOf             time.Time
	Families         int
	Devices          int
	ServerInstanceID string
	RecoveryEpoch    uint64
	HotFamilyID      string `json:",omitempty"`
}

// maxFleetLine bounds one line of the fleet file (a device's state is a
// few kilobytes at most).
const maxFleetLine = 1 << 20

// LiveActivityInstallationID is the Live Activity installation the seeder
// registered for deviceID (registration version 1): a UUIDv4-shaped
// function of the device, so a load run can register the same installation
// again at a later version without the fleet carrying it.
func LiveActivityInstallationID(deviceID string) string {
	sum := sha256.Sum256([]byte("baby-tracker-load/live-activity-installation\x00" + deviceID))
	return uuidV4(sum[:16])
}

// uuidV4 shapes 16 bytes as a lowercase hyphenated UUIDv4.
func uuidV4(b []byte) string {
	var value [16]byte
	copy(value[:], b)
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	h := hex.EncodeToString(value[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// PrivateDirectory refuses a directory that is not a real directory owned
// by this user with no group or other permissions.
func PrivateDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("loadtest: fleet directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("loadtest: fleet directory is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("loadtest: fleet directory %s is mode %v; it must be private (0700)", dir, info.Mode().Perm())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Getuid()) {
		return errors.New("loadtest: fleet directory belongs to another user")
	}
	return nil
}

// WriteFleet writes the header and devices to path atomically: a 0600
// temporary file in the same directory, synced, then renamed over path. It
// refuses a directory that is not private (0700) and a header that does not
// count its devices.
func WriteFleet(path string, h FleetHeader, devices []DeviceState) error {
	return writeFleet(path, h, devices, 0)
}

// fleetPaceChunk is how much a paced fleet write syncs at a time.
const fleetPaceChunk = 1 << 20

// pacedFile syncs every fleetPaceChunk written and then waits pace: a run's
// minute checkpoint (about 28 MiB for 40,000 devices) reaches the disk the
// database shares as a trickle of 1 MiB flushes, not one burst. In the v4
// soak, that burst stalled WAL writes for 220–310 ms every minute, and three
// of those stalls drew 503s.
type pacedFile struct {
	*os.File
	pace    time.Duration
	pending int
}

func (p *pacedFile) Write(b []byte) (int, error) {
	n, err := p.File.Write(b)
	if p.pending += n; err == nil && p.pending >= fleetPaceChunk {
		p.pending = 0
		if err = p.File.Sync(); err == nil {
			time.Sleep(p.pace)
		}
	}
	return n, err
}

// writeFleet is WriteFleet, paced (pacedFile) when pace is positive.
func writeFleet(path string, h FleetHeader, devices []DeviceState, pace time.Duration) (resultErr error) {
	if h.Version != FleetVersion {
		return fmt.Errorf("loadtest: fleet version %d, want %d", h.Version, FleetVersion)
	}
	if h.Devices != len(devices) {
		return fmt.Errorf("loadtest: fleet header counts %d devices, %d given", h.Devices, len(devices))
	}
	dir := filepath.Dir(path)
	if err := PrivateDirectory(dir); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".fleet-*.tmp")
	if err != nil {
		return fmt.Errorf("loadtest: fleet: %w", err)
	}
	temporary := file.Name()
	defer func() {
		if resultErr != nil {
			_ = file.Close()
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("loadtest: fleet: %w", err)
	}
	var sink io.Writer = file
	if pace > 0 {
		sink = &pacedFile{File: file, pace: pace}
	}
	writer := bufio.NewWriterSize(sink, fleetPaceChunk)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(h); err != nil {
		return fmt.Errorf("loadtest: fleet header: %w", err)
	}
	for i := range devices {
		if err := encoder.Encode(devices[i]); err != nil {
			return fmt.Errorf("loadtest: fleet device %d: %w", i, err)
		}
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("loadtest: fleet: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("loadtest: fleet: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("loadtest: fleet: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("loadtest: fleet: %w", err)
	}
	// The rename is durable once the directory is synced.
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

// ReadFleet reads a fleet WriteFleet wrote. It refuses a file readable by
// group or others, an unknown version, unknown fields and a device count
// that differs from the header's.
func ReadFleet(path string) (FleetHeader, []DeviceState, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return FleetHeader{}, nil, fmt.Errorf("loadtest: fleet: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return FleetHeader{}, nil, fmt.Errorf("loadtest: fleet: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return FleetHeader{}, nil, fmt.Errorf("loadtest: fleet %s is mode %v; it must be a private (0600) file", path, info.Mode().Perm())
	}
	reader := bufio.NewReaderSize(file, 1<<20)
	line, err := readFleetLine(reader)
	if err != nil {
		return FleetHeader{}, nil, fmt.Errorf("loadtest: fleet header: %w", err)
	}
	var h FleetHeader
	if err := decodeStrict(line, &h); err != nil {
		return FleetHeader{}, nil, fmt.Errorf("loadtest: fleet header: %w", err)
	}
	if h.Version != FleetVersion || h.Devices < 0 {
		return FleetHeader{}, nil, fmt.Errorf("loadtest: fleet version %d, want %d", h.Version, FleetVersion)
	}
	devices := make([]DeviceState, 0, h.Devices)
	for {
		line, err := readFleetLine(reader)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return FleetHeader{}, nil, fmt.Errorf("loadtest: fleet device %d: %w", len(devices), err)
		}
		var device DeviceState
		if err := decodeStrict(line, &device); err != nil {
			return FleetHeader{}, nil, fmt.Errorf("loadtest: fleet device %d: %w", len(devices), err)
		}
		devices = append(devices, device)
	}
	if len(devices) != h.Devices {
		return FleetHeader{}, nil, fmt.Errorf("loadtest: fleet holds %d devices, its header %d", len(devices), h.Devices)
	}
	return h, devices, nil
}

// readFleetLine is the next line without its newline; io.EOF at the end.
func readFleetLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := reader.ReadLine()
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > maxFleetLine {
			return nil, errors.New("line too long")
		}
		if !isPrefix {
			return line, nil
		}
	}
}

func decodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing data")
	}
	return nil
}

// CheckFresh refuses a seed whose sessions may not outlive a run started
// now: one older than lineageAge (the session lineage's age, past which
// every seeded device must sign in again) less a day of margin.
func (h FleetHeader) CheckFresh(now time.Time, lineageAge time.Duration) error {
	if h.AsOf.IsZero() {
		return errors.New("loadtest: the fleet names no seed time")
	}
	age := now.Sub(h.AsOf)
	if limit := lineageAge - 24*time.Hour; age > limit {
		return fmt.Errorf("loadtest: the seed is %s old, past %s (its %s lineage less a day); seed again", age.Round(time.Minute), limit, lineageAge)
	}
	return nil
}
