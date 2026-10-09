# Running-timer editing and invitation codes — design (2026-09-26)

User requests (2026-09-26): (1) tapping a running timer lets you edit it — decided scope: the start time AND each already-finished part (a finished breast side's duration, a bottle's volume and contents), synced to the other phone; (2) the QR scan to join didn't work — replace it with an invitation code: the inviter sees a code with Copy and Share, the joiner pastes the code (or the whole link) into one field. Decided: keep the server's existing long secure token as the code (copy/paste), no short code, no server change for invitations.

## Part A — Invitation code (client only)

- Inviting (owner, Settings → Caregivers): "Invite caregiver" / "Invite view-only" show the invitation code (the token from the minted link, `InvitationParser.parse(shareURL).token`) in a large monospaced, selectable text, with **Copy** (UIPasteboard, confirm "Copied") and **Share** (shares the code text). Revoke stays. The QR code view and the QR share are removed.
- Joining: one **Invitation code** text field + **Join** button (replaces the "Scan QR code" action and `InvitationScannerView`). Input accepts a bare code OR a full link: if it parses as a link use `InvitationParser.parse`, otherwise treat the trimmed text as the token and pair it with the confirmed endpoint's origin (`InvitationIntent(origin: confirmedEndpoint.origin, token:)`), validating the token shape the same way the parser does. Paste button fills from the clipboard. Errors show inline copy ("That code isn't valid" / server refusal text).
- Everything else about joining (server confirmation, name, Apple sign-in) is unchanged. Remove the camera usage string only if nothing else uses the camera.
- Tests: parser/intent from bare code and from link; view-model join from code; UI test: invite shows a code with Copy; join field present (no network).

## Part B — Editing a running timer (server + client)

### Key decisions
| Decision | Choice | Why |
|---|---|---|
| Wire operation | Reuse `update` on `entity_kind: "timer"` with new patch payload `TimerEditPayload` | Journal op CHECK constraint (`Server/migrations/000009_canonical_records.sql:181-182`) already allows `update`; a new op name would need migration 18 + schema bump. |
| Migration | None; schema stays 17 | |
| Semantics | Patch: `started_at` null/omitted = unchanged; `components` lists only changed completed parts, matched by id | A full replace would clobber a concurrent switch/bottle from the other phone. |
| Component ids | Preserved (and positions); kind immutable | Completion carries the same ids. |
| Concurrency | Server-authoritative, serialized by `GetTimer ... FOR UPDATE` (`records/timers.go:31-38`); rules evaluated against the locked row at apply time. | Same model as switch/attach. |

### Wire contract
`operation: "update"`, `entity_kind: "timer"`, `payload_version: 2` only (v1 → rejected `invalid_mutation`).
```json
{ "baby_id": "<uuid>", "started_at": "<RFC3339>" | null, "components": [ {"id":"<uuid>","kind":"breast","side":"left","seconds":300}, {"id":"<uuid>","kind":"bottle","volume_ml":70,"contents":"formula"} ] }
```
Elements use the existing `FeedComponent` shape (`protocol/entities.go:28-37`) validated by `validateComponents` (`protocol/mutations.go:261-299`, `records/validation.go:272-313`).

Stateless validation (`ValidateMutation`): kind timer + op update + version 2 + valid `baby_id` else `invalid_mutation`; `started_at` if present passes `validateTimestamp` else `invalid_timestamp`; components pass `validateComponents`; `started_at == null && components == []` → `invalid_mutation`.

Stateful apply (`editTimer`, against the locked row):
1. Timer tombstoned → `superseded/timer_ended` with the completion related entity (reuse `endedTimerOutcome` `records/timers.go:505-525`).
2. `baby_id` mismatch → `record_parent_mismatch` (existing check `timers.go:194-197`).
3. Sleep timer with non-empty components → `invalid_timer_kind`.
4. Each component edit: id not in `CompletedComponents` → `rejected/timer_component_not_found` (new code); kind differs → `invalid_mutation`; else replace in place.
5. If `started_at` set (S' new, S old): `S' > accepted_at + 1 min` or `S' < accepted_at − 7 days` → `invalid_timer_time`. Current-side tie rule: if `current_side != nil && current_side_started_at == S` (microsecond instants) then `current_side_started_at := S'`; else if `current_side != nil && S' > current_side_started_at` → `invalid_timer_time`.
6. Elapsed bound: sum of breast seconds ≤ `ref − start'` where ref = (possibly moved) `current_side_started_at` if a side runs, else `accepted_at` → else `invalid_timer_time`.
7. Result deep-equals stored → `applied` at current cursor, no journal row.
8. Else set `ComponentVersion = 2`, recompute legacy projection via `feedComponentProjection` (`records/feeds.go:165`, as at `timers.go:277-278`), persist via `persistTimerChange` (`timers.go:330-370`).
Every rejection returns the canonical timer. Concurrency: edit vs stop — first commit wins (stop first → edit superseded/timer_ended; edit first → `completedTimerPayload` uses edited values). Edit vs switch/bottle — ids stay valid (append-only); edit vs edit — later wins per field.

### Server changes
- `internal/protocol/entities.go`: `TimerEditPayload { BabyID string; StartedAt *string; Components []FeedComponent }` + `protocolPayload()`.
- `internal/protocol/mutations.go`: `decodeMutationPayload` (`:98-121`) route `update`+`timer`; `validateRequiredPayloadShape` (`:164-200`) require `baby_id`, `components`; `normalizeDecodedPayload` (`:396-489`) normalize `StartedAt` + component contents.
- `internal/protocol/canonical.go` `canonicalPayloadValue` (`:68-199`): add the case — REQUIRED (default errors, and `Repository.Apply` hashes first `records/repository.go:96`).
- `internal/records/validation.go` `ValidateMutation` (`:13-117`): stateless rules.
- `internal/records/timers.go`: `ApplyTimer` (`:116-123`) route `update` → `changeTimer`; `changeTimer` (`:205`) new case → `editTimer`; `timerMutationBabyID` (`:638-650`) add case.
- **Required bug fix** `putTimer` (`timers.go:568-573`): the `ON CONFLICT DO UPDATE` never writes `started_at`; add `started_at=EXCLUDED.started_at` (no-op for existing callers; row CHECK `current_side_started_at >= started_at` is evaluated after both are set).
- `api/openapi.yaml` (`:1632-1678`), `docs/protocol/v1.md` (`:97-113`), fixtures `api/fixtures/v1/mutations.json` + `manifest.json` (sha256) + a `canonical-vectors.json` vector.

### Client changes
- `TimerIntentReducer.swift`: `case edit(timerID:babyID:startedAt:components:)`; `(.edit, .neverDispatched)` folds into the undispatched start (`:94`, `foldIntoUndispatchedStart :142-154` — ActiveTimer already mutated); `(.edit, .inFlightOrUncertain)` dependency-blocked (`:106`); acknowledged/remote → ready. `appendTimerCommand` (`:175-221`): operation `.update`, payload `baby_id`, `started_at` (wireTime or NSNull), `components` via `MutationFactory.componentPayload`; version 2. Edit rows append after pending switch/bottle rows.
- Store verb (`BabyStore+Timers.swift`): `editRunningTimer(id:startedAt:components:expectedWriteContext:)` — re-fetch; apply exactly the server rules (tie compare on wireTime strings; elapsed bound with `ref = currentSideStartedAt ?? now`); replace parts in place; recompute `completedSides`/`pendingVolumeML/Contents` as `RemoteTimerState.write` (`BabyStore+RemoteTimers.swift:213-220`); `reduceTimerIntentLocked(.edit)`. Errors: `timerEnded, startInFuture, startAfterCurrentSide, partsExceedElapsed, unknownComponent, nothingChanged`. Local-only realm: local apply only.
- Remote apply: no change needed (`RemoteTimerState.write` already rewrites start/current side/components; owed-intent guard keeps a pending local edit; refused edit rolls back).
- Live Activity: editing phone calls `LiveActivityController.update(for:)` after save; other phone already updates via `.changed` announcement.

### Editor UI
- `RunningTimerView`: `TimerDial` becomes a plain Button (`home.timerDial`, hint "Edit the start time and the parts so far"), pencil next to "Started …"; only when the caregiver can write.
- `HomeView` presents `RunningTimerEditSheet` via `.sheet(item:)`; `HomeViewModel.makeTimerEditor()`.
- `RunningTimerEditModel` (@Observable, copy in a `*Copy` enum): holds a value copy + timer id (not the @Model); start DatePicker (date+time, min `min(start, now−24h)`, max now or current side start if untied); "So far" rows per finished part — breast duration stepper (60 s steps, min 1 s), bottle volume stepper (`EntryEditModel.volumeRange`/`volumeStep`) + contents picker; read-only running-side line; `validationMessage`/`canSave` mirror store rules; `save()` sends only changed fields; timer gone → "This timer was stopped on another phone — edit the feed in History." Sleep: start only.

### Tests
Go records (`timer_edit_test.go`): start edit moves tied side + readback via GetTimer/snapshot/journal + completion uses edited values; untied side stays / start after side → invalid_timer_time; part edits keep ids/positions/projection; unknown id / kind mismatch / sleep components; parts > elapsed, future/too-old start; edit after stop superseded; no-op edit no journal row; v1 rejected, baby mismatch, viewer refused; switch/edit orders; legacy v1 timer upgrade; resend same mutation id → already_applied. Go protocol decode + canonical vector + fixture conformance. `tests/protocol/timer_test.go`: dropped edit response recovers receipt then stop sees edit.
Swift: WireShapeTests (keys, update, v2, lowercase ids); SharedTimerTests (fold into undispatched start; blocked behind in-flight start; ready after acknowledged, ordered after pending switch; edit then stop on undispatched → one create feed with edited values; refused start takes held edit down); TimerTests (local rules); RemoteTimerApplyTests (remote edit → .changed same ids; pending local edit not overwritten; refused edit rolls back); HomeFeatureTests (edit model); LiveActivityControllerTests (presentation reflects edited start).
UI test: start feed, tap `home.timerDial`, move start back, save, "Started …" changed and elapsed grew; add sheet to AccessibilityAuditTests.
E2E: new step 11 after current step 10 — joiner edits the mirrored feed timer (start 2 min earlier, left 300 s, formula 70 ml), syncs; owner sees edited start and components with same ids; old 11–13 → 12–14 (stop step expects edited values); `protocol-e2e.sh` requires `step 14:`.

### Risks
Deploy order: redeploy server before shipping the app (old server refuses the edit; client quarantines only that row). No migration/backfill; `putTimer` fix no-op for existing callers. Client/server rule drift is caught by the e2e step. Out of scope: editing the running side's own time, adding/removing parts.
