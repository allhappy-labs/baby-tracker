//go:build identitytest

package loadseed

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// ID domains: an entity ID is the first 16 bytes of SHA-256 of
// seed‖family index‖ordinal, shaped as a UUIDv4. Mutation and feed part IDs
// prefix a domain byte, so the three never collide.
const (
	domainMutation  byte = 'm'
	domainComponent byte = 'c'
)

// entityID is the care entity with ordinal in family's plan under seed.
func entityID(seed uint64, family int, ordinal uint64) string {
	var input [24]byte
	binary.BigEndian.PutUint64(input[0:8], seed)
	binary.BigEndian.PutUint64(input[8:16], uint64(family))
	binary.BigEndian.PutUint64(input[16:24], ordinal)
	sum := sha256.Sum256(input[:])
	return shapeUUIDv4(sum[:16])
}

// domainID is a mutation or feed part ID: as entityID, with domain first.
func domainID(domain byte, seed uint64, family int, ordinal uint64) string {
	var input [25]byte
	input[0] = domain
	binary.BigEndian.PutUint64(input[1:9], seed)
	binary.BigEndian.PutUint64(input[9:17], uint64(family))
	binary.BigEndian.PutUint64(input[17:25], ordinal)
	sum := sha256.Sum256(input[:])
	return shapeUUIDv4(sum[:16])
}

// syntheticToken is a 64-hex synthetic APNs token for what (push or Live
// Activity), unique per seed, family, device slot and ordinal.
func syntheticToken(what string, seed uint64, family, slot int, ordinal uint64) string {
	var input [32]byte
	binary.BigEndian.PutUint64(input[0:8], seed)
	binary.BigEndian.PutUint64(input[8:16], uint64(family))
	binary.BigEndian.PutUint64(input[16:24], uint64(slot))
	binary.BigEndian.PutUint64(input[24:32], ordinal)
	sum := sha256.Sum256(append([]byte("baby-tracker-load/"+what+"\x00"), input[:]...))
	return hex.EncodeToString(sum[:])
}

func shapeUUIDv4(b []byte) string {
	var value [16]byte
	copy(value[:], b)
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	h := hex.EncodeToString(value[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
