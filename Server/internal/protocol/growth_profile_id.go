package protocol

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
)

// GrowthProfileID derives the per-baby UUIDv5 identity using network-order UUID bytes.
// Invalid UUID input returns an empty identity; callers validate baby IDs separately.
func GrowthProfileID(babyID string) string {
	if !canonicalUUIDPattern.MatchString(babyID) {
		return ""
	}
	namespace, err := hex.DecodeString(strings.ReplaceAll(babyID, "-", ""))
	if err != nil || len(namespace) != 16 {
		return ""
	}
	digest := sha1.Sum(append(namespace, []byte("growth-profile-v1")...))
	value := digest[:16]
	value[6] = (value[6] & 15) | 80
	value[8] = (value[8] & 63) | 128
	h := hex.EncodeToString(value)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
