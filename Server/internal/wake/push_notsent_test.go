package wake_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/config"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/wake"
)

// A push the real client never put on the wire (Apple's host did not
// resolve, or the dial was refused) was not sent: not counted as a send,
// and its sequence is owed again, so the device is pushed for that write
// when its window ends, with no further write (final review R2-I2). Before
// the fix such a failure was Ambiguous and the device never got the push.
func TestAPushThatNeverReachedTheWireIsOwed(t *testing.T) {
	for _, c := range []struct {
		name string
		dial error
	}{
		{"an unresolvable host", &net.DNSError{Err: "no such host", Name: "api.push.apple.com", IsNotFound: true}},
		{"a refused connection", &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := testkit.New(t)
			caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
			var requests atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			t.Cleanup(server.Close)
			var down atomic.Bool
			down.Store(true)
			transport := server.Client().Transport.(*http.Transport).Clone()
			dialer := &net.Dialer{}
			transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				if down.Load() {
					return nil, c.dial
				}
				return dialer.DialContext(ctx, network, server.Listener.Addr().String())
			}
			clock := h.Now
			client, err := apns.NewWithTransport(throwawayCredentials(t), transport, server.URL, server.URL, func() time.Time { return clock })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.CloseIdleConnections)
			tracker := wake.NewTracker(0)
			w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: client,
				Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
			device := backgrounded(t, h, tracker, token(0x77), clock)
			pushTick(t, w)
			before := pushRows(t, h, h.FamilyID)[device.DeviceID]
			write(t, h, tracker, 1)
			sentAt := clock
			if stats := pushTick(t, w); stats.Pushed != 0 || stats.PushFailed != 1 {
				t.Fatalf("%+v, want no send made and one failed", stats)
			}
			if totals := w.Totals(); totals.NotSent != 1 || totals.Ambiguous != 0 || totals.PushFailed != 1 || totals.Pushed != 0 {
				t.Fatalf("totals %+v, want one not sent", totals)
			}
			if row := pushRows(t, h, h.FamilyID)[device.DeviceID]; row.Pushed != before.Pushed || row.PushedAt == nil || !row.PushedAt.Equal(sentAt) {
				t.Fatalf("row %+v, want the sequence given back (%d) and the time kept", row, before.Pushed)
			}
			down.Store(false)
			clock = sentAt.Add(wake.APNsInterval - time.Second)
			pushTick(t, w)
			if requests.Load() != 0 {
				t.Fatal("pushed again inside the window")
			}
			clock = sentAt.Add(wake.APNsInterval)
			if stats := pushTick(t, w); stats.Pushed != 1 || requests.Load() != 1 {
				t.Fatalf("%+v with %d requests, want the owed push sent when the window ends", stats, requests.Load())
			}
		})
	}
}

// throwawayCredentials are a generated key and made-up identifiers.
func throwawayCredentials(t *testing.T) config.APNSCredentials {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	path := filepath.Join(t.TempDir(), "apns.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.APNSCredentials{TeamID: "TEAM123456", KeyID: "KEY1234567", KeyFile: path, BundleID: "com.example.babytracker"}
}
