package protocol

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
)

// Direct encoders for the pull's hot path. encoding/json compacts (and
// HTML-escapes) every json.RawMessage and every MarshalJSON result it
// writes, so a stored wire was scanned once in EncodeChange, once more as
// an EncodedChanges item and again as part of SyncResponse.MarshalJSON's
// output in the HTTP encoder. These encoders append the stored bytes
// verbatim instead and write the envelopes field by field, byte-identical
// to the json.Marshal / json.Encoder output (encoded_test.go compares them
// over random pages).
//
// A stored wire is json.Marshal output (care.Encode): compact, valid and
// HTML-escaped, so encoding/json's compaction leaves it unchanged. That is
// what makes appending it verbatim byte-identical, and the encoders rely
// on it for a value shaped like a stored entity (a JSON object: '{' … '}').
// Anything else (nil, empty, a scalar, an array) takes the json.Marshal
// path, which keeps its exact output and its errors.

// trustedObject reports whether raw is shaped like a stored wire (an
// object), which the direct encoders append verbatim.
func trustedObject(raw []byte) bool {
	return len(raw) >= 2 && raw[0] == '{' && raw[len(raw)-1] == '}'
}

// appendString appends s as encoding/json writes a string with HTML
// escaping (json.Marshal): printable ASCII without '"', '\\', '<', '>' and
// '&' is written as is, anything else through json.Marshal.
func appendString(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			raw, _ := json.Marshal(s) // a string always encodes
			return append(dst, raw...)
		}
	}
	dst = append(dst, '"')
	dst = append(dst, s...)
	return append(dst, '"')
}

// appendCursor appends c as json.Marshal writes a Cursor.
func appendCursor(dst []byte, c Cursor) []byte {
	dst = append(dst, `{"server_instance_id":`...)
	dst = appendString(dst, c.ServerInstanceID)
	dst = append(dst, `,"recovery_epoch":`...)
	dst = strconv.AppendUint(dst, c.RecoveryEpoch, 10)
	dst = append(dst, `,"family_id":`...)
	dst = appendString(dst, c.FamilyID)
	dst = append(dst, `,"sequence":`...)
	dst = strconv.AppendUint(dst, c.Sequence, 10)
	return append(dst, '}')
}

// AppendChange appends EncodeChange's encoding of one journal change to
// dst: the envelope written field by field and entity, a stored wire,
// appended verbatim. An entity that is not an object is encoded through
// json.Marshal as before (its output and errors unchanged).
func AppendChange(dst []byte, cursor Cursor, kind, entityID string, version uint64, actorAccountID, deviceID, operation, acceptedAt string, entity json.RawMessage) ([]byte, error) {
	if !trustedObject(entity) {
		raw, err := marshalChange(cursor, kind, entityID, version, actorAccountID, deviceID, operation, acceptedAt, entity)
		if err != nil {
			return dst, err
		}
		return append(dst, raw...), nil
	}
	dst = append(dst, `{"cursor":`...)
	dst = appendCursor(dst, cursor)
	dst = append(dst, `,"entity_kind":`...)
	dst = appendString(dst, kind)
	dst = append(dst, `,"entity_id":`...)
	dst = appendString(dst, entityID)
	dst = append(dst, `,"entity_version":`...)
	dst = strconv.AppendUint(dst, version, 10)
	dst = append(dst, `,"actor_account_id":`...)
	dst = appendString(dst, actorAccountID)
	dst = append(dst, `,"device_id":`...)
	dst = appendString(dst, deviceID)
	dst = append(dst, `,"operation":`...)
	dst = appendString(dst, operation)
	dst = append(dst, `,"accepted_at":`...)
	dst = appendString(dst, acceptedAt)
	dst = append(dst, `,"optional":false,"snapshot_or_tombstone":`...)
	dst = append(dst, entity...)
	return append(dst, '}'), nil
}

// changeEnvelopeBytes bounds a change's envelope around its entity, for
// EncodeChange's one allocation (UUIDs, kind, operation, timestamp and
// three integers of up to 20 digits, unescaped: about 560 bytes).
const changeEnvelopeBytes = 640

// AppendJSON appends response as the HTTP layer writes it
// (json.NewEncoder(w).Encode(response) without the trailing newline). With
// EncodedChanges set, the envelope is written field by field and each
// encoded change (EncodeChange output) appended verbatim; Outcomes, which
// a write carries, go through json.Marshal. Without EncodedChanges, or with
// an encoded change that is not an object, it is json.Marshal(response).
func (response SyncResponse) AppendJSON(dst []byte) ([]byte, error) {
	var outcomes []byte
	var err error
	switch {
	case response.EncodedChanges == nil || !trustedObjects(response.EncodedChanges):
		err = errMarshalResponse
	case response.Outcomes != nil && len(response.Outcomes) == 0:
		outcomes = []byte("[]")
	default:
		outcomes, err = json.Marshal(response.Outcomes)
	}
	if err != nil {
		raw, err := json.Marshal(response)
		if err != nil {
			return dst, err
		}
		return append(dst, raw...), nil
	}
	dst = slices.Grow(dst, response.encodedBytes()+len(outcomes))
	dst = append(dst, `{"server_instance_id":`...)
	dst = appendString(dst, response.ServerInstanceID)
	dst = append(dst, `,"recovery_epoch":`...)
	dst = strconv.AppendUint(dst, response.RecoveryEpoch, 10)
	dst = append(dst, `,"outcomes":`...)
	dst = append(dst, outcomes...)
	dst = append(dst, `,"changes":[`...)
	for i, change := range response.EncodedChanges {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, change...)
	}
	dst = append(dst, `],"high_water":`...)
	dst = appendCursor(dst, response.HighWater)
	dst = append(dst, `,"next_page_token":`...)
	if response.NextPageToken == nil {
		dst = append(dst, "null"...)
	} else {
		dst = appendString(dst, *response.NextPageToken)
	}
	return append(dst, '}'), nil
}

// errMarshalResponse sends AppendJSON down the json.Marshal path.
var errMarshalResponse = errors.New("protocol: response encoded through json.Marshal")

// encodedBytes bounds AppendJSON's direct output without its outcomes.
func (response SyncResponse) encodedBytes() int {
	n := 512 + len(response.ServerInstanceID) + len(response.EncodedChanges)
	if response.NextPageToken != nil {
		n += len(*response.NextPageToken)
	}
	for _, change := range response.EncodedChanges {
		n += len(change)
	}
	return n
}

func trustedObjects(items []json.RawMessage) bool {
	for _, item := range items {
		if !trustedObject(item) {
			return false
		}
	}
	return true
}
