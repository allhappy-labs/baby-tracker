package familyread

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"babytracker/server/internal/protocol"
)

// A history continuation is "h1." + base64url(claims JSON) + "." +
// base64url(HMAC-SHA256(cursor key, domain label || 0x00 || claims JSON)).
// It is self-contained: nothing is persisted, so a read-only transaction can
// issue and check it. The claims bind the position to everything that defines
// the result set; a mismatch on any of them rejects the continuation.
const (
	cursorPrefix   = "h1."
	cursorDomain   = "babytracker familyread history cursor v1"
	cursorVersion  = 1
	maxCursorBytes = 1024
)

// cursorBinding is what a continuation must match exactly: the grant, its
// account and family, the server instance and recovery epoch, and the query
// filters (baby, normalized range in microseconds, normalized kinds). The
// cursor carries only its digest, so it neither grows with the filters nor
// shows the bound identifiers to the client.
type cursorBinding struct {
	Grant    string   `json:"g"`
	Account  string   `json:"a"`
	Family   string   `json:"f"`
	Instance string   `json:"i"`
	Epoch    uint64   `json:"p"`
	Baby     string   `json:"b"`
	Start    int64    `json:"s"`
	End      int64    `json:"e"`
	Kinds    []string `json:"k"`
}

func (b cursorBinding) digest() ([]byte, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

type cursorClaims struct {
	Version    int    `json:"v"`
	Binding    []byte `json:"d"`
	Revision   uint64 `json:"r"`
	Expires    int64  `json:"x"`
	AfterStart int64  `json:"as"`
	AfterKind  string `json:"ak"`
	AfterID    string `json:"ai"`
}

// position is the last returned (event start, kind, id).
type position struct {
	start    time.Time
	kind, id string
}

func (s *Service) cursorMAC(payload []byte) []byte {
	mac := hmac.New(sha256.New, s.cursorKey[:])
	mac.Write([]byte(cursorDomain))
	mac.Write([]byte{0})
	mac.Write(payload)
	return mac.Sum(nil)
}

func (s *Service) sealCursor(binding cursorBinding, revision uint64, expires time.Time, after HistoryEntry) (string, error) {
	digest, err := binding.digest()
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(cursorClaims{Version: cursorVersion, Binding: digest, Revision: revision,
		Expires: expires.UnixMicro(), AfterStart: after.Start.UnixMicro(), AfterKind: after.Kind, AfterID: after.ID})
	if err != nil {
		return "", err
	}
	return cursorPrefix + base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(s.cursorMAC(payload)), nil
}

// openCursor authenticates a continuation before reading any of it. Every
// failure is ErrInvalidCursor.
func (s *Service) openCursor(token string) (cursorClaims, error) {
	if len(token) > maxCursorBytes || !strings.HasPrefix(token, cursorPrefix) {
		return cursorClaims{}, ErrInvalidCursor
	}
	body, signature, ok := strings.Cut(token[len(cursorPrefix):], ".")
	if !ok || strings.Contains(signature, ".") {
		return cursorClaims{}, ErrInvalidCursor
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil {
		return cursorClaims{}, ErrInvalidCursor
	}
	mac, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || !hmac.Equal(mac, s.cursorMAC(payload)) {
		return cursorClaims{}, ErrInvalidCursor
	}
	var claims cursorClaims
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&claims); err != nil || decoder.More() {
		return cursorClaims{}, ErrInvalidCursor
	}
	if claims.Version != cursorVersion || !slices.Contains(historyKinds[:], claims.AfterKind) || !protocol.ValidUUID(claims.AfterID) {
		return cursorClaims{}, ErrInvalidCursor
	}
	return claims, nil
}

// continueFrom checks an authenticated continuation against the current read:
// binding first, then expiry at the read's captured time, then the family
// data revision read in this same transaction.
func continueFrom(claims cursorClaims, binding cursorBinding, revision uint64, asOf time.Time) (position, error) {
	digest, err := binding.digest()
	if err != nil {
		return position{}, err
	}
	if !hmac.Equal(claims.Binding, digest) || !slices.Contains(binding.Kinds, claims.AfterKind) {
		return position{}, ErrInvalidCursor
	}
	if !asOf.Before(time.UnixMicro(claims.Expires)) {
		return position{}, ErrCursorExpired
	}
	if claims.Revision != revision {
		return position{}, ErrHistoryChanged
	}
	return position{start: time.UnixMicro(claims.AfterStart).UTC(), kind: claims.AfterKind, id: claims.AfterID}, nil
}
