package care

import (
	"math"

	"babytracker/server/internal/protocol"
)

// Derived holds the write-time aggregates of a row's parts. Each is what the
// legacy reads computed from feed_components at read time:
//   - BreastSeconds: the sum of the breast parts' seconds.
//   - BottleVolumeML: the sum of the bottle parts' recorded volumes, nil when
//     no bottle part records one (familyread summary_sql.go bottle_feeds:
//     sum(volume_ml), null without observations).
//   - BottleObservations: bottle parts with a recorded volume
//     (count(volume_ml)).
//   - BottleMissing: bottle parts without one (count(*) - count(volume_ml)).
//   - ShortestPartSeconds: the shortest timed part (breast, or a bottle that
//     carries seconds), nil when no part is timed. The schema's 1..604800
//     bound on it is the zero-duration second line.
//
// A feed has all of them; a timer only ShortestPartSeconds (its running
// bottles never count as volume); every other row and every tombstone has
// none.
type Derived struct {
	BreastSeconds       int32
	BottleVolumeML      *protocol.Decimal
	BottleObservations  int16
	BottleMissing       int16
	ShortestPartSeconds *int32
}

// DeriveFeed computes a feed's derived columns from its parts.
func DeriveFeed(components []protocol.FeedComponent) Derived {
	var derived Derived
	var volumes []protocol.Decimal
	breast := int64(0)
	for _, component := range components {
		switch component.Kind {
		case "breast":
			if component.Seconds != nil {
				breast += int64(*component.Seconds)
			}
		case "bottle":
			if component.VolumeML != nil {
				volumes = append(volumes, *component.VolumeML)
				derived.BottleObservations++
			} else {
				derived.BottleMissing++
			}
		}
	}
	derived.BreastSeconds = clampSeconds(breast)
	if len(volumes) > 0 {
		sum := protocol.SumDecimals(volumes)
		derived.BottleVolumeML = &sum
	}
	derived.ShortestPartSeconds = shortestPart(components)
	return derived
}

// shortestPart is the fewest seconds of any timed part, or nil.
func shortestPart(components []protocol.FeedComponent) *int32 {
	var shortest *int32
	for _, component := range components {
		if component.Seconds == nil || (component.Kind != "breast" && component.Kind != "bottle") {
			continue
		}
		if seconds := clampSeconds(int64(*component.Seconds)); shortest == nil || seconds < *shortest {
			shortest = &seconds
		}
	}
	return shortest
}

// clampSeconds keeps an out-of-range total out of int32 wraparound, so the
// schema's bound (not a wrapped value inside it) rejects it. Validation
// bounds every part at 604800 seconds before a row is encoded.
func clampSeconds(seconds int64) int32 {
	return int32(min(seconds, math.MaxInt32))
}
