package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"babytracker/server/internal/limits"
)

type LimitsConfig struct {
	Policies       map[string]limits.Policy
	MaxCaregivers  int
	MaxInvitations int
	InvitationTTL  time.Duration
}

func LoadLimits(getenv func(string) string) (LimitsConfig, error) {
	if getenv == nil {
		return LimitsConfig{}, errors.New("configuration getter is required")
	}
	policies := limits.DefaultPolicies()
	for scope, policy := range policies {
		stem := "BT_RATE_LIMIT_" + strings.ToUpper(strings.ReplaceAll(scope, "-", "_"))
		if raw := getenv(stem + "_COUNT"); raw != "" {
			maximum := limits.PolicyMaximum(scope)
			count, err := strconv.Atoi(raw)
			if err != nil || strconv.Itoa(count) != raw || count < 1 || count > maximum {
				return LimitsConfig{}, fmt.Errorf("%s_COUNT must be a canonical integer from 1 through %d", stem, maximum)
			}
			policy.Count = count
		}
		if raw := getenv(stem + "_WINDOW"); raw != "" {
			if limits.PolicyWindowIsFixed(scope) {
				return LimitsConfig{}, fmt.Errorf("%s_WINDOW is fixed at 1m", stem)
			}
			window, err := time.ParseDuration(raw)
			if err != nil || strings.TrimSpace(raw) != raw || window < time.Microsecond || window > 24*time.Hour || window%time.Microsecond != 0 {
				return LimitsConfig{}, fmt.Errorf("%s_WINDOW must be a whole-microsecond duration from 1us through 24h", stem)
			}
			policy.Window = window
		}
		policies[scope] = policy
	}
	maxCaregivers, err := boundedInteger(getenv, "BT_MAX_CAREGIVERS", 10, 1, 100)
	if err != nil {
		return LimitsConfig{}, err
	}
	maxInvitations, err := boundedInteger(getenv, "BT_MAX_INVITATIONS", 10, 1, 1000)
	if err != nil {
		return LimitsConfig{}, err
	}
	invitationTTL := 7 * 24 * time.Hour
	if raw := getenv("BT_INVITATION_TTL"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || strings.TrimSpace(raw) != raw || parsed < time.Hour || parsed > 30*24*time.Hour || parsed%time.Second != 0 {
			return LimitsConfig{}, errors.New("BT_INVITATION_TTL must be a whole-second duration from 1h through 720h")
		}
		invitationTTL = parsed
	}
	return LimitsConfig{
		Policies: policies, MaxCaregivers: maxCaregivers,
		MaxInvitations: maxInvitations, InvitationTTL: invitationTTL,
	}, nil
}

func boundedInteger(getenv func(string) string, name string, fallback, minimum, maximum int) (int, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || strconv.Itoa(value) != raw || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be a canonical integer from %d through %d", name, minimum, maximum)
	}
	return value, nil
}
