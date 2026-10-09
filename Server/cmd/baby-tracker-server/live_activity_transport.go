//go:build !identitytest

package main

import (
	"babytracker/server/internal/config"
	"babytracker/server/internal/liveactivity"
)

// Production builds have exactly one transport: Apple's two hosts.
func newLiveActivityTransport(cfg config.LiveActivityConfig) (liveActivityTransport, error) {
	transport, err := liveactivity.NewAPNSTransport(cfg)
	if err != nil {
		return nil, err
	}
	return transport, nil
}
