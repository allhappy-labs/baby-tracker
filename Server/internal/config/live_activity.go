package config

import (
	"errors"
	"fmt"
)

// LiveActivityConfig turns on Live Activity pushes (owner decision D1, main's
// live-activity-push-v1). They share the wake push's Apple credentials and
// its one APNs client.
type LiveActivityConfig struct {
	Enabled     bool
	Credentials APNSCredentials // the same BT_APNS_* as wakes
}

// The worker credential main's Live Activity worker logged in with. The port
// runs the worker on the maintenance pool, so a leftover setting is refused
// rather than silently ignored.
var retiredLiveActivityDatabaseSettings = []string{"BT_LIVE_ACTIVITY_DATABASE_URL_FILE", "BT_LIVE_ACTIVITY_DATABASE_URL"}

// LoadLiveActivity: BT_LIVE_ACTIVITY_PUSH_ENABLED "" or "false" is off (no
// BT_APNS_* variable is read); "true" requires LoadAPNSCredentials; anything
// else is an error. A set BT_LIVE_ACTIVITY_DATABASE_URL_FILE or
// BT_LIVE_ACTIVITY_DATABASE_URL is refused, on or off. Errors name the
// variable, never its value.
func LoadLiveActivity(getenv func(string) string) (LiveActivityConfig, error) {
	if getenv == nil {
		return LiveActivityConfig{}, errors.New("configuration getter is required")
	}
	for _, name := range retiredLiveActivityDatabaseSettings {
		if getenv(name) != "" {
			return LiveActivityConfig{}, fmt.Errorf("%s is not used: the Live Activity worker runs on the maintenance credential (BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE); remove the setting", name)
		}
	}
	switch getenv("BT_LIVE_ACTIVITY_PUSH_ENABLED") {
	case "", "false":
		return LiveActivityConfig{}, nil
	case "true":
	default:
		return LiveActivityConfig{}, errors.New("BT_LIVE_ACTIVITY_PUSH_ENABLED must be true or false")
	}
	credentials, err := LoadAPNSCredentials(getenv)
	if err != nil {
		return LiveActivityConfig{}, err
	}
	return LiveActivityConfig{Enabled: true, Credentials: credentials}, nil
}
