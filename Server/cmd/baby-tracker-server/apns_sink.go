//go:build !identitytest

package main

import (
	"babytracker/server/internal/apns"
	"babytracker/server/internal/config"
	"babytracker/server/internal/wake"
)

// newAPNSClient is the server's one Apple push client, for wakes, Live
// Activities and the apns-preflight command. Production builds have exactly
// one: Apple's two hosts.
func newAPNSClient(creds config.APNSCredentials) (*apns.Client, error) {
	return apns.New(creds)
}

// identityTestWakeWorker is a no-op in the release build, which serves no
// identitytest routes.
func identityTestWakeWorker(*wake.Worker) {}
