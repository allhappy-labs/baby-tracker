package database

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

type netTimeout struct{}

func (netTimeout) Error() string   { return "i/o timeout" }
func (netTimeout) Timeout() bool   { return true }
func (netTimeout) Temporary() bool { return true }

// A check reaches no verdict only when the database could not be reached
// or did not answer in time; any SQLSTATE the server answered with outside
// the connection classes is a verdict. A lost connection is a broken or
// refused socket or an admin shutdown, never a slow answer.
func TestUnreachableAndConnectionLostClassifyTheCause(t *testing.T) {
	pg := func(code string) error { return &pgconn.PgError{Code: code, Message: "server text"} }
	for _, c := range []struct {
		name              string
		err               error
		unreachable, lost bool
	}{
		{"nil", nil, false, false},
		{"insufficient privilege", pg("42501"), false, false},
		{"undefined object", pg("42704"), false, false},
		{"undefined table", pg("42P01"), false, false},
		{"refused password", pg("28P01"), false, false},
		{"admin shutdown", pg("57P01"), true, true},
		{"crash shutdown", pg("57P02"), true, true},
		{"cannot connect now", pg("57P03"), true, true},
		{"statement timeout", pg("57014"), true, false},
		{"connection failure", pg("08006"), true, true},
		{"too many connections", pg("53300"), true, false},
		{"lock not available", pg("55P03"), true, false},
		{"serialization failure", pg("40001"), true, false},
		{"deadlock", pg("40P01"), true, false},
		{"deadline", context.DeadlineExceeded, true, false},
		{"cancelled", context.Canceled, true, false},
		{"network timeout", &net.OpError{Op: "read", Err: netTimeout{}}, true, false},
		{"refused socket", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true, true},
		{"reset socket", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, true, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true, true},
		{"closed conn", pgconn.ErrConnClosed, true, true},
		{"scan error", errors.New("can't scan into dest[0]"), false, false},
		{"wrapped privilege", fmt.Errorf("read applied migrations: %w", pg("42501")), false, false},
		{"wrapped shutdown", fmt.Errorf("acquire: %w", pg("57P01")), true, true},
		{"carried privilege", Unavailable(pg("42501")), false, false},
		{"carried deadline", Unavailable(context.DeadlineExceeded), true, false},
		{"bare unavailable", ErrUnavailable, false, false},
	} {
		if got := Unreachable(c.err); got != c.unreachable {
			t.Errorf("%s: Unreachable=%v want %v", c.name, got, c.unreachable)
		}
		if got := ConnectionLost(c.err); got != c.lost {
			t.Errorf("%s: ConnectionLost=%v want %v", c.name, got, c.lost)
		}
	}
	// A carried cause keeps the stable category text and identity.
	carried := Unavailable(pg("42501"))
	if !errors.Is(carried, ErrUnavailable) || carried.Error() != "database_unavailable" {
		t.Fatalf("carried cause: %v", carried)
	}
	if Unavailable(nil) != ErrUnavailable {
		t.Fatal("Unavailable(nil) is not ErrUnavailable")
	}
}
