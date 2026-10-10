package database

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
)

// unavailableCause is ErrUnavailable carrying the database error behind it:
// its text stays the stable category (no database text reaches an operator
// or a log line through it), while Unreachable and ConnectionLost can still
// classify the cause.
type unavailableCause struct{ cause error }

func (e unavailableCause) Error() string   { return ErrUnavailable.Error() }
func (e unavailableCause) Unwrap() []error { return []error{ErrUnavailable, e.cause} }

// Unavailable is ErrUnavailable carrying cause for classification.
func Unavailable(cause error) error {
	if cause == nil {
		return ErrUnavailable
	}
	return unavailableCause{cause}
}

// Unreachable reports whether err says the database could not be reached
// or did not answer in time, so a check that failed with it reached no
// verdict: a connection that could not be made or broke, a deadline or
// cancellation, or a connection-class SQLSTATE (08 connection exception, 53
// insufficient resources, 57 operator intervention, 55P03 lock not
// available, 40001 serialization failure, 40P01 deadlock). Any other
// SQLSTATE the server answered with (42501 insufficient_privilege, 42704
// undefined_object, 28P01 a refused password, ...) is a verdict, and so is
// an error that carries no cause at all.
func Unreachable(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		code := pgErr.Code
		return strings.HasPrefix(code, "08") || strings.HasPrefix(code, "53") || strings.HasPrefix(code, "57") ||
			code == "55P03" || code == "40001" || code == "40P01"
	}
	return timedOut(err) || transportFailure(err)
}

// ConnectionLost reports whether err says a connection broke or could not
// be made (the server went away, restarted or refused the socket), as
// opposed to a slow answer or a refusal the server gave: pooled connections
// may be dead after it. A timeout is not a lost connection.
func ConnectionLost(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		code := pgErr.Code
		return strings.HasPrefix(code, "08") || code == "57P01" || code == "57P02" || code == "57P03"
	}
	return !timedOut(err) && transportFailure(err)
}

// timedOut is a deadline, a cancellation or a network timeout.
func timedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || pgconn.Timeout(err) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// transportFailure is a connection that could not be made or broke below
// the protocol, with no SQLSTATE and no timeout.
func transportFailure(err error) bool {
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	if errors.Is(err, pgconn.ErrConnClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && !netErr.Timeout()
}
