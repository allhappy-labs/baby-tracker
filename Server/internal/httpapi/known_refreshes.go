package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"babytracker/server/internal/admission"
	"babytracker/server/internal/authority"
)

const (
	// knownRefreshPeriod is a generation of KnownRefreshes: a token is
	// remembered for 12 to 24 hours, so a phone that refreshes at least
	// about daily keeps its refreshes out of the auth lane.
	knownRefreshPeriod = 12 * time.Hour
	// knownRefreshLimit bounds one generation (two are kept), as
	// knownBearerLimit does: under 4 MiB each when full. A rotation forgets
	// its parent, so a generation holds about one token per device that
	// refreshed or signed in on this node within it.
	knownRefreshLimit = 1 << 16
	// KnownRefreshWarmLimit is the most digests Warm remembers: one
	// generation's bound. A node's live sessions are about one per signed-in
	// device (a 10k-family node has about 40,000), so it holds them all.
	KnownRefreshWarmLimit = knownRefreshLimit
	// defaultAuthBodyReadTotal and defaultAuthBodyReadsPerClient bound the
	// refresh bodies read before admission (refreshAdmission), as the
	// body-first routes' reads are bounded, but apart from them: at
	// authBodyBytes each, 64 in all buffer at most 2 MiB.
	defaultAuthBodyReadTotal      = 64
	defaultAuthBodyReadsPerClient = 8
)

// KnownRefreshes remembers the refresh tokens of live sessions this node
// knows, by their keyed digest (sessions.refresh_digest, under the token key
// that issued them), so that a refresh presenting one is tied to a known
// session before admission without a database statement and takes a Sync
// slot. It knows the tokens it issued (a sign-in's, a refresh's child) and,
// warmed at startup (Warm), the live sessions' digests the database lists,
// so a restart does not send every device's next refresh to the auth lane.
// Any other refresh (a token another node issued after startup, an expired
// or forged one) takes the lane. Like knownBearers it authorizes nothing:
// the refresh is judged in full by the session service either way. Only a
// token the node issued or the database lists is remembered, so a client
// without one cannot fill it. A lookup checks the token's digest under
// every configured token key (SessionService.RefreshDigests), so a session
// issued under a key since rotated to previous is still known.
type KnownRefreshes struct{ set *knownBearers }

// NewKnownRefreshes is an empty KnownRefreshes; logger may be nil (no log).
// New makes one when Options.KnownRefreshes is nil.
func NewKnownRefreshes(logger *slog.Logger) *KnownRefreshes {
	return newKnownRefreshes(time.Now, logger)
}

func newKnownRefreshes(now func() time.Time, logger *slog.Logger) *KnownRefreshes {
	return &KnownRefreshes{set: newKnownSet("known refresh tokens full", knownRefreshPeriod, knownRefreshLimit, now, logger)}
}

// Warm remembers the refresh digests of live sessions (at most
// KnownRefreshWarmLimit) and reports how many it holds. They fill the
// previous generation, so they last until the first generation passes (12
// hours) and leave the current one to the tokens the node issues: a warmed
// session's next refresh takes a Sync slot, and its child is remembered as
// any issued token.
func (k *KnownRefreshes) Warm(digests []authority.KeyedDigest) int {
	if k == nil {
		return 0
	}
	sums := make([][32]byte, 0, min(len(digests), knownRefreshLimit))
	for _, digest := range digests {
		sums = append(sums, digest.Sum)
	}
	return k.set.seed(sums)
}

// known reports whether any of a token's digests (one per token key) is
// remembered.
func (k *KnownRefreshes) known(digests []authority.KeyedDigest) bool {
	if k == nil {
		return false
	}
	for _, digest := range digests {
		if k.set.known(digest.Sum) {
			return true
		}
	}
	return false
}

// issued remembers a token the node handed out, by its current-key digest
// (the first): the digest the sign-in or rotation stored.
func (k *KnownRefreshes) issued(digests []authority.KeyedDigest) {
	if k != nil && len(digests) > 0 {
		k.set.remember(digests[0].Sum)
	}
}

// rotated remembers child, issued in exchange for parent, and forgets
// parent under every key: a consumed token's retry (a lost answer) takes
// the auth lane.
func (k *KnownRefreshes) rotated(parent, child []authority.KeyedDigest) {
	if k == nil {
		return
	}
	for _, digest := range parent {
		k.set.forget(digest.Sum)
	}
	k.issued(child)
}

// refreshDigests is token's digests under every token key, or none when
// the session service has none for it (a malformed token): such a token is
// never known.
func refreshDigests(options Options, token string) []authority.KeyedDigest {
	if token == "" || options.SessionService == nil {
		return nil
	}
	digests, err := options.SessionService.RefreshDigests(token)
	if err != nil {
		return nil
	}
	return digests
}

// refreshAdmission is WithAdmission for the refresh routes, whose class
// depends on their refresh token: Sync for a token this node issued
// (KnownRefreshes), the auth lane otherwise. The token is in the body, so
// the body (at most authBodyBytes+1 bytes) is read before admission, under
// Options.AuthBodyReads and the body read deadline, and replayed to the
// handler, which judges it exactly as before. A request beyond that bound
// is not read here: it takes the lane, and its handler reads its body as
// before. A body that is not JSON, or whose token is unknown, takes the
// lane too. Reading the body first changes no answer: every refusal the
// handler makes is the same, in the same order.
func refreshAdmission(options Options, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		class := admission.Auth
		if token, ok := peekRefreshToken(w, r, options); ok && options.KnownRefreshes.known(refreshDigests(options, token)) {
			class = admission.Sync
		}
		finish, ok := acquireAdmission(w, r, options.Admission, class, options.AdmissionMetrics)
		if !ok {
			return
		}
		defer finish()
		next.ServeHTTP(w, r)
	})
}

// peekRefreshToken buffers r's body (replacing r.Body with its replay) and
// returns the refresh_token it carries, or reports false: the read bound
// is full (nothing read), the read was cut short, or the body is not a
// JSON object with a string refresh_token. The read is not strict: the
// handler's DecodeRequest is.
func peekRefreshToken(w http.ResponseWriter, r *http.Request, options Options) (string, bool) {
	release, ok := options.AuthBodyReads.TryAcquire(bodyClientAddress(r, options))
	if !ok {
		return "", false
	}
	buffered, cutShort := bufferBody(w, r, options.BodyReadTimeout, authBodyBytes)
	release()
	r.Body = buffered
	if cutShort {
		return "", false
	}
	replay, ok := buffered.(*replayedBody)
	if !ok || replay.err != nil || replay.data.Size() > authBodyBytes {
		return "", false
	}
	var wire struct {
		RefreshToken string `json:"refresh_token"`
	}
	data := make([]byte, replay.data.Size())
	if _, err := replay.data.ReadAt(data, 0); err != nil && err != io.EOF {
		return "", false
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return "", false
	}
	return wire.RefreshToken, wire.RefreshToken != ""
}
