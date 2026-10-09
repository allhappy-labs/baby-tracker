package export

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"

	"babytracker/server/internal/protocol"
)

const MaxArchivePlaintextBytes = 52_428_800

var ErrArchiveTooLarge = errors.New("export_too_large")

type ArchiveMember struct {
	AccountID   string `json:"account_id"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Status      string `json:"status"`
}

type ArchiveAudit struct {
	Revision       uint64 `json:"revision"`
	Kind           string `json:"kind"`
	ActorAccountID string `json:"actor_account_id"`
	At             string `json:"at"`
}

type AttachmentManifest struct {
	Version     string                        `json:"version"`
	Attachments []protocol.AttachmentMetadata `json:"attachments"`
}

// ArchiveV1 is the stable, ordered portable family data format. All entity
// arrays include tombstones; callers capture them in one authorized snapshot.
type ArchiveV1 struct {
	Version          string                     `json:"version"`
	ServerInstanceID string                     `json:"server_instance_id"`
	RecoveryEpoch    uint64                     `json:"recovery_epoch"`
	FamilyID         string                     `json:"family_id"`
	CapturedAt       string                     `json:"captured_at"`
	HighWater        protocol.Cursor            `json:"high_water"`
	SecurityRevision uint64                     `json:"security_revision"`
	Members          []ArchiveMember            `json:"members"`
	Babies           []protocol.CanonicalEntity `json:"babies"`
	Feeds            []protocol.CanonicalEntity `json:"feeds"`
	Sleeps           []protocol.CanonicalEntity `json:"sleeps"`
	Diapers          []protocol.CanonicalEntity `json:"diapers"`
	Supplements      []protocol.CanonicalEntity `json:"supplements"`
	GrowthProfiles   []protocol.CanonicalEntity `json:"growth_profiles"`
	Measurements     []protocol.CanonicalEntity `json:"measurements"`
	ActiveTimers     []protocol.CanonicalEntity `json:"active_timers"`
	Journal          []protocol.JournalChange   `json:"journal"`
	Audit            []ArchiveAudit             `json:"audit"`
	Attachments      AttachmentManifest         `json:"attachments"`
}

type boundedArchiveWriter struct {
	out io.Writer
	n   uint64
}

func (b *boundedArchiveWriter) Write(p []byte) (int, error) {
	if uint64(len(p)) > MaxArchivePlaintextBytes-b.n {
		return 0, ErrArchiveTooLarge
	}
	n, err := b.out.Write(p)
	b.n += uint64(n)
	if err == nil && n != len(p) {
		return n, io.ErrShortWrite
	}
	return n, err
}

// EncodeArchive writes compact JSON in stable field order and rejects any
// archive over 50 MiB. It does not add a newline.
func EncodeArchive(ctx context.Context, out io.Writer, a ArchiveV1) (uint64, error) {
	b := &boundedArchiveWriter{out: out}
	write := func(v any) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = b.Write(data)
		return err
	}
	raw := func(s string) error { _, err := b.Write([]byte(s)); return err }
	field := func(name string, v any) error {
		if err := raw(`,"` + name + `":`); err != nil {
			return err
		}
		return write(v)
	}
	if a.Version == "" {
		a.Version = "v1"
	}
	if a.Attachments.Version == "" {
		a.Attachments.Version = "v1"
	}
	a.Attachments.Attachments = []protocol.AttachmentMetadata{}
	if err := raw(`{"version":`); err != nil {
		return b.n, err
	}
	if err := write(a.Version); err != nil {
		return b.n, err
	}
	for _, f := range []struct {
		name  string
		value any
	}{
		{"server_instance_id", a.ServerInstanceID}, {"recovery_epoch", a.RecoveryEpoch}, {"family_id", a.FamilyID},
		{"captured_at", a.CapturedAt}, {"high_water", a.HighWater}, {"security_revision", a.SecurityRevision},
	} {
		if err := field(f.name, f.value); err != nil {
			return b.n, err
		}
	}
	members := append([]ArchiveMember{}, a.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].AccountID < members[j].AccountID })
	if err := field("members", members); err != nil {
		return b.n, err
	}
	for _, group := range []struct {
		name     string
		entities []protocol.CanonicalEntity
	}{
		{"babies", a.Babies}, {"feeds", a.Feeds}, {"sleeps", a.Sleeps}, {"diapers", a.Diapers},
		{"supplements", a.Supplements}, {"growth_profiles", a.GrowthProfiles}, {"measurements", a.Measurements}, {"active_timers", a.ActiveTimers},
	} {
		entities := append([]protocol.CanonicalEntity{}, group.entities...)
		sort.Slice(entities, func(i, j int) bool { return entities[i].Meta.ID < entities[j].Meta.ID })
		if err := field(group.name, entities); err != nil {
			return b.n, err
		}
	}
	journal := append([]protocol.JournalChange{}, a.Journal...)
	sort.Slice(journal, func(i, j int) bool { return journal[i].Cursor.Sequence < journal[j].Cursor.Sequence })
	if err := field("journal", journal); err != nil {
		return b.n, err
	}
	audit := append([]ArchiveAudit{}, a.Audit...)
	sort.Slice(audit, func(i, j int) bool { return audit[i].Revision < audit[j].Revision })
	if err := field("audit", audit); err != nil {
		return b.n, err
	}
	if err := field("attachments", a.Attachments); err != nil {
		return b.n, err
	}
	if err := raw("}"); err != nil {
		return b.n, err
	}
	return b.n, nil
}
