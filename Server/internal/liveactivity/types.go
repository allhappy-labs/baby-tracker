// Package liveactivity owns ActivityKit payloads separately from registration
// wire DTOs, because ActivityKit requires the Swift Codable camelCase keys.
package liveactivity

import (
	"encoding/json"
	"errors"
	"math"

	"babytracker/server/internal/protocol"
)

const FoundationReferenceUnixSeconds = 978307200

// FoundationDate converts Unix seconds to JSONEncoder's default Date encoding.
// APNs aps.timestamp and Revision.PresentationTimestamp remain Unix seconds.
func FoundationDate(unixSeconds float64) float64 { return unixSeconds - FoundationReferenceUnixSeconds }

type ActivityScope struct {
	ServerInstanceID string `json:"serverInstanceID"`
	RecoveryEpoch    uint64 `json:"recoveryEpoch"`
	FamilyID         string `json:"familyID"`
	BabyID           string `json:"babyID"`
	KindRaw          string `json:"kindRaw"`
}

type Revision struct {
	Scope                 ActivityScope `json:"scope"`
	GenerationID          string        `json:"generationID"`
	TimerID               *string       `json:"timerID,omitempty"`
	SessionID             *string       `json:"sessionID,omitempty"`
	FamilySequence        uint64        `json:"familySequence"`
	PresentationVersion   uint64        `json:"presentationVersion"`
	PresentationTimestamp uint64        `json:"presentationTimestamp"`
	CanWrite              bool          `json:"canWrite"`
}

func NewRevision(value protocol.LiveActivityRevision) Revision {
	return Revision{Scope: ActivityScope(value.Scope), GenerationID: value.GenerationID, TimerID: value.TimerID,
		SessionID: value.SessionID, FamilySequence: value.FamilySequence, PresentationVersion: value.PresentationVersion,
		PresentationTimestamp: value.PresentationTimestamp, CanWrite: value.CanWrite}
}

func (revision Revision) Protocol() protocol.LiveActivityRevision {
	return protocol.LiveActivityRevision{Scope: protocol.LiveActivityScope(revision.Scope), GenerationID: revision.GenerationID,
		TimerID: revision.TimerID, SessionID: revision.SessionID, FamilySequence: revision.FamilySequence,
		PresentationVersion: revision.PresentationVersion, PresentationTimestamp: revision.PresentationTimestamp, CanWrite: revision.CanWrite}
}

type PartLabel struct {
	Side    *string `json:"side,omitempty"`
	Seconds uint64  `json:"seconds"`
}

// ContentState mirrors TimerActivityContentState. Date fields are Foundation
// reference seconds, not RFC3339 strings or APNs Unix timestamps.
type ContentState struct {
	StartedAt     float64     `json:"startedAt"`
	SideLabel     string      `json:"sideLabel"`
	ActivityKind  string      `json:"activityKind"`
	Phase         string      `json:"phase"`
	BreakEndsAt   *float64    `json:"breakEndsAt,omitempty"`
	Parts         []PartLabel `json:"parts"`
	SuggestedSide *string     `json:"suggestedSide,omitempty"`
	OmittedParts  uint64      `json:"omittedParts"`
	Remote        *Revision   `json:"remote,omitempty"`
}

func (state *ContentState) UnmarshalJSON(raw []byte) error {
	type wire ContentState
	decoded := wire{Phase: "running", Parts: []PartLabel{}}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	*state = ContentState(decoded)
	return nil
}

func (state ContentState) Validate() error {
	invalid := errors.New("invalid live activity content")
	if math.IsNaN(state.StartedAt) || math.IsInf(state.StartedAt, 0) || state.OmittedParts > protocol.MaxSafeInteger {
		return invalid
	}
	if state.BreakEndsAt != nil && (math.IsNaN(*state.BreakEndsAt) || math.IsInf(*state.BreakEndsAt, 0)) {
		return invalid
	}
	switch state.ActivityKind {
	case "breastLeft", "breastRight", "breastMilkBottle", "formulaBottle", "mixedBottle", "timedBottle", "paused", "sleep":
	default:
		return invalid
	}
	switch state.Phase {
	case "running", "feedBreak", "sleepBreak":
	default:
		return invalid
	}
	if state.SuggestedSide != nil && *state.SuggestedSide != "left" && *state.SuggestedSide != "right" {
		return invalid
	}
	for _, part := range state.Parts {
		if part.Seconds > protocol.MaxSafeInteger || (part.Side != nil && *part.Side != "left" && *part.Side != "right") {
			return invalid
		}
	}
	if state.Remote != nil {
		return state.Remote.Protocol().Validate()
	}
	return nil
}
