package identity

import "context"

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

type AppleRevocationRequest struct {
	Binding      CredentialBinding
	RefreshToken []byte
}

type AppleRevoker interface {
	Revoke(context.Context, AppleRevocationRequest) error
}

type AppleAuthority interface {
	AppleCodeExchanger
	AppleRevoker
}
