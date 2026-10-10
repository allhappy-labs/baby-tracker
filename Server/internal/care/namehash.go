package care

import (
	"crypto/sha256"

	"babytracker/server/internal/protocol"
)

var babyMatchDomain = []byte("baby-tracker-baby-match-v1\x00")

// BabyNameHash is the 32-byte key duplicate-baby matching compares (the
// name_hash column; legacy matched normalized_name, birth_date and
// normalization_version): sha256 of the domain, the normalization version,
// NUL, NormalizeBabyName(name), NUL and the birth date.
func BabyNameHash(name, birthDate string) ([32]byte, error) {
	normalized, err := protocol.NormalizeBabyName(name)
	if err != nil {
		return [32]byte{}, err
	}
	input := make([]byte, 0, len(babyMatchDomain)+len(protocol.NormalizationVersion)+len(normalized)+len(birthDate)+2)
	input = append(input, babyMatchDomain...)
	input = append(input, protocol.NormalizationVersion...)
	input = append(input, 0)
	input = append(input, normalized...)
	input = append(input, 0)
	input = append(input, birthDate...)
	return sha256.Sum256(input), nil
}
