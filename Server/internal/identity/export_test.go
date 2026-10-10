package identity

import (
	"net/http"
	"testing"
	"time"
)

// Test hooks for the external identity_test package, which exercises the
// identity services against the PostgreSQL authority (the authority package
// imports identity, so these tests cannot live in package identity).

const (
	AppleIssuerForTest   = appleIssuer
	AppleAudienceForTest = testAppleAudience
)

func IdentityDigestForTest(key DigestKey, audience, subject string) KeyedDigest {
	return identityDigest(key, "apple", audience, subject)
}

func AuthorityDigestForTest(digest KeyedDigest) string { return authorityDigest(digest) }

func KeyringForTest() DigestKeyring { return testKeyring() }

// AppleFixture signs Apple identity tokens and notifications with a test key
// that its HTTP client serves as Apple's JWKS.
type AppleFixture struct{ fixture *appleVerifierFixture }

func NewAppleFixtureForTest(t *testing.T) *AppleFixture {
	return &AppleFixture{fixture: newAppleVerifierFixture(t)}
}

func (apple *AppleFixture) Verifier() IdentityVerifier { return apple.fixture.verifier() }
func (apple *AppleFixture) Client() *http.Client       { return apple.fixture.client }
func (apple *AppleFixture) Now() time.Time             { return apple.fixture.now }
func (apple *AppleFixture) SetNow(now time.Time)       { apple.fixture.now = now }
func (apple *AppleFixture) SetNonce(nonce string)      { apple.fixture.nonce = nonce }

// IdentityToken is a valid identity token for subject "apple-subject" under
// the fixture's current nonce.
func (apple *AppleFixture) IdentityToken(t *testing.T) string {
	return signJWT(t, apple.fixture.key, `{"alg":"RS256","kid":"apple-key"}`, apple.fixture.claims(""))
}

// NotificationBody is a signed Apple server notification envelope.
func (apple *AppleFixture) NotificationBody(t *testing.T, claims string) string {
	return notificationEnvelope(signJWT(t, apple.fixture.key, `{"alg":"RS256","kid":"apple-key"}`, claims))
}
