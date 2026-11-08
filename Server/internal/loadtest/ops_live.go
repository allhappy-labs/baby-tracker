package loadtest

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"babytracker/server/internal/protocol"
)

// liveSubprotocol is the v1 live subprotocol (internal/realtime).
const liveSubprotocol = "baby-tracker.v1"

// Live socket close codes (internal/realtime).
const (
	closeNormal          = 1000
	closeGoingAway       = 1001
	closePolicyViolation = 1008
)

// renewalGrace is how long after its own refresh a device's old socket may
// be closed 1008 "authorization changed" by the server's authorization
// sweep without counting as a loss: the refresh consumed the session that
// socket was opened with, and the device leaves the sweep to retire it.
// The hub's slowest retirement (realtime/hub.go) is 20 s: a pass whose
// CheckMany read missed the refresh's commit can take its 5 s timeout, the
// next tick follows 5 s on, that pass takes up to 5 s more, and the close
// write is bounded by the 5 s write timeout. 30 s leaves 10 s for a loaded
// generator's and server's scheduling; a sweep 1008 later than that, or on
// a socket dialled after the latest refresh, is a loss.
const renewalGrace = 30 * time.Second

// sweepCloseReason is the reason the hub's authorization sweep closes a
// socket whose session is no longer active (realtime/hub.go).
const sweepCloseReason = "authorization changed"

// liveConn is one open live socket.
type liveConn struct {
	conn     *websocket.Conn
	dialedAt time.Time   // when its handshake was sent
	retired  atomic.Bool // closed by the generator: its close is never a loss
}

// dialResult is a dial loop's end: the socket opened; or another dial of
// the device (or its open socket) pre-empted this one; and how many
// handshakes failed on the way.
type dialResult struct {
	opened, busy bool
	failures     int
}

// iosBackoffWindow is the iOS reconnect window for attempt n:
// SyncRetryPolicy(base: 1, ceiling: 60) (FamilySyncWebSocket.swift:84),
// 0...min(2^(n-1), 60) seconds.
func iosBackoffWindow(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	seconds := math.Min(math.Pow(2, float64(min(attempt, 16)-1)), 60)
	return time.Duration(seconds * float64(time.Second))
}

// iosBackoff draws attempt n's delay uniformly over its window (full
// jitter).
func iosBackoff(attempt int, rng *lockedRand) time.Duration {
	return time.Duration(rng.Float64() * float64(iosBackoffWindow(attempt)))
}

// liveURL is the base URL's /v1/live as ws(s).
func (g *generator) liveURL() string {
	base := g.c.base
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	return base + "/v1/live"
}

// dial opens d's live socket (one live_handshake request) and starts its
// reader. It reports the handshake's result.
func (g *generator) dial(ctx context.Context, d *device, intended time.Time) result {
	dialedAt := time.Now()
	header := http.Header{}
	header.Set("X-Baby-Tracker-Protocol", "v1")
	header.Set("X-Baby-Tracker-Capabilities", strings.Join(capabilitiesLive, ","))
	if d.ClientIP != "" {
		header.Set("X-Forwarded-For", d.ClientIP)
	}
	// The handshake holds the device's session: it waits for a refresh in
	// flight and goes with the child's access.
	release, waited := d.holdSession()
	g.noteSessionWait(RouteLive, waited)
	intended = intended.Add(waited)
	header.Set("Authorization", "Bearer "+d.access())
	conn, response, err := websocket.Dial(ctx, g.liveURL(), &websocket.DialOptions{HTTPClient: g.wsHTTP, HTTPHeader: header,
		Host: g.cfg.Host, Subprotocols: []string{liveSubprotocol}})
	release()
	var res result
	switch {
	case err == nil:
		res.Outcome = OutcomeOK
	case response != nil && response.StatusCode != http.StatusSwitchingProtocols:
		res = readResponse(ctx, response, nil)
		if res.Outcome == OutcomeOK {
			res.Outcome = OutcomeProtocol
		}
	default:
		res.Outcome = transportOutcome(ctx, err)
	}
	g.record(RouteLive, intended, res.Outcome)
	if !res.ok() {
		if conn != nil {
			_ = conn.CloseNow()
		}
		return res
	}
	conn.SetReadLimit(64 << 10)
	lc := &liveConn{conn: conn, dialedAt: dialedAt}
	d.mu.Lock()
	if d.socket != nil || ctx.Err() != nil {
		d.mu.Unlock()
		lc.retired.Store(true)
		_ = conn.Close(closeNormal, "duplicate")
		return res
	}
	if !g.addReader() {
		d.mu.Unlock()
		lc.retired.Store(true)
		_ = conn.CloseNow()
		return res
	}
	d.socket = lc
	d.mu.Unlock()
	g.noteSocket(d, lc, time.Now())
	go g.readLive(d, lc)
	return res
}

// connectLive dials d until its socket is open or the run ends, waiting
// first, and between failed attempts by the iOS policy (a 429 waits at
// least its Retry-After). attempt is the iOS attempt count so far. Only
// one dial loop runs per device.
func (g *generator) connectLive(ctx context.Context, d *device, attempt int, first time.Duration) dialResult {
	var out dialResult
	d.mu.Lock()
	if d.dialing || d.socket != nil {
		d.mu.Unlock()
		out.busy = true
		return out
	}
	d.dialing = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.dialing = false
		d.mu.Unlock()
	}()
	wait := first
	for {
		if wait > 0 && !sleepFor(ctx, wait) {
			return out
		}
		if ctx.Err() != nil {
			return out
		}
		if g.liveOnly() && d.needsRefresh(time.Now()) && !g.refreshForDial(ctx, d) {
			// Live-only has no refresh schedule: the device refreshes a
			// lapsing token before it dials, and backs off when it cannot.
			out.failures++
			attempt++
			wait = max(iosBackoff(attempt, g.rng), time.Second)
			continue
		}
		if !g.liveOnly() {
			// A pending refresh attempt (lost in a restart window) or an
			// expired access is renewed before the dial. iOS gets there
			// through the sync it runs on every reconnect (liveConnected:
			// prepareSession renews first); the generator's reconnect runs
			// no pull, so it renews here, and backs off when it cannot.
			if _, ok := g.renewBeforeUse(ctx, d, time.Now()); !ok {
				out.failures++
				attempt++
				wait = max(iosBackoff(attempt, g.rng), time.Second)
				continue
			}
		}
		res := g.dial(ctx, d, time.Now())
		if res.ok() {
			out.opened = true
			return out
		}
		out.failures++
		if res.Outcome == OutcomeAccessExpired {
			g.expired(ctx, d)
		}
		attempt++
		wait = iosBackoff(attempt, g.rng)
		if res.RetryAfter > wait {
			wait = res.RetryAfter
		}
	}
}

// refreshForDial rotates d's tokens ahead of a dial (live-only runs): no
// socket is open, and the dial that follows goes with the child's token.
func (g *generator) refreshForDial(ctx context.Context, d *device) bool {
	d.mu.Lock()
	if d.refreshing {
		d.mu.Unlock()
		return false
	}
	d.refreshing = true
	d.mu.Unlock()
	replaced := g.rotate(ctx, d, time.Now())
	d.mu.Lock()
	d.refreshing = false
	d.mu.Unlock()
	return replaced
}

// readLive reads d's socket until it closes: every read keeps the server's
// pings answered; a hint of d's family schedules an immediate pull. Then
// the close is classified and the device reconnects.
func (g *generator) readLive(d *device, lc *liveConn) {
	defer g.liveWG.Done()
	var err error
	for {
		var data []byte
		_, data, err = lc.conn.Read(g.work)
		if err != nil {
			break
		}
		var hint protocol.Hint
		if decodeErr := hint.UnmarshalJSON(data); decodeErr != nil {
			g.record(RouteSyncHintPull, time.Now(), OutcomeProtocol)
			continue
		}
		if hint.FamilyID != d.FamilyID {
			g.record(RouteSyncHintPull, time.Now(), OutcomeCrossFamily)
			continue
		}
		g.noteHint(d, hint.Cursor.Sequence, time.Now())
		g.hintPull(d)
	}
	_ = lc.conn.CloseNow()
	g.forgetSocket(d, lc)
	d.mu.Lock()
	if d.socket == lc {
		d.socket = nil
	}
	refreshedAt, refreshing := d.refreshedAt, d.refreshing
	d.mu.Unlock()
	if lc.retired.Load() || g.work.Err() != nil {
		return
	}
	var closeErr websocket.CloseError
	code, reason := -1, ""
	if errors.As(err, &closeErr) {
		code, reason = int(closeErr.Code), closeErr.Reason
	}
	now := time.Now()
	switch classifyClose(code, reason, lc.dialedAt, refreshedAt, refreshing, now) {
	case closeRedialNow:
		g.renewals.Add(1)
		g.goBG(func() { g.connectLive(g.work, d, 0, 0) })
		return
	case closeRedialSoon:
		g.renewals.Add(1)
		g.goBG(func() { g.connectLive(g.work, d, 1, iosBackoff(1, g.rng)) })
		return
	}
	switch {
	case g.phases.declared(now):
	case now.Before(g.phases.windowStart):
		g.warmLosses.Add(1)
	case g.phases.inMeasure(now):
		g.liveLosses.Add(1)
		g.phases.lose(now)
		g.noteLiveLoss(LiveLoss{At: now.UTC(), Code: code, Reason: reason, UpSeconds: now.Sub(lc.dialedAt).Seconds(),
			SinceRefreshSeconds: sinceOrZero(now, refreshedAt), Refreshing: refreshing})
	}
	g.goBG(func() { g.connectLive(g.work, d, 1, iosBackoff(1, g.rng)) })
}

// closeKind is what a device does after the server closed its socket.
type closeKind int

const (
	closeLost       closeKind = iota // an unexpected loss (outside a declared window)
	closeRedialNow                   // a planned renewal, redialled at once
	closeRedialSoon                  // a planned renewal, redialled after the first backoff
)

func (k closeKind) String() string {
	return [...]string{"lost", "redial now", "redial soon"}[k]
}

// classifyClose judges a close as FamilySyncWebSocket.decide does:
//   - 1000 (the server sends it only at a socket's maximum lifetime: 15
//     minutes or its token's expiry) is redialled at once, attempts reset;
//   - 1001 (going away) after the first backoff, U(0, 1 s);
//   - the sweep's 1008 "authorization changed" on a socket dialled before
//     the device's latest refresh, within renewalGrace of it: that refresh
//     consumed the socket's session, and the device leaves the sweep to
//     retire it, as iOS does (it then backs off U(0, 1 s));
//   - the same 1008 while the device's own refresh is in flight: the server
//     committed it (the session is consumed) before its answer arrived, a
//     stall or a retry after server_busy; iOS's one HTTP sync waits for that
//     refresh, then backs off the same way;
//   - anything else (a 1008 on a socket dialled after the refresh, or too
//     late, or for another reason; a close without a frame) is a loss.
//
// The device never closes a socket itself to renew it: the hub frees a
// device's single slot before it sends any close, but frees it after a
// client's close only once it has read it, so a client close and an
// immediate redial could meet the old socket's slot (429).
func classifyClose(code int, reason string, dialedAt, refreshedAt time.Time, refreshing bool, now time.Time) closeKind {
	switch {
	case code == closePolicyViolation && reason == sweepCloseReason && refreshing:
		return closeRedialSoon
	case code == closeNormal && (reason == "" || reason == "maximum lifetime"):
		return closeRedialNow
	case code == closeGoingAway:
		return closeRedialSoon
	case code == closePolicyViolation && reason == sweepCloseReason && !refreshedAt.IsZero() &&
		dialedAt.Before(refreshedAt) && now.Sub(refreshedAt) < renewalGrace:
		return closeRedialSoon
	}
	return closeLost
}

// hintPull pulls d now (sync_hint_pull), coalescing hints that arrive
// while a hint pull is in flight into one more pull.
func (g *generator) hintPull(d *device) {
	d.mu.Lock()
	if d.hintPulling {
		d.hintPending = true
		d.mu.Unlock()
		return
	}
	d.hintPulling = true
	d.mu.Unlock()
	g.goBG(func() {
		for {
			g.pullPages(g.work, d, time.Now(), 0, RouteSyncHintPull)
			d.mu.Lock()
			if !d.hintPending {
				d.hintPulling = false
				d.mu.Unlock()
				return
			}
			d.hintPending = false
			d.mu.Unlock()
		}
	})
}

// retireSockets closes the given devices' sockets client-side at once
// (code), waiting for every close; it returns the devices it closed.
func (g *generator) retireSockets(devices []*device, code websocket.StatusCode, reason string) []*device {
	var closed []*device
	var wg sync.WaitGroup
	for _, d := range devices {
		d.mu.Lock()
		lc := d.socket
		d.socket = nil
		d.mu.Unlock()
		if lc == nil {
			continue
		}
		lc.retired.Store(true)
		closed = append(closed, d)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = lc.conn.Close(code, reason)
		}()
	}
	wg.Wait()
	return closed
}

// closeSockets ends every live socket at the end of the run.
func (g *generator) closeSockets() {
	g.retireSockets(g.devices, closeNormal, "run ended")
}

// ensureLive designates the first n devices of the live order as live and
// connects the ones that are not yet (after warm-up: a ramp step's growth),
// each within a second. A live-only step's new devices first refresh their
// lapsed tokens (two requests and a handshake each), so they are spread over
// liveOnlyRampIn (at most a quarter of the step) instead of arriving at
// once.
func (g *generator) ensureLive(n int) {
	n = min(n, len(g.liveOrder))
	spread := time.Second
	if g.liveOnly() {
		spread = g.liveRampIn()
	}
	for _, d := range g.liveOrder[:n] {
		if !d.live.Swap(true) {
			g.goBG(func() { g.connectLive(g.work, d, 0, g.rng.Duration(spread)) })
		}
	}
}

// liveOnlyRampIn is how long a live-only ramp step takes to open its new
// sockets.
const liveOnlyRampIn = 30 * time.Second

// liveRampIn is a live-only step's ramp-in: liveOnlyRampIn, at most a
// quarter of the step, at least a second.
func (g *generator) liveRampIn() time.Duration {
	return max(min(liveOnlyRampIn, g.cfg.StepDuration/4), time.Second)
}
