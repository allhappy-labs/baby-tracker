package protocol

import (
	"encoding/json"
	"fmt"
	"slices"
)

type Surface string

const (
	SurfaceAuth         Surface = "auth"
	SurfaceContinuity   Surface = "auth-continuity"
	SurfaceFamily       Surface = "family"
	SurfaceRecovery     Surface = "recovery"
	SurfaceSync         Surface = "sync"
	SurfaceSyncTimer    Surface = "sync-timer"
	SurfaceSnapshot     Surface = "snapshot"
	SurfaceImport       Surface = "import"
	SurfaceLive         Surface = "live"
	SurfaceExport       Surface = "export"
	SurfaceLiveActivity Surface = "live-activity"
	SurfacePushToken    Surface = "push-token"
)

func RequiredCapabilities(surface Surface) []string {
	switch surface {
	case SurfaceAuth:
		return []string{CapabilityIdentityV1}
	case SurfaceContinuity:
		return []string{CapabilityIdentityV1, CapabilityDeviceContinuityV1}
	case SurfaceFamily:
		return []string{CapabilityFamilyLifecycleV1}
	case SurfaceRecovery:
		return []string{CapabilityRecoveryV1}
	case SurfaceSync:
		return []string{CapabilityRecordsV1}
	case SurfaceSyncTimer:
		return []string{CapabilityRecordsV1, CapabilityTimerCommandsV1}
	case SurfaceSnapshot:
		return []string{CapabilityRecordsV1, CapabilitySnapshotV1}
	case SurfaceImport:
		return []string{CapabilityRecordsV1, CapabilityImportResolverV1}
	case SurfaceLive:
		return []string{CapabilityRecordsV1, CapabilityLiveHintsV1}
	case SurfaceExport:
		return []string{CapabilityExportV1}
	case SurfaceLiveActivity:
		return []string{CapabilityLiveActivityPushV1}
	case SurfacePushToken:
		return []string{CapabilityPushTokenV1}
	default:
		return nil
	}
}

func ValidateRouteCapabilities(surface Surface, header, body, advertised []string) error {
	required := RequiredCapabilities(surface)
	if required == nil || ValidateCapabilities(advertised, KnownCapabilities()) != nil ||
		ValidateCapabilities(required, advertised) != nil || ValidateCapabilities(header, advertised) != nil {
		return errUnsupportedCapability
	}
	normalizedHeader := slices.Clone(header)
	slices.Sort(normalizedHeader)
	normalizedRequired := slices.Clone(required)
	slices.Sort(normalizedRequired)
	if !slices.Equal(normalizedHeader, normalizedRequired) {
		return errUnsupportedCapability
	}
	if body != nil {
		if ValidateCapabilities(body, advertised) != nil {
			return errUnsupportedCapability
		}
		normalizedBody := slices.Clone(body)
		slices.Sort(normalizedBody)
		if !slices.Equal(normalizedHeader, normalizedBody) {
			return errUnsupportedCapability
		}
	}
	return nil
}

type Limits struct {
	MaxLiveBabies                   int   `json:"max_live_babies"`
	MaxCanonicalRecordsPerFamily    int64 `json:"max_canonical_records_per_family"`
	MaxChargedStorageBytesPerFamily int64 `json:"max_charged_storage_bytes_per_family"`
	SyncBodyBytes                   int   `json:"sync_body_bytes"`
	SyncBatchSize                   int   `json:"sync_batch_size"`
	SyncPageSize                    int   `json:"sync_page_size"`
	SyncPageBytes                   int   `json:"sync_page_bytes"`
	SyncMutationBytes               int   `json:"sync_mutation_bytes"`
	SyncPageTTLSeconds              int   `json:"sync_page_ttl_seconds"`
	SnapshotTTLSeconds              int   `json:"snapshot_ttl_seconds"`
	SnapshotMaxEntities             int   `json:"snapshot_max_entities"`
	SnapshotMaxBytes                int   `json:"snapshot_max_bytes"`
	SyncTransactionSeconds          int   `json:"sync_transaction_seconds"`
	ImportMaxRecords                int   `json:"import_max_records"`
	ImportMaxBabies                 int   `json:"import_max_babies"`
	ImportChunkSize                 int   `json:"import_chunk_size"`
	ImportStagingTTLHours           int   `json:"import_staging_ttl_hours"`
	ImportMaxSessionsPerFamily      int   `json:"import_max_sessions_per_family"`
	ImportMaxSessionsPerAccount     int   `json:"import_max_sessions_per_account"`
	ActiveSnapshotAccountLimit      int   `json:"active_snapshot_account_limit"`
	ActiveSnapshotFamilyLimit       int   `json:"active_snapshot_family_limit"`
	UnfinishedImportAccountLimit    int   `json:"unfinished_import_account_limit"`
	UnfinishedImportFamilyLimit     int   `json:"unfinished_import_family_limit"`
	SocketDeviceLimit               int   `json:"socket_device_limit"`
	SocketAccountLimit              int   `json:"socket_account_limit"`
	SocketProcessLimit              int   `json:"socket_process_limit"`
	SocketAttemptIPPerMinute        int   `json:"socket_attempt_ip_per_minute"`
	SocketAttemptDevicePerMinute    int   `json:"socket_attempt_device_per_minute"`
	SocketAttemptAccountPerMinute   int   `json:"socket_attempt_account_per_minute"`
	SocketAttemptFamilyPerMinute    int   `json:"socket_attempt_family_per_minute"`
	MaxCapabilities                 int   `json:"max_capabilities"`
	MaxNameScalars                  int   `json:"max_name_scalars"`
	MaxNameBytes                    int   `json:"max_name_bytes"`
	MaxNoteBytes                    int   `json:"max_note_bytes"`
	MaxFoodScalars                  int   `json:"max_food_scalars"`
	MaxFoodBytes                    int   `json:"max_food_bytes"`
	MaxFoods                        int   `json:"max_foods"`
	MaxSideIntervals                int   `json:"max_side_intervals"`
	MaxSideSeconds                  int   `json:"max_side_seconds"`
	MaxSideTotalSeconds             int   `json:"max_side_total_seconds"`
	MaxSortOrder                    int   `json:"max_sort_order"`
	SyncMutationDevicePerMinute     int   `json:"sync_mutation_device_per_minute"`
	SyncMutationAccountPerMinute    int   `json:"sync_mutation_account_per_minute"`
	SyncMutationFamilyPerMinute     int   `json:"sync_mutation_family_per_minute"`
	SyncPullDevicePerMinute         int   `json:"sync_pull_device_per_minute"`
	SyncPullAccountPerMinute        int   `json:"sync_pull_account_per_minute"`
}

type EffectiveLimits = Limits

func DefaultLimits() Limits {
	return Limits{
		MaxLiveBabies: 20, MaxCanonicalRecordsPerFamily: 100000,
		MaxChargedStorageBytesPerFamily: 256 << 20, SyncBodyBytes: 1 << 20, SyncBatchSize: 100,
		SyncPageSize: 200, SyncPageBytes: 1 << 20, SyncMutationBytes: 32 << 10,
		SyncPageTTLSeconds: 900, SnapshotTTLSeconds: 900, SnapshotMaxEntities: 250000, SnapshotMaxBytes: 128 << 20,
		SyncTransactionSeconds: 10, ImportMaxRecords: 100000, ImportMaxBabies: 100,
		ImportChunkSize: 1000, ImportStagingTTLHours: 168,
		ImportMaxSessionsPerFamily: 1000, ImportMaxSessionsPerAccount: 100,
		ActiveSnapshotAccountLimit: 2, ActiveSnapshotFamilyLimit: 8,
		UnfinishedImportAccountLimit: 1, UnfinishedImportFamilyLimit: 4,
		SocketDeviceLimit: 1, SocketAccountLimit: 3, SocketProcessLimit: 1000,
		SocketAttemptIPPerMinute: 60, SocketAttemptDevicePerMinute: 12,
		SocketAttemptAccountPerMinute: 36, SocketAttemptFamilyPerMinute: 120,
		MaxCapabilities: 16, MaxNameScalars: 100, MaxNameBytes: 400, MaxNoteBytes: 4096,
		MaxFoodScalars: 100, MaxFoodBytes: 400, MaxFoods: 50, MaxSideIntervals: 256,
		MaxSideSeconds: 604800, MaxSideTotalSeconds: 604800, MaxSortOrder: 1000000,
		SyncMutationDevicePerMinute: 1200, SyncMutationAccountPerMinute: 2400,
		SyncMutationFamilyPerMinute: 6000, SyncPullDevicePerMinute: 120,
		SyncPullAccountPerMinute: 240,
	}
}

func (limits Limits) Validate() error {
	if limits.MaxCanonicalRecordsPerFamily < 100 || limits.MaxCanonicalRecordsPerFamily > 1000000 {
		return fmt.Errorf("max_canonical_records_per_family must be from 100 through 1000000")
	}
	if limits.MaxChargedStorageBytesPerFamily < 16<<20 || limits.MaxChargedStorageBytesPerFamily > int64(4)<<30 {
		return fmt.Errorf("max_charged_storage_bytes_per_family must be from %d through %d", 16<<20, int64(4)<<30)
	}
	checks := []struct {
		name                    string
		value, minimum, maximum int
	}{
		{"max_live_babies", limits.MaxLiveBabies, 1, 100},
		{"sync_body_bytes", limits.SyncBodyBytes, 65536, 4194304},
		{"sync_batch_size", limits.SyncBatchSize, 1, 200},
		{"sync_page_size", limits.SyncPageSize, 1, 1000},
		{"sync_page_bytes", limits.SyncPageBytes, MinimumSyncPageBytes, 4 << 20},
		{"sync_mutation_bytes", limits.SyncMutationBytes, 4096, 65536},
		{"sync_page_ttl_seconds", limits.SyncPageTTLSeconds, 60, 3600},
		{"snapshot_ttl_seconds", limits.SnapshotTTLSeconds, 60, 3600},
		{"snapshot_max_entities", limits.SnapshotMaxEntities, 1000, 2000000},
		{"snapshot_max_bytes", limits.SnapshotMaxBytes, 1 << 20, 512 << 20},
		{"sync_transaction_seconds", limits.SyncTransactionSeconds, 1, 30},
		{"import_max_records", limits.ImportMaxRecords, 100, 1000000},
		{"import_max_babies", limits.ImportMaxBabies, 1, 500},
		{"import_chunk_size", limits.ImportChunkSize, 1, 1000},
		{"import_staging_ttl_hours", limits.ImportStagingTTLHours, 1, 720},
		{"import_max_sessions_per_family", limits.ImportMaxSessionsPerFamily, 10, 10000},
		{"import_max_sessions_per_account", limits.ImportMaxSessionsPerAccount, 1, 1000},
		{"active_snapshot_account_limit", limits.ActiveSnapshotAccountLimit, 2, 2},
		{"active_snapshot_family_limit", limits.ActiveSnapshotFamilyLimit, 8, 8},
		{"unfinished_import_account_limit", limits.UnfinishedImportAccountLimit, 1, 1},
		{"unfinished_import_family_limit", limits.UnfinishedImportFamilyLimit, 4, 4},
		{"socket_device_limit", limits.SocketDeviceLimit, 1, 5},
		{"socket_account_limit", limits.SocketAccountLimit, 1, 10},
		{"socket_process_limit", limits.SocketProcessLimit, 10, 10000},
		{"socket_attempt_ip_per_minute", limits.SocketAttemptIPPerMinute, 1, 10000},
		{"socket_attempt_device_per_minute", limits.SocketAttemptDevicePerMinute, 1, 10000},
		{"socket_attempt_account_per_minute", limits.SocketAttemptAccountPerMinute, 1, 10000},
		{"socket_attempt_family_per_minute", limits.SocketAttemptFamilyPerMinute, 1, 10000},
		{"max_capabilities", limits.MaxCapabilities, 16, 16},
		{"max_name_scalars", limits.MaxNameScalars, 100, 100},
		{"max_name_bytes", limits.MaxNameBytes, 400, 400},
		{"max_note_bytes", limits.MaxNoteBytes, 4096, 4096},
		{"max_food_scalars", limits.MaxFoodScalars, 100, 100},
		{"max_food_bytes", limits.MaxFoodBytes, 400, 400},
		{"max_foods", limits.MaxFoods, 50, 50},
		{"max_side_intervals", limits.MaxSideIntervals, 256, 256},
		{"max_side_seconds", limits.MaxSideSeconds, 604800, 604800},
		{"max_side_total_seconds", limits.MaxSideTotalSeconds, 604800, 604800},
		{"max_sort_order", limits.MaxSortOrder, 1000000, 1000000},
		{"sync_mutation_device_per_minute", limits.SyncMutationDevicePerMinute, 1, 100000},
		{"sync_mutation_account_per_minute", limits.SyncMutationAccountPerMinute, 1, 100000},
		{"sync_mutation_family_per_minute", limits.SyncMutationFamilyPerMinute, 1, 100000},
		{"sync_pull_device_per_minute", limits.SyncPullDevicePerMinute, 1, 10000},
		{"sync_pull_account_per_minute", limits.SyncPullAccountPerMinute, 1, 10000},
	}
	for _, check := range checks {
		if check.value < check.minimum || check.value > check.maximum {
			return fmt.Errorf("%s must be from %d through %d", check.name, check.minimum, check.maximum)
		}
	}
	return nil
}

type Outcome struct {
	MutationID            string            `json:"mutation_id"`
	Status                string            `json:"status"`
	OriginalStatus        *string           `json:"original_status"`
	CanonicalEntity       *CanonicalEntity  `json:"canonical_entity"`
	RelatedEntities       []CanonicalEntity `json:"related_entities"`
	Cursor                *Cursor           `json:"cursor"`
	Code                  *string           `json:"code"`
	OverwroteNewerVersion bool              `json:"overwrote_newer_version"`
}

func (outcome *Outcome) UnmarshalJSON(raw []byte) error {
	type wireOutcome Outcome
	var wire wireOutcome
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	if wire.Cursor != nil && wire.Cursor.Validate() != nil {
		return errInvalidRequest
	}
	*outcome = Outcome(wire)
	return nil
}

type JournalChange struct {
	Cursor              Cursor          `json:"cursor"`
	EntityKind          string          `json:"entity_kind"`
	EntityID            string          `json:"entity_id"`
	EntityVersion       uint64          `json:"entity_version"`
	ActorAccountID      string          `json:"actor_account_id"`
	DeviceID            string          `json:"device_id"`
	Operation           string          `json:"operation"`
	AcceptedAt          string          `json:"accepted_at"`
	Optional            bool            `json:"optional"`
	SnapshotOrTombstone CanonicalEntity `json:"snapshot_or_tombstone"`
}

func (change *JournalChange) UnmarshalJSON(raw []byte) error {
	type wireJournalChange JournalChange
	var wire wireJournalChange
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	if wire.Cursor.Validate() != nil || wire.EntityVersion < 1 || wire.EntityVersion > MaxSafeInteger {
		return errInvalidRequest
	}
	*change = JournalChange(wire)
	return nil
}

type SyncRequest struct {
	RequestMeta
	Cursor    *Cursor    `json:"cursor"`
	Mutations []Mutation `json:"mutations"`
	PageToken *string    `json:"page_token"`
}

type SyncResponse struct {
	ServerInstanceID       string          `json:"server_instance_id"`
	RecoveryEpoch          uint64          `json:"recovery_epoch"`
	Outcomes               []Outcome       `json:"outcomes"`
	Changes                []JournalChange `json:"changes"`
	HighWater              Cursor          `json:"high_water"`
	NextPageToken          *string         `json:"next_page_token"`
	SkippedOptionalCursors []Cursor        `json:"-"`
	// EncodedChanges, when non-nil, replaces Changes on the wire: each item
	// is one change already encoded (EncodeChange around a stored entity),
	// written verbatim. It takes precedence: when both are set, Changes is
	// ignored. nil means "not encoded", so an empty encoded page must be a
	// non-nil empty slice (make(..., 0)) to be written as [] rather than
	// fall back to Changes (null when nil, as before).
	EncodedChanges []json.RawMessage `json:"-"`
}

// MarshalJSON writes the v1 sync response with its usual field order. The
// changes array is EncodedChanges when it is non-nil, else Changes. The
// output is always HTML-escaped (json.Marshal), whatever SetEscapeHTML the
// outer encoder uses: an encoder with escaping off still gets \u003c here.
func (response SyncResponse) MarshalJSON() ([]byte, error) {
	if response.EncodedChanges == nil {
		type plain SyncResponse
		return json.Marshal(plain(response))
	}
	return json.Marshal(struct {
		ServerInstanceID string            `json:"server_instance_id"`
		RecoveryEpoch    uint64            `json:"recovery_epoch"`
		Outcomes         []Outcome         `json:"outcomes"`
		Changes          []json.RawMessage `json:"changes"`
		HighWater        Cursor            `json:"high_water"`
		NextPageToken    *string           `json:"next_page_token"`
	}{response.ServerInstanceID, response.RecoveryEpoch, response.Outcomes, response.EncodedChanges, response.HighWater, response.NextPageToken})
}

// EncodeChange encodes one journal change around an entity that is already
// encoded (a stored care wire), byte-identical to json.Marshal of the
// JournalChange holding that entity decoded. Changes are never optional.
func EncodeChange(cursor Cursor, kind, entityID string, version uint64, actorAccountID, deviceID, operation, acceptedAt string, entity json.RawMessage) (json.RawMessage, error) {
	return json.Marshal(struct {
		Cursor              Cursor          `json:"cursor"`
		EntityKind          string          `json:"entity_kind"`
		EntityID            string          `json:"entity_id"`
		EntityVersion       uint64          `json:"entity_version"`
		ActorAccountID      string          `json:"actor_account_id"`
		DeviceID            string          `json:"device_id"`
		Operation           string          `json:"operation"`
		AcceptedAt          string          `json:"accepted_at"`
		Optional            bool            `json:"optional"`
		SnapshotOrTombstone json.RawMessage `json:"snapshot_or_tombstone"`
	}{cursor, kind, entityID, version, actorAccountID, deviceID, operation, acceptedAt, false, entity})
}

// IncrementalPageEncodedBytes measures the exact v1 incremental page envelope
// and changes array while deliberately excluding unsplittable mutation
// outcomes. The HTTP encoder appends one newline.
func IncrementalPageEncodedBytes(server ServerInfo, familyID string, highWater uint64, changes []JournalChange, nextPageToken *string) int {
	wire := struct {
		ServerInstanceID string          `json:"server_instance_id"`
		RecoveryEpoch    uint64          `json:"recovery_epoch"`
		Changes          []JournalChange `json:"changes"`
		HighWater        Cursor          `json:"high_water"`
		NextPageToken    *string         `json:"next_page_token"`
	}{server.InstanceID, server.RecoveryEpoch, changes, Cursor{ServerInstanceID: server.InstanceID, RecoveryEpoch: server.RecoveryEpoch, FamilyID: familyID, Sequence: highWater}, nextPageToken}
	raw, _ := json.Marshal(wire)
	return len(raw) + 1
}

// IncrementalPageEncodedBytesRaw is IncrementalPageEncodedBytes for a page
// of changeCount encoded changes totalling changeBytes: the empty envelope,
// the changes and the commas between them.
func IncrementalPageEncodedBytesRaw(server ServerInfo, familyID string, highWater uint64, changeBytes, changeCount int, nextPageToken *string) int {
	return IncrementalPageEncodedBytes(server, familyID, highWater, []JournalChange{}, nextPageToken) + changeBytes + max(changeCount-1, 0)
}

func (response *SyncResponse) UnmarshalJSON(raw []byte) error {
	var wire struct {
		ServerInstanceID string            `json:"server_instance_id"`
		RecoveryEpoch    uint64            `json:"recovery_epoch"`
		Outcomes         []Outcome         `json:"outcomes"`
		Changes          []json.RawMessage `json:"changes"`
		HighWater        Cursor            `json:"high_water"`
		NextPageToken    *string           `json:"next_page_token"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	changeBytes, err := json.Marshal(wire.Changes)
	if err != nil {
		return err
	}
	changes, skipped, err := DecodeJournalChanges(changeBytes)
	if err != nil {
		return err
	}
	if validateUUID("server_instance_id", wire.ServerInstanceID) != nil || !ValidRecoveryEpoch(wire.RecoveryEpoch) ||
		wire.Outcomes == nil || wire.Changes == nil || wire.HighWater.Validate() != nil {
		return errInvalidRequest
	}
	*response = SyncResponse{
		ServerInstanceID: wire.ServerInstanceID, RecoveryEpoch: wire.RecoveryEpoch,
		Outcomes: wire.Outcomes, Changes: changes, HighWater: wire.HighWater,
		NextPageToken: wire.NextPageToken, SkippedOptionalCursors: skipped,
	}
	return nil
}

type SnapshotRequest struct {
	PageToken *string `json:"page_token"`
}

type SnapshotResponse struct {
	ServerInstanceID string            `json:"server_instance_id"`
	RecoveryEpoch    uint64            `json:"recovery_epoch"`
	SnapshotID       string            `json:"snapshot_id"`
	HighWater        Cursor            `json:"high_water"`
	Entities         []CanonicalEntity `json:"entities"`
	NextPageToken    *string           `json:"next_page_token"`
	// EncodedEntities, when non-nil, replaces Entities on the wire: each item
	// is one stored entity wire, written verbatim. It takes precedence: when
	// both are set, Entities is ignored. nil means "not encoded", so an empty
	// encoded page must be a non-nil empty slice to be written as [].
	EncodedEntities []json.RawMessage `json:"-"`
}

// MarshalJSON writes the v1 snapshot response with its usual field order.
// The entities array is EncodedEntities when it is non-nil, else Entities.
// The output is always HTML-escaped (json.Marshal), whatever SetEscapeHTML
// the outer encoder uses.
func (response SnapshotResponse) MarshalJSON() ([]byte, error) {
	if response.EncodedEntities == nil {
		type plain SnapshotResponse
		return json.Marshal(plain(response))
	}
	return json.Marshal(struct {
		ServerInstanceID string            `json:"server_instance_id"`
		RecoveryEpoch    uint64            `json:"recovery_epoch"`
		SnapshotID       string            `json:"snapshot_id"`
		HighWater        Cursor            `json:"high_water"`
		Entities         []json.RawMessage `json:"entities"`
		NextPageToken    *string           `json:"next_page_token"`
	}{response.ServerInstanceID, response.RecoveryEpoch, response.SnapshotID, response.HighWater, response.EncodedEntities, response.NextPageToken})
}

const (
	MinimumSyncPageBytes   = 64 << 10
	MaximumPageTokenLength = 174
)

// SnapshotPageEncodedBytes measures the complete v1 snapshot response as it is
// written by the HTTP JSON encoder, including its trailing newline.
func SnapshotPageEncodedBytes(server ServerInfo, snapshotID string, highWater Cursor, entities []CanonicalEntity, nextPageToken *string) int {
	response := SnapshotResponse{ServerInstanceID: server.InstanceID, RecoveryEpoch: server.RecoveryEpoch, SnapshotID: snapshotID,
		HighWater: highWater, Entities: entities, NextPageToken: nextPageToken}
	raw, _ := json.Marshal(response)
	return len(raw) + 1
}

// SnapshotPageEncodedBytesRaw is SnapshotPageEncodedBytes for a page of
// entityCount stored entity wires totalling entityBytes.
func SnapshotPageEncodedBytesRaw(server ServerInfo, snapshotID string, highWater Cursor, entityBytes, entityCount int, nextPageToken *string) int {
	return SnapshotPageEncodedBytes(server, snapshotID, highWater, []CanonicalEntity{}, nextPageToken) + entityBytes + max(entityCount-1, 0)
}

func (response *SnapshotResponse) UnmarshalJSON(raw []byte) error {
	type wireSnapshotResponse SnapshotResponse
	var wire wireSnapshotResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	if validateUUID("server_instance_id", wire.ServerInstanceID) != nil || !ValidRecoveryEpoch(wire.RecoveryEpoch) ||
		validateUUID("snapshot_id", wire.SnapshotID) != nil || wire.HighWater.Validate() != nil || wire.Entities == nil {
		return errInvalidRequest
	}
	*response = SnapshotResponse(wire)
	return nil
}

func DecodeJournalChanges(raw []byte) ([]JournalChange, []Cursor, error) {
	var envelopes []json.RawMessage
	if err := json.Unmarshal(raw, &envelopes); err != nil || envelopes == nil {
		return nil, nil, errInvalidRequest
	}
	changes := make([]JournalChange, 0, len(envelopes))
	skipped := make([]Cursor, 0)
	for _, envelope := range envelopes {
		var header struct {
			Cursor              Cursor `json:"cursor"`
			EntityKind          string `json:"entity_kind"`
			Optional            bool   `json:"optional"`
			SnapshotOrTombstone struct {
				EntityKind string `json:"entity_kind"`
			} `json:"snapshot_or_tombstone"`
		}
		if err := json.Unmarshal(envelope, &header); err != nil {
			return nil, nil, errInvalidRequest
		}
		if err := header.Cursor.Validate(); err != nil {
			return nil, nil, errInvalidRequest
		}
		knownOuter := slices.Contains([]string{"baby", "feed", "sleep", "diaper", "supplement", "growth_profile", "measurement", "timer"}, header.EntityKind)
		knownInner := slices.Contains([]string{"baby", "feed", "sleep", "diaper", "supplement", "growth_profile", "measurement", "timer"}, header.SnapshotOrTombstone.EntityKind)
		if !knownOuter || !knownInner || header.EntityKind != header.SnapshotOrTombstone.EntityKind {
			if header.Optional {
				skipped = append(skipped, header.Cursor)
				continue
			}
			kind := header.EntityKind
			if !knownInner {
				kind = header.SnapshotOrTombstone.EntityKind
			}
			return nil, nil, UnsupportedResourceError{Kind: kind, Cursor: header.Cursor}
		}
		var change JournalChange
		if err := json.Unmarshal(envelope, &change); err != nil {
			return nil, nil, err
		}
		changes = append(changes, change)
	}
	return changes, skipped, nil
}

type UnsupportedResourceError struct {
	Kind   string
	Cursor Cursor
}

func (err UnsupportedResourceError) Error() string {
	return fmt.Sprintf("unsupported resource %q", err.Kind)
}
func (err UnsupportedResourceError) Unwrap() error { return ErrUnsupportedResource }
