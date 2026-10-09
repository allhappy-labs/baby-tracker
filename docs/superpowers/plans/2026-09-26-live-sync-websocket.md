# Live Family Sync over WebSocket Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A change on one caregiver's phone appears on the other (app in foreground) within ~1–2 s instead of ~17 s, by connecting the app to the server's existing `/v1/live` WebSocket hint channel.

**Architecture:** No server code change. The app gets a real `URLSessionWebSocketTask` transport behind the existing `FamilySyncWebSocket` state machine, owned by `FamilySyncEngine` and following its foreground/background/stop lifecycle; a hint triggers a coalesced `syncNow(.liveHint)`; polling drops to a 300 s safety net while the socket is connected; the push debounce drops to 300 ms. The e2e harness's nginx passes WebSocket upgrades and a new step proves delivery under 3 s without polling.

**Tech Stack:** Swift 6.2, URLSessionWebSocketTask, Swift Testing; Go server unchanged; Docker e2e harness; Unraid compose config.

> **Status (2026-09-26):** merged to main at `f186224`, server redeployed, installed on both phones, sockets proven through Tailscale Serve. Evidence: `docs/verification/2026-09-26-live-sync.md`.

**Spec:** `docs/superpowers/specs/2026-09-26-live-sync-websocket-design.md` (binding; server rules, file:line references, client error table, tests and risks are all there).

## Global Constraints
- Server rules (verified in code): Bearer header only, family-scoped session; headers `X-Baby-Tracker-Protocol: v1`, `X-Baby-Tracker-Capabilities: records-v1,live-hints-v1`; exactly one `Sec-WebSocket-Protocol: baby-tracker.v1`; no Origin header; no query string; client data frames are forbidden; server ping 25 s; socket lifetime ≤ access-token expiry (10 min) → close 1000.
- Hints carry no state; correctness never depends on them. Polling stays as the fallback: 300 s while connected, 20 s otherwise.
- `isInForeground` (polling task alive) must stay true while the app is foreground — it gates remote-timer Live Activity cards.
- Engine `stop()` must end the socket runner before draining work; no hint may start a sync after stop.
- Push debounce 1 s → 300 ms.
- `EXPECTED_TESTS` updated; copy in `*Copy` enums; signed commits with the `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>` trailer.

## Review Focus
- Socket dies silently on a network switch (Wi-Fi → cellular): the client ping detects it within ~40 s and reconnects; polling covers the gap (Task 1 test).
- Access token expires mid-socket: server closes 1000, client reconnects immediately, 401 → renew via HTTP sync → reconnect with the new token (Task 1 test).
- The writer's own hint arrives while its push is in flight: no extra sync request (Task 2 test).
- App backgrounded: socket closed cleanly with 1000, and re-foregrounding reconnects without a 429 from the single device slot (Task 3 e2e).
- Server doesn't support the upgrade (400/404/426, or the proxy refuses): the client stops trying for this engine lifetime and polling stays at 20 s (Task 1 test).

---

### Task 1: Live socket transport and runner
**Files:** new `Packages/BabyTracker/Sources/BabySync/LiveSocket.swift` (protocols, `LiveSocketError`, `URLSessionLiveSocketConnector`); modify `BabySync/FamilySyncWebSocket.swift` (runner, reconnect rules, bounded hint tracking, delayed backoff reset); tests `Tests/BabySyncTests/FamilySyncWebSocketTests.swift` (spec tests 1–14).
**Interfaces:** Produces `LiveSocketConnecting`, `LiveSocket`, `LiveSocketError`, `URLSessionLiveSocketConnector(endpoint:session:)`, and `FamilySyncWebSocket` with `start(realm:serverInfo:)`, `stop() async`, `state`, callbacks `onHint(Cursor)`, `onConnected()`, `onUnauthorized()`, `onStateChange(State)`.
- [x] Failing tests (spec §5 unit tests 1–14 relevant to the transport/runner, incl. the live-hint fixture decode)
- [x] Implement per spec §2.2 a–c, §2.3 minimal path, §2.4
- [x] `make test`
- [x] Commit `feat(sync): open the live hint socket`

### Task 2: Engine integration
**Files:** `BabySync/FamilySyncEngine.swift`, `SettingsFeature/FamilySyncComposition.swift`, `BabySync/BackgroundSync.swift` (doc comment); tests `FamilySyncEngineTests.swift` (spec tests 15–16 + coalescing/writer's-own-hint/local-write follow-up tests 4–7).
**Interfaces:** Consumes Task 1. Produces engine `liveState`, `lastSyncReason`, `liveHintArrived(_:)`; `assemble(..., foregroundPollInterval:)` for tests; debounce 300 ms.
- [x] Failing tests
- [x] Implement per spec §2.2 d–e, §3 debounce
- [x] `make test`, `make build`
- [x] Commit `feat(sync): sync as soon as the other phone writes`

### Task 3: E2E proof and deploy config
**Files:** `Server/scripts/protocol-e2e.sh` (nginx upgrade map/headers; require `step 15:`), `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/TwoCaregiverSyncE2ETests.swift` (step 15 per spec §5), `Server/deploy/unraid/compose.yaml` (`BT_SOCKET_DEVICE_LIMIT=2`) and its structure test if one pins the env.
- [x] Add step 15; run `make server-protocol-e2e` until 15/15 with the latency proof line
- [x] `make server-test` (deploy-structure tests)
- [x] Commit `test(e2e): the other phone hears about a write within seconds`

### Task 4: Gates, redeploy, install, measure
- [x] All gates: `make test`, `make build`, full UI suite, `make server-test`, `make server-smoke`, `make server-protocol-e2e`
- [x] Merge to main; redeploy the server (compose config change) with the documented procedure; verify
- [x] Install on both phones; measure write-on-A → visible-on-B on the real phones (10 trials, median/p90) and confirm the socket connects through Tailscale Serve (risk R1)
- [x] Verification doc `docs/verification/2026-09-26-live-sync.md`; commit
