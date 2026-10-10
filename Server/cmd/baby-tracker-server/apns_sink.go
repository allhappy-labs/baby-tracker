//go:build !identitytest

package main

import (
	"babytracker/server/internal/apns"
	"babytracker/server/internal/config"
)

// newAPNSClient is the server's one Apple push client, for wakes, Live
// Activities and the apns-preflight command. Production builds have exactly
// one: Apple's two hosts.
func newAPNSClient(creds config.APNSCredentials) (*apns.Client, error) {
	return apns.New(creds)
}
