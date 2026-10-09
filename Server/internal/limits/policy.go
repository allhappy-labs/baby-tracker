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
	ScopeLiveActivityAccount     = "live-activity-account"
	ScopeLiveActivityDevice      = "live-activity-device"
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
}

var defaultPolicies = map[string]Policy{
	ScopeLiveActivityAccount:     {Count: 120, Window: time.Minute},
	ScopeLiveActivityDevice:      {Count: 30, Window: time.Minute},
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
}

func PolicyMaximum(scope string) int {
	switch scope {
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
		ScopeLiveAttemptIP, ScopeLiveAttemptDevice, ScopeLiveAttemptAccount, ScopeLiveAttemptFamily:
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
