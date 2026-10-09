# Identity v1 protocol

`GET /v1/server` advertises only capabilities implemented by the running binary. This release advertises `identity-v1`. The reserved registry also contains `family-lifecycle-v1`, `recovery-v1`, and `device-continuity-v1`; those values are not accepted by the challenge operation yet.

The required `apple_authorization_mode` is `identity_token_only` for development and self-hosted servers, and `code_exchange_required` for the hosted service. The Apple session route rejects authorization-code input in the former mode and requires it in the latter.

## Challenge and nonce

`POST /v1/auth/challenge` accepts at most 32 KiB of strict JSON:

```json
{"protocol_version":"v1","capabilities":["identity-v1"]}
```

The route is recovery-gated and rate-limited by canonical client IP before request validation. Responses, including errors, carry `Cache-Control: no-store`. A successful response contains a cryptographically random 32-byte unpadded-base64url challenge that expires after exactly five minutes and can be consumed once.

Clients pass Apple's lowercase hexadecimal SHA-256 nonce:

```text
SHA256("baby-tracker-auth-v1" || 0x00 || canonical_origin || 0x00 || server_instance_id || 0x00 || challenge)
```

All text is UTF-8. The origin is a canonical HTTPS origin with no path, query, fragment, credentials, or IP-literal host. The instance ID is a lowercase canonical nonzero UUID. The shared cross-language vector is [`auth-nonce-v1.json`](../../api/fixtures/auth-nonce-v1.json).

## Proof verification and privacy

The server accepts only an Apple RS256 identity token from `https://appleid.apple.com`, for its exact configured audience and expected nonce. Token, JSON, JWKS, timestamp, key, redirect, and cache bounds fail closed. The Apple subject is stored in erasable application state; durable lookup and authority references use a versioned HMAC digest. Neither raw IP addresses nor bearer proofs are logged or persisted.

Requests reject unknown fields, duplicate or escaped-equivalent keys, invalid UTF-8, trailing JSON, unsupported capabilities, and missing required metadata. Responses are additive: clients must ignore unknown response fields.

## Opaque sessions and continuity

`POST /v1/auth/apple` consumes a challenge and returns an opaque access/refresh pair. The response binds the tokens to the server recovery epoch, device, account projection, optional family slot, access expiry, and absolute reauthentication deadline. Only first registration returns a one-time `device_credential`; a continued-device sign-in omits it, so clients retain the existing Keychain value.

A fresh Apple exchange always persists an account-scoped session, even when its
response reports an existing `family_slot`. After the authority projection is
ready, the client rotates that exchange once through `POST /v1/auth/refresh`;
the refresh snapshots the current family slot into the durable session and is
the credential used for family, sync, import, and live routes. This explicit
step also prevents a fresh proof from implicitly acquiring recovery authority.

`POST /v1/auth/refresh` rotates the refresh credential. Reusing a consumed credential revokes its full device lineage and returns `refresh_reused`; the client must perform a fresh Apple proof. `POST /v1/auth/logout` authenticates one bearer access token and idempotently revokes that device binding plus all of its sessions using the supplied command ID.

Exchange is charged to the canonical client-IP bucket before Apple proof work.
Refresh first charges its client-IP bucket, then performs a read-only keyed
credential lookup and atomically charges the known account and device buckets
before rotation. A denied bucket returns matching positive `Retry-After` and
`retry_after_seconds`; a limiter dependency failure fails closed with 503.

Access credentials are accepted only in one `Authorization: Bearer` header. Query parameters, cookies, and WebSocket subprotocols are never credential transports. Every auth success or error has `Cache-Control: no-store`; every 401 uses the same safe message and `WWW-Authenticate: Bearer`.

Hosted servers additionally register `POST /v1/auth/apple/notifications` for the exact signed Apple notification envelope. Self-hosted servers leave the path unregistered. The notification path authenticates Apple’s compact JWS, bounds event age, and derives durable keyed replay identities before applying destructive identity-unavailable authority events. It never accepts unsigned provider fields or returns provider text.
