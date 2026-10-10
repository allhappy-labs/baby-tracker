// Package apns is the server's one APNs client: the wake worker's
// contentless background pushes (owner decision D4) and Live Activity
// pushes both go through Send. It is main's Live Activity APNs transport
// (Server/internal/liveactivity/{apns.go,provider_token.go}) made generic over
// the push type: the same ES256 provider token, host per environment,
// bounded request, response classification and Retry-After handling. The
// topic is derived from the bundle ID, never supplied by a caller.
//
// A wake's payload is always WakePayload: no family hint, care data, name or
// count ever leaves the server in a wake. Nothing in this package logs;
// callers log a Result's class, status and reason only, never a token, the
// provider token or the key.
package apns
