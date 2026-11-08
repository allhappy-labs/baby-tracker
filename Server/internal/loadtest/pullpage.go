package loadtest

import (
	"bytes"
	"encoding/json"
	"errors"

	"babytracker/server/internal/protocol"
)

// pullPage is a pull answer as an untracked device reads it: what the
// generator acts on (the high water, the next page token) and what the
// family check reads (every change's cursor and entity family), with the
// protocol's structural rules for a page (protocol.SyncResponse's
// UnmarshalJSON): valid server identity and high water, outcomes and changes
// present, each change's cursor valid and its entity version in range, the
// change's and its entity's kinds known and equal (an optional change of an
// unknown kind is skipped, as the protocol skips it), the entity's meta
// present with its version in range, and a payload that is an object, or
// null only for a tombstone.
//
// It does not decode the payloads: the protocol's decoder holds each one to
// its kind's shape, which costs about a hundred times this check and which
// no figure of an untracked device reads. A tracked device, whose map the
// audit compares with the database, decodes its pages in full
// (protocol.SyncResponse), as before.
//
// A skipped change is held to what the protocol reads of it (its cursor,
// its kinds and optional), no more: its entity_version and its entity's
// meta are read lazily (lazyVersion, lazyMeta) and checked only for a change
// that is applied, as the full decode checks them.
//
// Duplicate keys are the one input classed differently: encoding/json
// takes the last of a duplicated key and the protocol's nested RawMessage
// envelopes do too, while a lazy field seen twice keeps a failure from the
// first. The server never writes a duplicate key.
type pullPage struct {
	ServerInstanceID string             `json:"server_instance_id"`
	RecoveryEpoch    uint64             `json:"recovery_epoch"`
	Outcomes         []protocol.Outcome `json:"outcomes"`
	Changes          []pageChange       `json:"changes"`
	HighWater        protocol.Cursor    `json:"high_water"`
	NextPageToken    *string            `json:"next_page_token"`
}

type pageChange struct {
	Cursor              protocol.Cursor `json:"cursor"`
	EntityKind          entityKind      `json:"entity_kind"`
	EntityVersion       lazyVersion     `json:"entity_version"`
	Optional            bool            `json:"optional"`
	SnapshotOrTombstone pageEntity      `json:"snapshot_or_tombstone"`

	skipped bool // optional, of a kind the protocol does not know: not applied
}

type pageEntity struct {
	EntityKind entityKind   `json:"entity_kind"`
	Meta       lazyMeta     `json:"meta"`
	Payload    jsonPresence `json:"payload"`
}

type pageMeta struct {
	FamilyID      string  `json:"family_id"`
	EntityVersion uint64  `json:"entity_version"`
	TombstonedAt  *string `json:"tombstoned_at"`
}

// lazyVersion is an entity_version read without failing the page: a value
// that is not an unsigned integer marks it bad, for valid to judge when the
// change is applied.
type lazyVersion struct {
	v   uint64
	bad bool
}

func (v *lazyVersion) UnmarshalJSON(raw []byte) error {
	if json.Unmarshal(raw, &v.v) != nil {
		v.bad = true
	}
	return nil
}

func (v lazyVersion) valid() bool { return !v.bad && validVersion(v.v) }

// lazyMeta is an entity's meta read without failing the page: absent, null,
// a value that is not an object, or an object whose read fields have the
// wrong type, all leave it not ok, for valid to judge when the entity is
// applied (the protocol refuses each of them there).
type lazyMeta struct {
	state jsonPresence
	bad   bool
	pageMeta
}

func (m *lazyMeta) UnmarshalJSON(raw []byte) error {
	_ = m.state.UnmarshalJSON(raw)
	if m.state == jsonObject && json.Unmarshal(raw, &m.pageMeta) != nil {
		m.bad = true
	}
	return nil
}

func (m *lazyMeta) ok() bool { return m.state == jsonObject && !m.bad }

// entityKinds are the protocol's entity kinds (protocol.knownEntityKind);
// an entityKind is its index here plus one, 0 for any other string.
var entityKinds = []string{"baby", "feed", "sleep", "diaper", "supplement", "growth_profile", "measurement", "immunization", "pump", "timer"}

// entityKind is an entity_kind read without keeping the string.
type entityKind uint8

var errNotString = errors.New("loadtest: entity_kind is not a string")

func (k *entityKind) UnmarshalJSON(raw []byte) error {
	if len(raw) < 2 || raw[0] != '"' {
		if bytes.Equal(raw, []byte("null")) {
			*k = 0 // null leaves the protocol's string empty: unknown
			return nil
		}
		return errNotString
	}
	name := raw[1 : len(raw)-1]
	if bytes.IndexByte(name, '\\') >= 0 {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		name = []byte(s)
	}
	*k = 0
	for i, kind := range entityKinds {
		if string(name) == kind {
			*k = entityKind(i + 1)
			break
		}
	}
	return nil
}

func (k entityKind) known() bool { return k != 0 }

// jsonPresence records what a value was without keeping it: absent, null,
// an object or anything else.
type jsonPresence uint8

const (
	jsonAbsent jsonPresence = iota
	jsonNull
	jsonObject
	jsonOther
)

func (p *jsonPresence) UnmarshalJSON(raw []byte) error {
	switch {
	case bytes.Equal(raw, []byte("null")):
		*p = jsonNull
	case len(raw) > 0 && raw[0] == '{':
		*p = jsonObject
	default:
		*p = jsonOther
	}
	return nil
}

// uuidMemo validates UUIDs as protocol.ValidUUID does, remembering the last
// two valid ones: a page repeats the same server and family on every change.
type uuidMemo struct{ a, b string }

func (m *uuidMemo) valid(s string) bool {
	if s != "" && (s == m.a || s == m.b) {
		return true
	}
	if !protocol.ValidUUID(s) {
		return false
	}
	m.a, m.b = s, m.a
	return true
}

func (m *uuidMemo) cursor(c protocol.Cursor) bool {
	return m.valid(c.ServerInstanceID) && protocol.ValidRecoveryEpoch(c.RecoveryEpoch) && c.Sequence <= protocol.MaxSafeInteger &&
		m.valid(c.FamilyID)
}

func validVersion(v uint64) bool { return v >= 1 && v <= protocol.MaxSafeInteger }

// valid applies the protocol's structural rules for a sync answer (see
// pullPage). It marks the optional changes of an unknown kind skipped.
func (p *pullPage) valid() bool {
	var ids uuidMemo
	if !ids.valid(p.ServerInstanceID) || !protocol.ValidRecoveryEpoch(p.RecoveryEpoch) || p.Outcomes == nil || p.Changes == nil ||
		!ids.cursor(p.HighWater) {
		return false
	}
	for i := range p.Changes {
		c := &p.Changes[i]
		if !ids.cursor(c.Cursor) {
			return false
		}
		inner := c.SnapshotOrTombstone.EntityKind
		if !c.EntityKind.known() || !inner.known() || c.EntityKind != inner {
			if !c.Optional {
				return false
			}
			c.skipped = true
			continue
		}
		if !c.EntityVersion.valid() || !c.SnapshotOrTombstone.valid() {
			return false
		}
	}
	return true
}

// valid applies protocol.CanonicalEntity's rules but the payload's shape: a
// known kind, the meta present with its version in range, and a payload
// that is an object, or null only for a tombstone.
func (e *pageEntity) valid() bool {
	if !e.EntityKind.known() || !e.Meta.ok() || !validVersion(e.Meta.EntityVersion) {
		return false
	}
	switch e.Payload {
	case jsonObject:
		return true
	case jsonNull:
		return e.Meta.TombstonedAt != nil
	}
	return false
}

// ownFamily is generator.ownFamily for a light page: the high water, every
// change (but a skipped one) and every outcome entity belong to familyID.
func (p *pullPage) ownFamily(familyID string) bool {
	if p.HighWater.FamilyID != familyID {
		return false
	}
	for i := range p.Changes {
		c := &p.Changes[i]
		if c.skipped {
			continue
		}
		if c.SnapshotOrTombstone.Meta.FamilyID != familyID || c.Cursor.FamilyID != familyID {
			return false
		}
	}
	for _, outcome := range p.Outcomes {
		if outcome.CanonicalEntity != nil && outcome.CanonicalEntity.Meta.FamilyID != familyID {
			return false
		}
		for _, related := range outcome.RelatedEntities {
			if related.Meta.FamilyID != familyID {
				return false
			}
		}
	}
	return true
}

// snapshotPage is a snapshot page as an untracked device reads it, as
// pullPage reads a pull page: the high water, the next page token and every
// entity's family, with protocol.SnapshotResponse's structural rules (valid
// server, snapshot and high water identities, entities present, each one
// valid as pageEntity.valid says) and without decoding the payloads.
type snapshotPage struct {
	ServerInstanceID string          `json:"server_instance_id"`
	RecoveryEpoch    uint64          `json:"recovery_epoch"`
	SnapshotID       string          `json:"snapshot_id"`
	HighWater        protocol.Cursor `json:"high_water"`
	Entities         []pageEntity    `json:"entities"`
	NextPageToken    *string         `json:"next_page_token"`
}

func (p *snapshotPage) valid() bool {
	var ids uuidMemo
	if !ids.valid(p.ServerInstanceID) || !protocol.ValidRecoveryEpoch(p.RecoveryEpoch) || !protocol.ValidUUID(p.SnapshotID) ||
		!ids.cursor(p.HighWater) || p.Entities == nil {
		return false
	}
	for i := range p.Entities {
		if !p.Entities[i].valid() {
			return false
		}
	}
	return true
}

// ownFamily is snapshotOwnFamily for a light page.
func (p *snapshotPage) ownFamily(familyID string) bool {
	if p.HighWater.FamilyID != familyID {
		return false
	}
	for i := range p.Entities {
		if p.Entities[i].Meta.FamilyID != familyID {
			return false
		}
	}
	return true
}
