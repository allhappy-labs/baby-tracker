package identity

import "context"

// The hosted Apple code exchange. Its revocation half (HostedApple.Revoke,
// AppleRevoker, AppleAuthority and the credential binding it carried) had no
// caller since the Apple revocation worker was removed, and went with the
// worker's database role (Phase 4 Task 27).

type AppleTokens struct {
	IdentityToken string
	RefreshToken  string
}

type AppleCodeExchangeRequest struct {
	AuthorizationCode string
	ExpectedNonce     string
	ExpectedIdentity  VerifiedIdentity
}

type AppleCodeExchanger interface {
	ExchangeCode(context.Context, AppleCodeExchangeRequest) (AppleTokens, error)
}
