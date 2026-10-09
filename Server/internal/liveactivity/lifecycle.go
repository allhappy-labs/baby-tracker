package liveactivity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// TargetState is durable delivery metadata. Raw tokens and care payloads never
// enter it. The snapshot refreshes ActivityID/TokenVersion and SuppressStart.
type TargetState struct {
	StartAttemptID      string     `json:"start_attempt_id,omitempty"`
	GenerationID        string     `json:"generation_id,omitempty"`
	Status              string     `json:"status,omitempty"`
	TimerID             *string    `json:"timer_id,omitempty"`
	SessionID           *string    `json:"session_id,omitempty"`
	Kind                string     `json:"kind,omitempty"`
	ActivityID          string     `json:"activity_id,omitempty"`
	TokenVersion        uint64     `json:"token_version,omitempty"`
	KnownActivityIDs    []string   `json:"known_activity_ids,omitempty"`
	DismissedGeneration string     `json:"dismissed_generation,omitempty"`
	LastDigest          string     `json:"last_digest,omitempty"`
	LastTimestamp       int64      `json:"last_timestamp,omitempty"`
	LastSequence        uint64     `json:"last_sequence,omitempty"`
	PresentationVersion uint64     `json:"presentation_version,omitempty"`
	PendingEvent        string     `json:"pending_event,omitempty"`
	AttemptID           string     `json:"attempt_id,omitempty"`
	Attempts            int        `json:"attempts,omitempty"`
	RetryAt             *time.Time `json:"retry_at,omitempty"`
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`
	SuppressStart       bool       `json:"suppress_start,omitempty"`
	AbsenceRetryAllowed bool       `json:"absence_retry_allowed,omitempty"`
	AbsenceRetryUsed    bool       `json:"absence_retry_used,omitempty"`
	// What the last decided start or update showed, so the next update knows
	// whether it changes the card's controls (priority 10) or only its
	// content (priority 5). Priority is the pending send's own.
	LastPhase        string `json:"last_phase,omitempty"`
	LastActivityKind string `json:"last_activity_kind,omitempty"`
	LastCanWrite     bool   `json:"last_can_write,omitempty"`
	Priority         int    `json:"priority,omitempty"`
}

// APNs priorities for Live Activity pushes: 10 is delivered at once and
// counts against the activity's budget, 5 is opportunistic.
const (
	PriorityImmediate = 10
	PriorityCosmetic  = 5
)

// updatePriority: a start, an end and any update that changes what the card's
// controls mean — phase, activity kind (running, paused, break, side or
// bottle), or write access — is immediate; a content edit in the same phase
// (a corrected start time, a renamed baby, the parts list) is cosmetic. A
// retry keeps the urgency of the send it repeats.
func updatePriority(s TargetState, event string, desired *DesiredCard) int {
	if event != "update" || s.Status != "active" || (s.PendingEvent != "" && s.Priority == PriorityImmediate) ||
		s.LastPhase != desired.Content.Phase || s.LastActivityKind != desired.Content.ActivityKind || s.LastCanWrite != desired.CanWrite {
		return PriorityImmediate
	}
	return PriorityCosmetic
}

type Decision struct {
	Event    string
	State    TargetState
	Deadline time.Time
}

func referenceEqual(a, b *string) bool { return a != nil && b != nil && *a == *b }

func sameLifecycle(s TargetState, d *DesiredCard) bool {
	if s.Status == "terminal" || s.GenerationID == "" || s.Kind != d.Scope.KindRaw {
		return false
	}
	if referenceEqual(s.TimerID, d.TimerID) {
		return true
	}
	return s.Kind == "feed" && referenceEqual(s.SessionID, d.SessionID)
}

func desiredDigest(d *DesiredCard) string {
	if d == nil {
		return ""
	}
	c := *d
	c.Sequence = 0
	c.Content.Remote = nil
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// deterministicID keeps reconciliation pure; the persisted previous generation
// and committed sequence distinguish successive lifecycles for the same scope.
func deterministicID(seed string) string {
	h := sha256.Sum256([]byte(seed))
	h[6] = (h[6] & 15) | 0x50
	h[8] = (h[8] & 63) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// Reconcile never manufactures a future APNs timestamp. Callers must project
// again at Deadline; only the current, authorized snapshot may become a send.
func Reconcile(previous TargetState, desired *DesiredCard, now time.Time) Decision {
	s := previous
	decision := func(event string, at time.Time) Decision { return Decision{event, s, at} }
	if desired != nil && desired.ExpiresAt != nil && !now.Before(*desired.ExpiresAt) {
		desired = nil
	}
	if desired == nil || (s.GenerationID != "" && !sameLifecycle(s, desired)) {
		if s.ActivityID != "" {
			if s.PendingEvent == "end" && s.RetryAt != nil && now.Before(*s.RetryAt) {
				return decision("wait", *s.RetryAt)
			}
			if now.Unix() <= s.LastTimestamp {
				return decision("wait", time.Unix(s.LastTimestamp+1, 0))
			}
			s.Status = "terminal"
			s.PendingEvent = "end"
			s.Attempts++
			s.RetryAt = nil
			s.LastTimestamp = now.Unix()
			s.PresentationVersion++
			s.AttemptID = deterministicID(fmt.Sprintf("%s/end/%d", s.GenerationID, s.PresentationVersion))
			return decision("end", now.Add(time.Second))
		}
		s.Status = "terminal"
		s.PendingEvent = ""
		s.RetryAt = nil
		if desired == nil {
			return decision("none", time.Time{})
		}
	}
	if !sameLifecycle(s, desired) {
		if !desired.MayStart {
			return decision("none", time.Time{})
		}
		generation := deterministicID(fmt.Sprintf("%s/%s/%s/%d/%d", s.GenerationID, desired.Scope.BabyID, desired.Scope.KindRaw, desired.Sequence, now.UnixNano()))
		s = TargetState{GenerationID: generation, Kind: desired.Scope.KindRaw, Status: "pending", TimerID: desired.TimerID, SessionID: desired.SessionID, SuppressStart: s.SuppressStart, ActivityID: s.ActivityID, TokenVersion: s.TokenVersion, KnownActivityIDs: s.KnownActivityIDs}
	}
	s.TimerID = desired.TimerID
	s.SessionID = desired.SessionID
	s.ExpiresAt = desired.ExpiresAt
	deadline := time.Time{}
	if s.ExpiresAt != nil {
		deadline = *s.ExpiresAt
	}
	if s.DismissedGeneration == s.GenerationID {
		s.PendingEvent = ""
		s.RetryAt = nil
		return decision("none", deadline)
	}
	digest := desiredDigest(desired)
	event := "update"
	if s.ActivityID == "" {
		if s.SuppressStart {
			return decision("wait", deadline)
		}
		if s.Status == "startUnconfirmed" && !s.AbsenceRetryAllowed {
			return decision("wait", deadline)
		}
		if !desired.MayStart {
			return decision("wait", deadline)
		}
		event = "start"
	} else if s.Status == "active" && s.LastDigest == digest && s.PendingEvent == "" {
		return decision("none", deadline)
	}
	if s.RetryAt != nil && now.Before(*s.RetryAt) {
		return decision("wait", *s.RetryAt)
	}
	if now.Unix() <= s.LastTimestamp {
		return decision("wait", time.Unix(s.LastTimestamp+1, 0))
	}
	if event == "start" && s.AbsenceRetryAllowed {
		s.AbsenceRetryUsed = true
		s.AbsenceRetryAllowed = false
	}
	s.Priority = updatePriority(s, event, desired)
	s.LastPhase, s.LastActivityKind, s.LastCanWrite = desired.Content.Phase, desired.Content.ActivityKind, desired.CanWrite
	s.Status = "active"
	if event == "start" {
		s.Status = "startUnconfirmed"
	}
	s.LastDigest = digest
	s.LastTimestamp = now.Unix()
	s.LastSequence = desired.Sequence
	s.PresentationVersion++
	s.PendingEvent = event
	s.Attempts++
	s.RetryAt = nil
	s.AttemptID = deterministicID(fmt.Sprintf("%s/%s/%d", s.GenerationID, event, s.PresentationVersion))
	if event == "start" {
		s.StartAttemptID = s.AttemptID
	}
	return decision(event, deadline)
}

func retryDelay(attempt int, jitter func(time.Duration) time.Duration) time.Duration {
	seconds := []int{2, 4, 8, 16, 32, 60}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(seconds) {
		attempt = len(seconds)
	}
	d := time.Duration(seconds[attempt-1]) * time.Second
	if jitter != nil {
		d = jitter(d)
	}
	if d < time.Second {
		d = time.Second
	}
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}
