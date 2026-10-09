package protocol

import (
	"bytes"
	"encoding/json"
	"reflect"
)

type Hint struct {
	FamilyID string `json:"family_id"`
	Cursor   Cursor `json:"cursor"`
}

func (hint *Hint) UnmarshalJSON(raw []byte) error {
	var wire struct {
		FamilyID string `json:"family_id"`
		Cursor   struct {
			ServerInstanceID string `json:"server_instance_id"`
			RecoveryEpoch    uint64 `json:"recovery_epoch"`
			FamilyID         string `json:"family_id"`
			Sequence         uint64 `json:"sequence"`
		} `json:"cursor"`
	}
	if len(raw) == 0 || rejectDuplicateRequestKeys(raw) != nil || validateExactJSONObject(raw, reflect.TypeOf(wire)) != nil {
		return errInvalidRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	cursor := Cursor{ServerInstanceID: wire.Cursor.ServerInstanceID, RecoveryEpoch: wire.Cursor.RecoveryEpoch,
		FamilyID: wire.Cursor.FamilyID, Sequence: wire.Cursor.Sequence}
	if validateUUID("family_id", wire.FamilyID) != nil || cursor.Validate() != nil || cursor.FamilyID != wire.FamilyID {
		return errInvalidRequest
	}
	*hint = Hint{FamilyID: wire.FamilyID, Cursor: cursor}
	return nil
}
