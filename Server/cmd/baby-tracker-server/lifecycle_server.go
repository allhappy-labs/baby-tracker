package main

import (
	"context"
	"errors"
	"time"
)

func runServerLifecycle(ctx context.Context, runHTTP func(context.Context) error, shutdownLive func(context.Context) error) error {
	if runHTTP == nil || shutdownLive == nil {
		return errors.New("HTTP and live lifecycle functions are required")
	}
	httpErr := runHTTP(ctx)
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(httpErr, shutdownLive(shutdownContext))
}
