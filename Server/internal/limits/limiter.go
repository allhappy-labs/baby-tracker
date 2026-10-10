package limits

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	maximumCharges = 5
	maximumValue   = 1_000_000
	maximumHops    = 32
)

type ErrorCode string

const (
	CodeInvalidLimit        ErrorCode = "invalid_rate_limit"
	CodeInvalidForwardedFor ErrorCode = "invalid_forwarded_for"
	CodeLimitUnavailable    ErrorCode = "rate_limit_unavailable"
)

type safeError struct {
	code      ErrorCode
	retryable bool
	cause     error
}

func (err *safeError) Error() string { return string(err.code) }
func (err *safeError) Unwrap() error { return err.cause }

func ErrorCodeOf(err error) ErrorCode {
	var safe *safeError
	if errors.As(err, &safe) {
		return safe.code
	}
	return ""
}

func IsRetryable(err error) bool {
	var safe *safeError
	return errors.As(err, &safe) && safe.retryable
}

type Options struct {
	Pepper [32]byte
	Now    func() time.Time
	// MaxVerifiedEntries bounds the buckets keyed by server-verified IDs;
	// 0 means DefaultMaxVerifiedEntries.
	MaxVerifiedEntries int
	// MaxIPEntriesPerScope bounds each IP-keyed scope's buckets; 0 means
	// DefaultMaxIPEntriesPerScope.
	MaxIPEntriesPerScope int
	// Logger receives at most one line a minute per full or evicting pool;
	// nil keeps only the counters (Stats).
	Logger *slog.Logger
}

type Service struct {
	store  rateStore
	pepper [32]byte
	now    func() time.Time
}

// New returns the in-memory limiter. It has no database dependency: no
// phone-path rate limit survives a restart (see the Phase 3 rate-limit ruling).
func New(options Options) (Limiter, error) {
	if options.Now == nil || options.Pepper == ([32]byte{}) || options.MaxVerifiedEntries < 0 || options.MaxIPEntriesPerScope < 0 {
		return nil, &safeError{code: CodeInvalidLimit}
	}
	store := newMemoryStore(options.MaxVerifiedEntries, options.MaxIPEntriesPerScope)
	store.logger = options.Logger
	return newLimiter(store, options.Pepper, options.Now), nil
}

// Stats reports the in-memory store's capacity events: refusals by the full
// verified-key pool and evictions per IP scope.
func (service *Service) Stats() Stats {
	if service != nil {
		if store, ok := service.store.(*memoryStore); ok {
			return store.stats()
		}
	}
	return Stats{IPEvictions: map[string]uint64{}}
}

func newLimiter(store rateStore, pepper [32]byte, now func() time.Time) *Service {
	return &Service{store: store, pepper: pepper, now: now}
}

func (service *Service) Take(ctx context.Context, scope, key string, policy Policy) (Decision, error) {
	return service.TakeMany(ctx, []Charge{{Scope: scope, Key: key, Cost: 1, Policy: policy}})
}

func (service *Service) TakeMany(ctx context.Context, charges []Charge) (Decision, error) {
	if service == nil || service.store == nil || service.now == nil || service.pepper == ([32]byte{}) {
		return Decision{}, &safeError{code: CodeInvalidLimit}
	}
	now := service.now().UTC().Truncate(time.Microsecond)
	prepared, err := prepareCharges(charges, service.pepper, now)
	if err != nil {
		return Decision{}, err
	}
	decision, err := service.store.take(ctx, prepared, now)
	if err != nil {
		return Decision{}, &safeError{code: CodeLimitUnavailable, retryable: true, cause: err}
	}
	if decision.Remaining < 0 || decision.Remaining > maximumValue || decision.RetryAfter < 0 ||
		(decision.Allowed && decision.RetryAfter != 0) || (!decision.Allowed && decision.RetryAfter < time.Second) {
		return Decision{}, &safeError{code: CodeLimitUnavailable, retryable: true}
	}
	return decision, nil
}

// Peek is the decision TakeMany would return for charges, with no bucket
// created, charged or evicted.
func (service *Service) Peek(ctx context.Context, charges []Charge) (Decision, error) {
	if service == nil || service.store == nil || service.now == nil || service.pepper == ([32]byte{}) {
		return Decision{}, &safeError{code: CodeInvalidLimit}
	}
	now := service.now().UTC().Truncate(time.Microsecond)
	prepared, err := prepareCharges(charges, service.pepper, now)
	if err != nil {
		return Decision{}, err
	}
	decision, err := service.store.peek(ctx, prepared, now)
	if err != nil {
		return Decision{}, &safeError{code: CodeLimitUnavailable, retryable: true, cause: err}
	}
	return decision, nil
}

// prepareCharges validates a request's charges and keys each one by
// HMAC(pepper, scope || 0 || normalized key), so no raw key reaches the store.
// The result is sorted by (scope, digest) and holds no duplicate bucket.
func prepareCharges(charges []Charge, pepper [32]byte, now time.Time) ([]preparedCharge, error) {
	if len(charges) < 1 || len(charges) > maximumCharges || pepper == ([32]byte{}) || now.IsZero() {
		return nil, &safeError{code: CodeInvalidLimit}
	}
	result := make([]preparedCharge, 0, len(charges))
	for _, charge := range charges {
		if _, exists := defaultPolicies[charge.Scope]; !exists || charge.Cost < 1 || charge.Cost > maximumValue ||
			charge.Policy.Count < 1 || charge.Policy.Count > PolicyMaximum(charge.Scope) ||
			charge.Policy.Window < time.Microsecond || charge.Policy.Window > 24*time.Hour || charge.Policy.Window%time.Microsecond != 0 ||
			PolicyWindowIsFixed(charge.Scope) && charge.Policy.Window != time.Minute {
			return nil, &safeError{code: CodeInvalidLimit}
		}
		normalized, err := normalizeKey(charge.Scope, charge.Key)
		if err != nil {
			return nil, err
		}
		windowStart := now.Truncate(charge.Policy.Window).UTC()
		windowEnd := windowStart.Add(charge.Policy.Window)
		if windowStart.After(now) || !windowEnd.After(now) {
			return nil, &safeError{code: CodeInvalidLimit}
		}
		mac := hmac.New(sha256.New, pepper[:])
		_, _ = mac.Write([]byte(charge.Scope))
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(normalized))
		prepared := preparedCharge{scope: charge.Scope, cost: charge.Cost, limit: charge.Policy.Count, windowStart: windowStart, windowEnd: windowEnd}
		copy(prepared.digest[:], mac.Sum(nil))
		result = append(result, prepared)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].scope != result[right].scope {
			return result[left].scope < result[right].scope
		}
		return bytes.Compare(result[left].digest[:], result[right].digest[:]) < 0
	})
	for index := 1; index < len(result); index++ {
		if result[index-1].scope == result[index].scope && result[index-1].digest == result[index].digest {
			return nil, &safeError{code: CodeInvalidLimit}
		}
	}
	return result, nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// normalizeKey canonicalizes a bucket key. A client address is keyed by
// itself for IPv4 and by its /64 for IPv6, one subscriber's allocation, so a
// single /64 cannot mint 2^64 keys.
func normalizeKey(scope, key string) (string, error) {
	switch scopeKeyClasses[scope] {
	case keyIP:
		address, err := netip.ParseAddr(key)
		if err != nil || address.Zone() != "" {
			return "", &safeError{code: CodeInvalidLimit}
		}
		address = address.Unmap()
		if address.Is6() {
			address = netip.PrefixFrom(address, 64).Masked().Addr()
		}
		return address.String(), nil
	case keyVerified:
	default:
		return "", &safeError{code: CodeInvalidLimit}
	}
	canonical := strings.ToLower(key)
	if !uuidPattern.MatchString(canonical) {
		return "", &safeError{code: CodeInvalidLimit}
	}
	return canonical, nil
}

type ClientIPResolver struct {
	trusted []netip.Prefix
}

func NewClientIPResolver(trusted []netip.Prefix) (*ClientIPResolver, error) {
	result := &ClientIPResolver{trusted: make([]netip.Prefix, 0, len(trusted))}
	for _, prefix := range trusted {
		if !prefix.IsValid() || prefix.Addr().Zone() != "" {
			return nil, &safeError{code: CodeInvalidForwardedFor}
		}
		address := prefix.Addr()
		bits := prefix.Bits()
		if address.Is4In6() {
			address = address.Unmap()
			bits -= 96
		}
		if bits < 0 {
			return nil, &safeError{code: CodeInvalidForwardedFor}
		}
		result.trusted = append(result.trusted, netip.PrefixFrom(address, bits).Masked())
	}
	return result, nil
}

func (resolver *ClientIPResolver) Resolve(remoteAddress string, header http.Header) (netip.Addr, error) {
	peer, err := parseRemoteAddress(remoteAddress)
	if err != nil {
		return netip.Addr{}, &safeError{code: CodeInvalidForwardedFor}
	}
	if resolver == nil || !resolver.isTrusted(peer) {
		return peer, nil
	}
	values := header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return peer, nil
	}
	hops := make([]netip.Addr, 0, len(values))
	for _, value := range values {
		parts := strings.Split(value, ",")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if len(hops) == maximumHops || part == "" {
				return netip.Addr{}, &safeError{code: CodeInvalidForwardedFor}
			}
			address, parseErr := netip.ParseAddr(part)
			if parseErr != nil || address.Zone() != "" {
				return netip.Addr{}, &safeError{code: CodeInvalidForwardedFor}
			}
			hops = append(hops, address.Unmap())
		}
	}
	for index := len(hops) - 1; index >= 0; index-- {
		if !resolver.isTrusted(hops[index]) {
			return hops[index], nil
		}
	}
	return netip.Addr{}, &safeError{code: CodeInvalidForwardedFor}
}

func (resolver *ClientIPResolver) isTrusted(address netip.Addr) bool {
	address = address.Unmap()
	for _, prefix := range resolver.trusted {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func parseRemoteAddress(value string) (netip.Addr, error) {
	if addressPort, err := netip.ParseAddrPort(value); err == nil {
		return addressPort.Addr().Unmap(), nil
	}
	host, _, err := net.SplitHostPort(value)
	if err == nil {
		address, parseErr := netip.ParseAddr(host)
		if parseErr == nil && address.Zone() == "" {
			return address.Unmap(), nil
		}
	}
	address, err := netip.ParseAddr(value)
	if err != nil || address.Zone() != "" {
		return netip.Addr{}, errors.New("invalid remote address")
	}
	return address.Unmap(), nil
}
