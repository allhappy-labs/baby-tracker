package protocol

import (
	"bytes"
	"encoding/json"
)

func requireImportFields(raw []byte, fields ...string) error {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return errInvalidRequest
	}
	for _, field := range fields {
		value, ok := obj[field]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errInvalidRequest
		}
	}
	return nil
}

func (r *ImportResolveRequest) UnmarshalJSON(raw []byte) error {
	if err := requireImportFields(raw, "protocol_version", "capabilities", "import_id", "import_mode", "normalization_version", "babies", "records", "total_record_count", "chunk_index", "finalize"); err != nil {
		return err
	}
	type wire ImportResolveRequest
	var value wire
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*r = ImportResolveRequest(value)
	return nil
}
func (r *ImportConflictRequest) UnmarshalJSON(raw []byte) error {
	if err := requireImportFields(raw, "protocol_version", "capabilities", "command_id", "resolver_revision", "conflict_id", "choice"); err != nil {
		return err
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return errInvalidRequest
	}
	if _, ok := obj["canonical_baby_id"]; !ok {
		return errInvalidRequest
	}
	type wire ImportConflictRequest
	var value wire
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*r = ImportConflictRequest(value)
	return nil
}

type ImportBaby struct {
	LocalBabyID string `json:"local_baby_id"`
	Name        string `json:"name"`
	BirthDate   string `json:"birth_date"`
	SortOrder   uint32 `json:"sort_order"`
}

func (b *ImportBaby) UnmarshalJSON(raw []byte) error {
	if err := requireImportFields(raw, "local_baby_id", "name", "birth_date", "sort_order"); err != nil {
		return err
	}
	type wire ImportBaby
	var value wire
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*b = ImportBaby(value)
	return nil
}

type RecordManifest struct {
	RecordID      string `json:"record_id"`
	EntityKind    string `json:"entity_kind"`
	LocalBabyID   string `json:"local_baby_id"`
	ContentSHA256 string `json:"content_sha256"`
}

func (m *RecordManifest) UnmarshalJSON(raw []byte) error {
	if err := requireImportFields(raw, "record_id", "entity_kind", "local_baby_id", "content_sha256"); err != nil {
		return err
	}
	type wire RecordManifest
	var value wire
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*m = RecordManifest(value)
	return nil
}

type ImportResolveRequest struct {
	RequestMeta
	ImportID             string           `json:"import_id"`
	ImportMode           string           `json:"import_mode"`
	NormalizationVersion string           `json:"normalization_version"`
	Babies               []ImportBaby     `json:"babies"`
	Records              []RecordManifest `json:"records"`
	TotalRecordCount     uint32           `json:"total_record_count"`
	ChunkIndex           uint32           `json:"chunk_index"`
	Finalize             bool             `json:"finalize"`
}

type BabyMapping struct {
	LocalBabyID  string `json:"local_baby_id"`
	SharedBabyID string `json:"shared_baby_id"`
}

type ImportConflict struct {
	ConflictID        string   `json:"conflict_id"`
	Kind              string   `json:"kind"`
	LocalBabyID       string   `json:"local_baby_id"`
	RecordID          *string  `json:"record_id"`
	CandidateBabyIDs  []string `json:"candidate_baby_ids"`
	CandidateVersions []uint64 `json:"candidate_versions"`
}

func (conflict *ImportConflict) UnmarshalJSON(raw []byte) error {
	type wireImportConflict ImportConflict
	var wire wireImportConflict
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	for _, version := range wire.CandidateVersions {
		if version < 1 || version > MaxSafeInteger {
			return errInvalidRequest
		}
	}
	*conflict = ImportConflict(wire)
	return nil
}

type ImportResolveResponse struct {
	ImportID              string           `json:"import_id"`
	ResolverRevision      uint64           `json:"resolver_revision"`
	NormalizationVersion  string           `json:"normalization_version"`
	Mappings              []BabyMapping    `json:"mappings"`
	Conflicts             []ImportConflict `json:"conflicts"`
	ConflictCount         uint64           `json:"conflict_count"`
	ConflictsComplete     bool             `json:"conflicts_complete"`
	NextConflictPageToken *string          `json:"next_conflict_page_token"`
	MappingComplete       bool             `json:"mapping_complete"`
	NextChunkIndex        uint32           `json:"next_chunk_index"`
	ManifestComplete      bool             `json:"manifest_complete"`
}

func (response *ImportResolveResponse) UnmarshalJSON(raw []byte) error {
	type wireImportResolveResponse ImportResolveResponse
	var wire wireImportResolveResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	if wire.ResolverRevision < 1 || wire.ResolverRevision > MaxSafeInteger ||
		wire.ConflictCount > MaxSafeInteger ||
		(wire.NextConflictPageToken != nil && len(*wire.NextConflictPageToken) > 512) {
		return errInvalidRequest
	}
	*response = ImportResolveResponse(wire)
	return nil
}

type ImportConflictRequest struct {
	RequestMeta
	CommandID        string  `json:"command_id"`
	ResolverRevision uint64  `json:"resolver_revision"`
	ConflictID       string  `json:"conflict_id"`
	Choice           string  `json:"choice"`
	CanonicalBabyID  *string `json:"canonical_baby_id"`
}
