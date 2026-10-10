package limits

import (
	"context"
	"time"
)

const (
	ScopeAuthChallengeIP         = "auth-challenge-ip"
	ScopeAuthExchangeIP          = "auth-exchange-ip"
	ScopeAuthRefreshIP           = "auth-refresh-ip"
	ScopeAuthRefreshAccount      = "auth-refresh-account"
	ScopeAuthRefreshDevice       = "auth-refresh-device"
	ScopeFamilyStructuralAccount = "family-structural-account"
	ScopeFamilyStructuralDevice  = "family-structural-device"
	ScopeInvitationCreateAccount = "invitation-create-account"
	ScopeInvitationCreateIP      = "invitation-create-ip"
	ScopeInvitationRedeemAccount = "invitation-redeem-account"
	ScopeInvitationRedeemIP      = "invitation-redeem-ip"
	ScopeSyncMutationDevice      = "sync-mutation-device"
	ScopeSyncMutationAccount     = "sync-mutation-account"
	ScopeSyncMutationFamily      = "sync-mutation-family"
	ScopeSyncPullDevice          = "sync-pull-device"
	ScopeSyncPullAccount         = "sync-pull-account"
	ScopeLiveAttemptIP           = "live-attempt-ip"
	ScopeLiveAttemptDevice       = "live-attempt-device"
	ScopeLiveAttemptAccount      = "live-attempt-account"
	ScopeLiveAttemptFamily       = "live-attempt-family"
	ScopeExportIP                = "export-create-ip"
	ScopeExportAccount           = "export-account"
	ScopeExportDevice            = "export-device"
	// Device push-token registration (PUT/DELETE /v1/devices/push-token),
	// charged per authenticated device and per account.
	ScopePushTokenDevice  = "push-token-device"
	ScopePushTokenAccount = "push-token-account"
	// Failed bearer checks on the routes that read a request body before
	// the credential is checked (sync, import, push-token), per client
	// address: an address over it is refused before its body is read.
	ScopeBearerFailureIP = "bearer-failure-ip"
	// Private MCP read quotas, charged together for every authenticated MCP
	// request: per integration grant, per owner account and per family.
	ScopeMCPIntegration = "mcp-integration"
	ScopeMCPAccount     = "mcp-account"
	ScopeMCPFamily      = "mcp-family"
	// Live Activity registration (PUT/DELETE /v1/live-activities/...),
	// charged per authenticated account and per device.
	ScopeLiveActivityAccount = "live-activity-account"
	ScopeLiveActivityDevice  = "live-activity-device"
)

type Policy struct {
	Count  int
	Window time.Duration
}

type Decision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

type Charge struct {
	Scope  string
	Key    string
	Cost   int
	Policy Policy
}

type Limiter interface {
	Take(context.Context, string, string, Policy) (Decision, error)
	TakeMany(context.Context, []Charge) (Decision, error)
	// Peek is the decision TakeMany would return for charges, with nothing
	// charged: whether a key already spent its budget, asked before the
	// work a further charge would pay for.
	Peek(context.Context, []Charge) (Decision, error)
}

var defaultPolicies = map[string]Policy{
	ScopeAuthChallengeIP:         {Count: 30, Window: 10 * time.Minute},
	ScopeAuthExchangeIP:          {Count: 30, Window: 10 * time.Minute},
	ScopeAuthRefreshIP:           {Count: 120, Window: 10 * time.Minute},
	ScopeAuthRefreshAccount:      {Count: 60, Window: 10 * time.Minute},
	ScopeAuthRefreshDevice:       {Count: 30, Window: 10 * time.Minute},
	ScopeFamilyStructuralAccount: {Count: 30, Window: time.Minute},
	ScopeFamilyStructuralDevice:  {Count: 60, Window: time.Minute},
	ScopeInvitationCreateAccount: {Count: 20, Window: time.Hour},
	ScopeInvitationCreateIP:      {Count: 60, Window: time.Hour},
	ScopeInvitationRedeemAccount: {Count: 20, Window: time.Hour},
	ScopeInvitationRedeemIP:      {Count: 60, Window: time.Hour},
	ScopeSyncMutationDevice:      {Count: 1200, Window: time.Minute},
	ScopeSyncMutationAccount:     {Count: 2400, Window: time.Minute},
	ScopeSyncMutationFamily:      {Count: 6000, Window: time.Minute},
	ScopeSyncPullDevice:          {Count: 120, Window: time.Minute},
	ScopeSyncPullAccount:         {Count: 240, Window: time.Minute},
	ScopeLiveAttemptIP:           {Count: 60, Window: time.Minute},
	ScopeLiveAttemptDevice:       {Count: 12, Window: time.Minute},
	ScopeLiveAttemptAccount:      {Count: 36, Window: time.Minute},
	ScopeLiveAttemptFamily:       {Count: 120, Window: time.Minute},
	ScopeExportIP:                {Count: 10, Window: time.Hour},
	ScopeExportAccount:           {Count: 60, Window: time.Minute},
	ScopeExportDevice:            {Count: 30, Window: time.Minute},
	ScopePushTokenDevice:         {Count: 10, Window: time.Hour},
	ScopePushTokenAccount:        {Count: 30, Window: time.Hour},
	ScopeBearerFailureIP:         {Count: 60, Window: 10 * time.Minute},
	ScopeMCPIntegration:          {Count: 30, Window: time.Minute},
	ScopeMCPAccount:              {Count: 60, Window: time.Minute},
	ScopeMCPFamily:               {Count: 120, Window: time.Minute},
	ScopeLiveActivityAccount:     {Count: 120, Window: time.Minute},
	ScopeLiveActivityDevice:      {Count: 30, Window: time.Minute},
}

// keyClass says who vouches for a scope's bucket key, and so which pool of
// the in-memory store holds its buckets.
type keyClass int

const (
	keyUnclassified keyClass = iota
	// keyVerified: an account, device, family or integration ID the server
	// verified before charging. The population is bounded by the database.
	keyVerified
	// keyIP: the client address. Anyone can mint new keys, so each IP scope
	// has its own bounded pool that evicts instead of refusing other users.
	keyIP
)

// scopeKeyClasses classifies every scope, in one place. A scope missing here
// is refused by prepareCharges and fails TestEveryScopeHasExactlyOneKeyClass.
var scopeKeyClasses = map[string]keyClass{
	ScopeAuthChallengeIP:         keyIP,
	ScopeAuthExchangeIP:          keyIP,
	ScopeAuthRefreshIP:           keyIP,
	ScopeAuthRefreshAccount:      keyVerified,
	ScopeAuthRefreshDevice:       keyVerified,
	ScopeFamilyStructuralAccount: keyVerified,
	ScopeFamilyStructuralDevice:  keyVerified,
	ScopeInvitationCreateAccount: keyVerified,
	ScopeInvitationCreateIP:      keyIP,
	ScopeInvitationRedeemAccount: keyVerified,
	ScopeInvitationRedeemIP:      keyIP,
	ScopeSyncMutationDevice:      keyVerified,
	ScopeSyncMutationAccount:     keyVerified,
	ScopeSyncMutationFamily:      keyVerified,
	ScopeSyncPullDevice:          keyVerified,
	ScopeSyncPullAccount:         keyVerified,
	ScopeLiveAttemptIP:           keyIP,
	ScopeLiveAttemptDevice:       keyVerified,
	ScopeLiveAttemptAccount:      keyVerified,
	ScopeLiveAttemptFamily:       keyVerified,
	ScopeExportIP:                keyIP,
	ScopeExportAccount:           keyVerified,
	ScopeExportDevice:            keyVerified,
	ScopePushTokenDevice:         keyVerified,
	ScopePushTokenAccount:        keyVerified,
	ScopeBearerFailureIP:         keyIP,
	ScopeMCPIntegration:          keyVerified,
	ScopeMCPAccount:              keyVerified,
	ScopeMCPFamily:               keyVerified,
	ScopeLiveActivityAccount:     keyVerified,
	ScopeLiveActivityDevice:      keyVerified,
}

func PolicyMaximum(scope string) int {
	switch scope {
	case ScopeMCPIntegration, ScopeMCPAccount, ScopeMCPFamily:
		// MCP quotas may be lowered but never raised above the private defaults.
		return defaultPolicies[scope].Count
	case ScopeSyncMutationDevice, ScopeSyncMutationAccount, ScopeSyncMutationFamily:
		return 100_000
	case ScopeSyncPullDevice, ScopeSyncPullAccount, ScopeLiveAttemptIP, ScopeLiveAttemptDevice, ScopeLiveAttemptAccount, ScopeLiveAttemptFamily:
		return 10_000
	default:
		return maximumValue
	}
}

func PolicyWindowIsFixed(scope string) bool {
	switch scope {
	case ScopeSyncMutationDevice, ScopeSyncMutationAccount, ScopeSyncMutationFamily, ScopeSyncPullDevice, ScopeSyncPullAccount,
		ScopeLiveAttemptIP, ScopeLiveAttemptDevice, ScopeLiveAttemptAccount, ScopeLiveAttemptFamily,
		ScopeMCPIntegration, ScopeMCPAccount, ScopeMCPFamily:
		return true
	default:
		return false
	}
}

func DefaultPolicies() map[string]Policy {
	result := make(map[string]Policy, len(defaultPolicies))
	for scope, policy := range defaultPolicies {
		result[scope] = policy
	}
	return result
}
