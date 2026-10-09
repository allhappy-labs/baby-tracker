package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5/pgxpool"
)

const NotificationChannel = "family_cursor_v1"

type CursorResyncer interface {
	Current(context.Context, AuthorizationLease) (protocol.Cursor, error)
}

type PostgresOptions struct {
	Pool          *pgxpool.Pool
	Hub           *Hub
	InstanceID    string
	RecoveryEpoch uint64
	Resyncer      CursorResyncer
	ResyncTimeout time.Duration
}

type PostgresListener struct {
	options   PostgresOptions
	ctx       context.Context
	cancel    context.CancelFunc
	ready     chan struct{}
	readyOnce sync.Once
	done      chan struct{}
}

func StartPostgresListener(parent context.Context, options PostgresOptions) (*PostgresListener, error) {
	if parent == nil || options.Pool == nil || options.Hub == nil || !protocol.ValidUUID(options.InstanceID) ||
		!protocol.ValidRecoveryEpoch(options.RecoveryEpoch) || options.Resyncer == nil {
		return nil, errors.New("invalid postgres live listener")
	}
	if options.ResyncTimeout == 0 {
		options.ResyncTimeout = 5 * time.Second
	}
	if options.ResyncTimeout < 0 || options.ResyncTimeout > 5*time.Second {
		return nil, errors.New("invalid postgres live resync timeout")
	}
	ctx, cancel := context.WithCancel(parent)
	listener := &PostgresListener{options: options, ctx: ctx, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{})}
	go listener.run()
	return listener, nil
}

func (listener *PostgresListener) WaitReady(ctx context.Context) error {
	select {
	case <-listener.ready:
		return nil
	case <-listener.done:
		return errors.New("postgres live listener stopped")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (listener *PostgresListener) Close() {
	if listener == nil {
		return
	}
	listener.cancel()
	<-listener.done
}

func (listener *PostgresListener) run() {
	defer close(listener.done)
	for listener.ctx.Err() == nil {
		connection, err := listener.options.Pool.Acquire(listener.ctx)
		if err != nil {
			if !listener.retry() {
				return
			}
			continue
		}
		_, err = connection.Exec(listener.ctx, `SET application_name='baby-tracker-live-listener'`)
		if err == nil {
			_, err = connection.Exec(listener.ctx, `LISTEN family_cursor_v1`)
		}
		if err != nil {
			closeDedicated(connection)
			if !listener.retry() {
				return
			}
			continue
		}
		listener.readyOnce.Do(func() { close(listener.ready) })
		listener.resync()
		for listener.ctx.Err() == nil {
			notification, waitErr := connection.Conn().WaitForNotification(listener.ctx)
			if waitErr != nil {
				break
			}
			hint, decodeErr := DecodeNotificationForFamily([]byte(notification.Payload), listener.options.InstanceID, listener.options.RecoveryEpoch, listener.options.Hub.HasRegisteredFamily)
			if decodeErr == nil {
				listener.options.Hub.Publish(listener.ctx, hint)
			}
		}
		closeDedicated(connection)
		if !listener.retry() {
			return
		}
	}
}

func closeDedicated(connection *pgxpool.Conn) {
	if connection == nil {
		return
	}
	raw := connection.Hijack()
	_ = raw.Close(context.Background())
}

func (listener *PostgresListener) retry() bool {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-listener.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (listener *PostgresListener) resync() {
	leasing := listener.options.Hub.RegisteredLeases()
	if len(leasing) == 0 {
		return
	}
	timeout := listener.options.ResyncTimeout
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(listener.ctx, timeout)
	defer cancel()
	workers := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for _, lease := range leasing {
		lease := lease
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case workers <- struct{}{}:
				defer func() { <-workers }()
			case <-ctx.Done():
				return
			}
			cursor, err := listener.options.Resyncer.Current(ctx, lease)
			if err == nil && cursor.FamilyID == lease.Grant.FamilyID {
				listener.options.Hub.Publish(ctx, Hint{FamilyID: cursor.FamilyID, Cursor: cursor})
			}
		}()
	}
	wait.Wait()
}

func DecodeNotification(payload []byte, instanceID string, recoveryEpoch uint64, registered map[string]struct{}) (Hint, error) {
	return DecodeNotificationForFamily(payload, instanceID, recoveryEpoch, func(familyID string) bool {
		_, ok := registered[familyID]
		return ok
	})
}

// DecodeNotificationForFamily validates the complete wire domain before looking up
// a single registered family. The lookup never reserves or binds a connection.
func DecodeNotificationForFamily(payload []byte, instanceID string, recoveryEpoch uint64, registered func(string) bool) (Hint, error) {
	if len(payload) == 0 || len(payload) > 1024 || !protocol.ValidUUID(instanceID) || !protocol.ValidRecoveryEpoch(recoveryEpoch) || registered == nil {
		return Hint{}, errors.New("invalid live notification")
	}
	var wire protocol.Hint
	if err := json.Unmarshal(payload, &wire); err != nil || wire.Cursor.ServerInstanceID != instanceID ||
		wire.Cursor.RecoveryEpoch != recoveryEpoch || wire.Cursor.FamilyID != wire.FamilyID {
		return Hint{}, errors.New("invalid live notification")
	}
	if !registered(wire.FamilyID) {
		return Hint{}, errors.New("foreign live notification")
	}
	return Hint{FamilyID: wire.FamilyID, Cursor: wire.Cursor}, nil
}
