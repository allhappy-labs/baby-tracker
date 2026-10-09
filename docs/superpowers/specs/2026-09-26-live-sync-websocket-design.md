# Live family sync over WebSocket — design (2026-09-26)

User request (2026-09-26): a change on one phone reached the other after ~17 s ("too much, we should make the sync work better and faster. Consider websockets, SSE or similar"). Measured: each sync round trip is 200–400 ms; the delay is the other phone's 20 s foreground poll.

Verified 2026-09-26 against the deployed server through Tailscale Serve: `GET /v1/live` with upgrade headers and a bogus bearer returns `401 session_revoked` from the app (route reachable through Serve). A request with no token returns 503 (quirk of the missing-bearer code mapping, not a live outage). Serve negotiates HTTP/2 for ordinary requests — see risk R1.

# Near-real-time family sync: design

## 0. Summary

The server already provides everything this needs. `/v1/live` is a working, tested WebSocket hint channel. It sends one hint to every open socket in the family after every commit, including the writer's own socket, and it arrives within milliseconds of the Postgres commit. The iPhone app never opens that socket: `FamilySyncWebSocket` is a state machine with no networking. So the whole problem is on the client, plus one gap in the test setup: the e2e nginx config doesn't pass WebSocket upgrades.

With the socket in place, a change on phone A should show on phone B about 1.2–2 s later, or about 0.6–1.2 s if the push debounce drops to 300 ms. Today it's ~17 s.

---

## 1. Transport: use WebSocket on the existing `/v1/live`, not SSE

What the server already does (all verified in code):

| Aspect | Rule | Source |
|---|---|---|
| Route | `GET /v1/live`, WebSocket upgrade | `Server/internal/httpapi/router.go:176,235`, handler `Server/internal/httpapi/live.go:14-107` |
| Auth | **Bearer header only.** `auth.BearerToken(request)` (`live.go:24`). The session must be family-scoped (`live.go:34`). A query string or cookie is rejected with `invalid_request` (`live.go:85`). No first-message auth. | `docs/protocol/v1.md:225-230` |
| Headers | `X-Baby-Tracker-Protocol: v1`, and `X-Baby-Tracker-Capabilities` must be exactly `records-v1,live-hints-v1` | `live.go:38` → `sync.go:191-219`, `protocol/sync.go:41-42` |
| Subprotocol | Exactly one `Sec-WebSocket-Protocol: baby-tracker.v1` | `live.go:85-86`, `realtime/websocket.go:14` |
| Origin | May be left out (native client). If present, it must equal the canonical origin byte for byte. | `live.go:90-94` |
| Message | Text frame `{"family_id","cursor":{server_instance_id,recovery_epoch,family_id,sequence}}` | `realtime/hub.go:29-32`, `realtime/websocket.go:46-52`, `Server/api/fixtures/v1/live-hint.json` |
| Emission | Commit → `pg_notify('family_cursor_v1')` inside the commit tx (`delivery/observer.go:38`) → `PostgresListener` (`realtime/postgres.go:97-109`) → `Hub.Publish` sends to **every lease in the family, writer included** (`hub.go:319-363`). Hints are coalesced per socket, and a hint that is lower or equal to the last one sent is dropped (`hub.go:347-360`). When the listener reconnects, it resyncs the current cursor to every registered family (`postgres.go:137-168`). | |
| Client → server | Client data frames are not allowed. `CloseRead` handles control frames, and any data frame gets a policy close (`live.go:106`, test `live_test.go:144`). Inbound frames are capped at 1024 bytes (`websocket.go:41`). | |
| Keepalive | Server pings every 25 s. A missing pong within 5 s closes the socket with 1013 "slow consumer" (`hub.go:124,286-294`). | |
| Lifetime | The socket closes with 1000 "maximum lifetime" at whichever comes first: 15 min, or the access-token expiry (`hub.go:125,189-192,262-285`). Access tokens last 10 min (`auth/sessions.go:18`), so **a socket lives at most ~10 min**. | |
| Security | An invalidation or the 5 s authorization sweep closes the socket with 1008 (`hub.go:369-387,427-462`). An epoch change also gives 1008 (`hub.go:337-341`). Server shutdown gives 1001 (`hub.go:557`). | |
| Limits | `SocketDeviceLimit` **1**, account 3, process 1000 (`protocol/sync.go:136`, env `BT_SOCKET_DEVICE_LIMIT` range 1–5 in `config/sync.go:57`). Connection attempts are limited to 12/min per device, 36 per account, 120 per family, 60 per IP. Going over capacity returns 429 with `Retry-After: 5` (`live.go:77-78`). | |
| Upgrade HTTP errors | 401 covers `access_expired`, `session_revoked`, `reauthentication_required` and `epoch_changed` (`httpapi/sync.go` `writeSyncFamilyCode`). 403 means role, membership or entitlement. 429 means rate or capacity. 503 means the service is unavailable. 400 means bad headers, subprotocol, query or origin. | |
| Server timeouts | `http.Server` has `ReadTimeout` 15 s and `WriteTimeout` 30 s (`cmd/baby-tracker-server/main.go:180-190`). These **do not** kill the socket: the Go runtime clears them on hijack (`net/http/server.go:325` `rwc.SetDeadline(time.Time{})`). | |

**Why not SSE:** the server would need a new route, new tests, a new fixture and a protocol-doc change. It would also still need the same keepalive and lifetime handling, and it would buy nothing over an already-audited WebSocket. It could also buffer in proxies.

**Server code changes: none required.** Recommended changes to config and tests:

- **Deploy config, recommended:** set `BT_SOCKET_DEVICE_LIMIT=2` in `Server/deploy/unraid/compose.yaml`. With the limit at 1, a socket that dies without a clean close (for example on a Wi-Fi to cellular switch) holds the device's only slot for up to ~30 s: 25 s ping plus 5 s write timeout. Every reconnect in that window gets a 429.
- **Test setup, required:** `Server/scripts/protocol-e2e.sh:107-117` doesn't pass the upgrade through nginx. Add a `map $http_upgrade $connection_upgrade { default upgrade; '' close; }` block (conf.d is in the http context), plus `proxy_http_version 1.1; proxy_set_header Upgrade $http_upgrade; proxy_set_header Connection $connection_upgrade;`.
- **Tailscale Serve:** `Server/deploy/unraid/tailscale/serve.json` is a plain `"Proxy": "http://app:8080"` on `tailscale/tailscale:v1.98.3`. Serve's proxy is built on Go's `httputil.ReverseProxy`, which forwards HTTP/1.1 `Upgrade`, so the upgrade is **expected to pass. This hasn't been verified against this deployment.** Nothing in `verify-unraid-deployment.sh` exercises `/v1/live`. See risk R1 on HTTP/2 extended CONNECT. Before relying on this, run the existing Go dialer (`cmd/protocol-e2e-client/main.go:609-622` `client.live`) once against `https://baby-sync.example-tailnet.ts.net`, and add that as a step in `verify-unraid-deployment.sh`.

---

## 2. Client design

### 2.1 Where the socket lives: inside the engine

`FamilySyncEngine` already has the right lifecycle:

- It starts in the foreground: `startForegroundPolling()` (`FamilySyncEngine.swift:249-259`) is called by `FamilySettingsViewModel.startSyncing` (`FamilySettingsViewModel.swift:333`), `sceneBecameActive` (`:347`) and `buildEngineForCurrentSession` when foreground (`:421`).
- It stops on background through `stopForegroundPolling()` (`:261-264`), called by `sceneEnteredBackground` (`VM:361`).
- It stops for good, before any purge, through `stop()` (`:273-283`), called by `stopEngine` (`VM:467`) and `withSyncStopped` (`VM:486`). That covers leave, disconnect, sign-out, key change and relaunch.

So the socket goes inside the engine and follows the same calls. `FamilySyncDriving` (`FamilySyncComposition.swift:124-135`), the view model and `RootView` don't change.

### 2.2 New and changed types, all in `BabySync`

**a) Seam for the network (new file `LiveSocket.swift`):**
```swift
protocol LiveSocketConnecting: Sendable {
    /// Returns once the 101 is received (proved by a first pong), or throws LiveSocketError.
    func open(token: String) async throws -> any LiveSocket
}
protocol LiveSocket: Sendable {
    func receive() async throws -> Data          // text frames only; binary = protocol error
    func ping() async throws                     // resolves on pong
    func close(_ code: URLSessionWebSocketTask.CloseCode)
}
enum LiveSocketError: Error, Equatable {
    case unauthorized                 // 401
    case forbidden                    // 403
    case rateLimited(TimeInterval)    // 429 + Retry-After (default 5)
    case unsupported                  // 400/404/426 – don't retry this engine lifetime
    case unavailable                  // 503/5xx
    case closed(code: Int, reason: String)
    case network
}
```

**b) `URLSessionLiveSocketConnector`.** Built in `FamilySyncComposition.assemble` (`FamilySyncComposition.swift:89-102`), reusing the same `session` and `tokens` as `FamilySyncHTTPClient`:

- URL: `endpoint.origin.canonicalString` with `https` changed to `wss`, plus `/v1/live`. No query string.
- `URLRequest` settings:
  - `Authorization: Bearer <token>`
  - `X-Baby-Tracker-Protocol: v1`
  - `X-Baby-Tracker-Capabilities: FamilySyncWebSocket.capabilities.joined(",")` (`FamilySyncWebSocket.swift:100`)
  - `Sec-WebSocket-Protocol: baby-tracker.v1` (`:99`)
  - `httpShouldHandleCookies = false`
  - No `Origin` header.
  - These mirror `FamilySyncHTTPClient.send` (`FamilySyncHTTPClient.swift:43-53`).
- `task = session.webSocketTask(with: request)`, `task.maximumMessageSize = 4096`, `resume()`.
- **Open detection without a delegate:** `await withCheckedThrowingContinuation { task.sendPing(pongReceiveHandler:) }`. A pong only comes back after a successful 101. On failure, map `(task.response as? HTTPURLResponse)?.statusCode` (and `Retry-After`) to `LiveSocketError`.
  - A per-task delegate isn't needed. Server trust still flows through the session-level challenge, so the e2e `PinnedCertificateDelegate` (`Tests/FamilySyncClientE2ETests/PinnedSession.swift:70-107`) keeps working.
- `receive()`: a `.string` or `.data` message becomes `Data`.
- On error, read `task.closeCode` and `task.closeReason` and throw `.closed(code, reason)`. Otherwise throw `.network`.
- `close`: `task.cancel(with: .normalClosure, reason: nil)`.

**c) `FamilySyncWebSocket` (`FamilySyncWebSocket.swift:25-101`).** Keep the actor as the state machine and add a runner loop to it. Changes:

- New initializer inputs: `connector`, `tokens: SessionTokenProviding`, `sleep: @Sendable (TimeInterval) async throws -> Void` (injectable for tests), and an `onStateChange` callback.
- `onSyncRequested` changes to `onHint: (Cursor) async -> Void`.
- New `run()`, supervised by one `Task` that `start()` creates and `stop()` cancels:
  ```
  while state != .stopped:
    token = await tokens.currentAccessToken()  (nil → stop; no session)
    do socket = try await connector.open(token)
       markConnected()           // existing :60-66 → onConnected → engine.syncNow(.liveHint)
       spawn pinger: every 30 s socket.ping() with 10 s timeout; failure → socket.close + throw
       loop: data = try await socket.receive(); decode LiveHint; handle(hint)   // existing :91-96
    catch → decide(error) → delay; sleep(delay)
  ```
- Error handling in `decide(error)`:

  | Error | Action |
  |---|---|
  | `.closed(1000, _)` (lifetime) | Reconnect **immediately**. Don't count it as a failure. |
  | `.unauthorized` | Call `onUnauthorized()`. This is `engine.syncNow(.liveHint)`, whose HTTP 401 path already renews through `renewSession` (`FamilySyncEngine.swift:95,388-391`) and puts fresh-proof or revocation on the status line (`SyncRetryPolicy.swift:57-95`). If the engine status afterwards is `.needsSignIn`, go to `.stopped` until the next foreground. Otherwise count a failure and back off. |
  | `.closed(1008, _)` (authorization or epoch changed), `.forbidden` | Call `onUnauthorized()`, i.e. one HTTP sync. That surfaces `membership_revoked` → `revokeAndPurge`, `epoch_changed` → snapshot, and so on. Then back off. |
  | `.rateLimited(t)` | Sleep `max(t, backoff)`. |
  | `.unsupported` | Go to `.stopped` for this engine's lifetime. Polling stays at 20 s. |
  | `.closed(1013/1001)`, `.unavailable`, `.network`, ping failure | Back off using the existing `handleDisconnect()` (`:77-82`), which uses `SyncRetryPolicy(base:1, ceiling:60)` with full jitter (`SyncRetryPolicy.swift:126-135`). |

- **Fix, reset the backoff counter later:** only reset `reconnectAttempt` once a socket has stayed up for 30 s, or when a close is code 1000. Today it resets in `markConnected` (`:62`). With that, a socket that opens and immediately gets 1008 cycles at about 1/s until it hits the 12/min device limit.
- **Fix, bound the hint log:** replace `observedHints` (`:31,93`) with `highestHintSequence: Int64?`. The array grows without limit.
- `start(realm:serverInfo:)` (`:50-58`) stays as the capability gate. The `ServerInfo` comes from one `GET /v1/server` per engine start via `HTTPIdentityClient.serverInfo(_:)` (`HTTPIdentityClient.swift:101`). Check `serverInstanceID == endpoint.serverInstanceID` and `advertisesLiveHints` (`EndpointIdentity.swift:68`). A probe failure counts as a backoff, not as unsupported.

**d) `FamilySyncEngine` changes (`FamilySyncEngine.swift`):**

1. New optional init parameter `live: FamilySyncWebSocket?` in the internal init (`:73-97`), and a `liveFactory` in the convenience init (`:58-70`). Tests pass `nil` or a fake.
2. `startForegroundPolling()` (`:249`) also calls `live.start(realm: familyKey.realmID, …)`.
   - `stopForegroundPolling()` (`:261`) also calls `live.stop()`, which closes with 1000. That frees the server's single device slot right away.
   - `stop()` (`:273`) calls `live.stop()` and awaits the runner, **before** the loop over `running`, so no hint can queue a sync after stop. `syncNow`'s `guard !stopped` (`:188`) is the second line of defence.
3. **Poll interval depends on socket state.** Keep the polling `Task` alive in both cases, because `isInForeground` is defined as `polling != nil` (`:325`) and it gates remote-timer Live Activity cards (`:341`). On `live` state change, cancel and restart the polling task with the new interval:
   - Socket `.connected`: **300 s** safety poll (reuse `SyncScheduler.pollingInterval`, `BackgroundSync.swift:51`). Hints can be lost (`docs/protocol/v1.md:234`). The socket also reconnects and syncs at least every ~10 min.
   - `.connecting`, `.backingOff` or `.stopped` because unsupported: **20 s**, as today (`defaultPollInterval`, `:29`).
   - Update the doc comment on `SyncScheduler.needsPolling` (`BackgroundSync.swift:59-63`). It currently says a live server needs no polling at all.
4. **Hint coalescing and skipping the writer's own hint.** Add `liveHintArrived(_ cursor: Cursor)`:
   - If `store.syncCursor(in: familyKey.realmID)` (`BabyStore+RemoteApply.swift:78`) has the same instance, epoch and family and `sequence >= cursor.sequence`, return. The hint is already covered.
   - If a sync is `running`: raise `pendingHintSequence` to the max, and set `followUp = .liveHint` **only if `followUp == nil`**. A queued `.localWrite` or `.foreground` follow-up must never be overwritten, and it pulls anyway (`:189-192`).
   - Otherwise call `syncNow(.liveHint)` (`:187-195`).
   - In the follow-up loop in `exclusively` (`:216-221`), skip a `.liveHint` follow-up when the local cursor now covers `pendingHintSequence` and `nextDispatchBatch(limit: 1)` is empty.
   - Result: phone A's own hint, which usually arrives *while A's POST is in flight* (the NOTIFY fires at commit, before the response), no longer causes a second request. A burst of N hints costs at most one extra pass.
5. Internal `var liveState` (forwarded) and `var lastSyncReason: SyncReason? { coordinator.lastReason }` (`SyncCoordinator.swift:101`), for tests.

**e) Composition (`FamilySyncComposition.swift:89-102`).** In `makeEngine`, build `FamilySyncWebSocket(connector: URLSessionLiveSocketConnector(endpoint:, session:), tokens: tokens, …)`. Add an internal-only `assemble(..., foregroundPollInterval: Duration? = nil)` parameter so the e2e test can turn polling off.

### 2.3 Token refresh

The access token lasts 10 min and the socket is capped at token expiry, so the socket closes with 1000 about every ≤10 min.

- **Minimal path:** reconnect right away. The stale token gets a 401. `onUnauthorized` then runs an HTTP sync, which renews through the existing path. Then the socket reconnects. That's about 3 round trips (~1 s) of hint gap every 10 min, covered by the reconnect sync, and 2 of the 12/min allowed attempts.
- **Optional improvement:** add `currentAccessTokenExpiry()` to `KeychainTokenProvider` (`DeferredFamilyClient.swift:59-94`), reading `SessionCredentials.expiresAt`. If less than 60 s remain, call the engine's renew before dialing. Not required for a first version.

### 2.4 Ping and pong

- **Server → client:** `URLSessionWebSocketTask` answers the server's 25 s pings automatically.
- **Client → server:** send a ping every 30 s and treat a 10 s timeout as dead. This catches half-open TCP after a network change much sooner than `receive()` would. Pings are control frames, which the server allows.

---

## 3. Latency

**Expected path, B in the foreground with the socket up:**

| Step | Time |
|---|---|
| A: debounce | 1000 ms (`defaultDebounce`, `FamilySyncEngine.swift:26`) |
| A: POST /v1/sync + commit | 200–400 ms. The NOTIFY fires at commit, before A's response. |
| Postgres LISTEN → hub → Tailscale → B | ~10–50 ms |
| B: `syncNow(.liveHint)` POST + apply | 200–400 ms |
| **Total** | **~1.2–1.9 s** (today ~17 s) |

**Recommendation: cut the debounce from 1 s to 300 ms.** It saves 0.7 s, which is now the biggest single item. The outbox batches on its own (`SyncCoordinator.swift:135-153`), so a burst that splits into two POSTs is harmless. And `syncNow` joins a running sync as a follow-up (`:189-192`), so two syncs never run at once. That brings the total to **~0.6–1.2 s**.

**How to measure:**

- **e2e, one process, one clock (exact):** record `t0` just before the owner's write and push. Poll the joiner's `history()` every 25 ms until the record appears, giving `t1`. Print `t1 - t0` in the proof line.
- **Real phones:**
  - Add `AppLog.sync` lines (privacy-safe, sequence numbers only): on A, `push acked seq=N at <wall>`; on B, `live hint seq=N at <wall>` and `applied through seq=N at <wall>`.
  - Compare timestamps in Console. NTP skew is about 10–50 ms, which is fine at this scale.
  - For a manual check, also time "tap on A" to "row visible on B" over 10 trials and report the median and p90.

---

## 4. Phone B in the background

- `sceneEnteredBackground` → `stopForegroundPolling` → the socket closes cleanly and polling stops. The app is suspended, so B gets **no updates while backgrounded**, apart from the opportunistic `BGAppRefresh` via `BackgroundSync.runOnce` (`BackgroundSync.swift:34-40`), which is unchanged.
- When B comes back: `sceneBecameActive` → `syncNow(.foreground)` (`VM:347-359`), and the socket reconnects, whose `markConnected` also syncs. The redundant second sync merges into a follow-up or is skipped by the cursor check. B is current about 0.5 s after unlocking.
- **APNs is out of scope.** The server already records durable wake intents (`delivery_pending`, `docs/protocol/v1.md:240-242`), but it has no APNs client (`docs/verification/family-sync-protocol.md:37`). So the work isn't trivial: it needs an APNs sender plus a device-token route.
- **Live Activity:** while B is in the foreground, a timer A starts now shows on B's Lock Screen and Dynamic Island about 1–2 s later, not up to 20 s. `announceRemoteTimers` still only starts cards while `isInForeground` (`FamilySyncEngine.swift:337-350`), which is the accepted limitation. Keeping the polling task alive (2.2d point 3) is what keeps that flag true.

---

## 5. Tests

**Unit tests, `Tests/BabySyncTests/FamilySyncWebSocketTests.swift`** (extends the existing suite at `:25-130`). Use a `FakeLiveSocketConnector`: a scripted queue of outcomes, sockets backed by an `AsyncThrowingStream`, a record of tokens and close codes, and injected `sleep` that records delays.

1. The request builder produces a `wss://…/v1/live` URL with no query string, exactly one `Sec-WebSocket-Protocol: baby-tracker.v1`, exact protocol and capability headers, a Bearer token, no Origin, and cookies off.
2. `LiveHint` decodes `Server/api/fixtures/v1/live-hint.json` byte for byte. No Swift test reads this fixture today.
3. A successful open causes exactly one `.liveHint` sync. A hint for this family causes one; a hint for another family causes none (existing tests, adapted).
4. A hint whose sequence is at or below the local cursor causes no sync.
5. Five hints during a running sync cause at most one follow-up pass.
6. A hint arriving while a `.localWrite` follow-up is queued doesn't replace it, and the mutation is still sent.
7. **Writer's own hint:** the hint arrives while A's own push is in flight and is covered by the post-push cursor. Result: A's transport records exactly 1 `sync` call.
8. 401 on upgrade → one engine sync (renew called once via the fake transport's `access_expired`) → reconnect with the *new* token. If the renew leaves the engine `.needsSignIn`, nothing more is dialed.
9. 429 with `Retry-After: 7` → the recorded sleep is at least 7 s.
10. 400 or 404 → `.stopped`, no more dials, poll interval stays 20 s.
11. Close 1000 → immediate redial with no sleep and no increase in the attempt count. Close 1008 → one sync, then backoff. Close 1013 → backoff grows.
12. Ping timeout → socket closed and a redial.
13. An open-then-immediate-close loop grows the backoff: the counter doesn't reset until the socket has stayed up 30 s.
14. `stopForegroundPolling()` closes with `.normalClosure`, and `stop()` finishes the runner. A hint delivered after `stop()` causes no sync.

**Unit tests, `FamilySyncEngineTests.swift`** (next to `foregroundPollingSyncsUntilStopped`, `:837`):

15. The poll interval switches from 20 s to 300 s on connect and back on disconnect, and `isInForeground` stays true throughout.
16. A remote timer that appears through a hint-triggered sync still calls `announcer.started` (fake announcer).

**Go tests:** none are needed, because the server doesn't change. Optional: a `realtime/hub_test.go` case that states the rule the client relies on, namely that the writer's own lease also receives the hint.

**e2e, new step 15 in `TwoCaregiverSyncE2ETests.swift`** (after step 14 at `:283-298`; `protocol-e2e.sh:261` then checks `step 15`):

- Build both phones through `assemble(..., foregroundPollInterval: .seconds(3600))`, so polling is effectively off.
- Call `joiner.model.sceneBecameActive()`, then wait (at most 5 s) for `engine.liveState == .connected`.
- The owner logs a diaper and calls `owner.sync(.localWrite)` directly, so the test doesn't depend on the debounce. Take `t0` there.
- Poll the joiner's history until the diaper appears. **Assert the time from `t0` is under 3 s.**
- **How "without polling" is enforced:**
  - The poll interval is 1 h.
  - The joiner's `engine.lastSyncReason == .liveHint`.
  - The joiner's HTTP sync count went up by exactly 1 or 2: the hint sync, plus possibly the reconnect sync.
- Then `joiner.model.sceneEnteredBackground()`: assert the state is `.stopped`, and a fresh `sceneBecameActive()` connects **with no 429**. That proves the clean close freed the single device slot.
- Print `two-caregiver proof: step 15: live hint delivered owner's diaper in N ms, joiner syncs by reason=[…]`.
- **Prerequisite:** the nginx upgrade headers from section 1.

**Deployment smoke:** add a `/v1/live` dial through Tailscale Serve to `verify-unraid-deployment.sh`, reusing `protocol-e2e-client`'s `live()` (`main.go:609`).

---

## 6. Risks

- **R1, proxies.**
  - Tailscale Serve probably passes HTTP/1.1 upgrades, but this is unverified here.
  - If Serve's TLS front negotiates h2 *and* advertises extended CONNECT (RFC 8441), `URLSessionWebSocketTask` may try WebSocket over HTTP/2, which `ReverseProxy` can't forward. That would show up as a failed open → our client treats it as backoff, then after repeated 400/404 as unsupported. Polling at 20 s still works, so the failure mode is safe. **Verify on a real phone before shipping.**
  - The e2e nginx doesn't enable h2, so it won't catch this.
- **R2, single device slot.** With `SocketDeviceLimit=1`, a dead socket that wasn't closed cleanly blocks reconnects with 429 for up to ~30 s. Mitigations: set `BT_SOCKET_DEVICE_LIMIT=2`, close explicitly on background, and honor `Retry-After`.
- **R3, reconnect storms.** Covered by jittered backoff (1–60 s), the counter reset only after 30 s up, the server's 12/min device limit, and `unsupported` stopping dials. Both phones reconnect after a server restart (1001), but jitter spreads them out.
- **R4, battery.** One idle TLS socket in the foreground only, a 25 s server ping plus a 30 s client ping (small frames), and polling drops from every 20 s to every 300 s. Radio use in the foreground probably goes *down*. Nothing runs in the background.
- **R5, duplicate syncs.** Sources are the hint plus the reconnect sync plus the writer's own hint. Handled by the cursor check and `syncNow`'s follow-up merge. Correctness doesn't depend on any of this, since hints carry no state (`FamilySyncWebSocket.swift:5-9`).
- **R6, token churn.** A 1000 close every ≤10 min, then 401, then renew. That's a small gap covered by the reconnect sync. The optional expiry-aware pre-refresh (2.3) removes it.
- **R7, stop ordering.** A hint landing during `stop()` must not start work against a realm about to be purged. The runner is cancelled before the engine drains `running`, and `syncNow` and `exclusively` re-check `stopped` (`FamilySyncEngine.swift:188,210`).
- **R8, Origin header.** If some iOS version adds an `Origin` to WebSocket upgrades, the server rejects it (`live.go:90-94`). The e2e step catches this.

### Critical Files for Implementation
- <repo>/Packages/BabyTracker/Sources/BabySync/FamilySyncWebSocket.swift
- <repo>/Packages/BabyTracker/Sources/BabySync/FamilySyncEngine.swift
- <repo>/Packages/BabyTracker/Sources/SettingsFeature/FamilySyncComposition.swift
- <repo>/Packages/BabyTracker/Tests/FamilySyncClientE2ETests/TwoCaregiverSyncE2ETests.swift
- <repo>/Server/scripts/protocol-e2e.sh (nginx upgrade headers); reference only: <repo>/Server/internal/httpapi/live.go, <repo>/Server/internal/realtime/hub.go