# Automatic family Live Activity push

Status: approved by the user on 2026-10-03 ("Approved"). The user confirmed remote start/update/end and replaced the proposed opt-in with automatic behavior on their server, with no in-app setting.

The [implementation plan](../plans/2026-10-03-family-live-activity-push.md) was approved. It is **implemented and verified locally** on `feat/family-live-activity-push`: Tasks 1–11, final code commit `a73528c`, [local verification](../../verification/2026-10-03-family-live-activity-push.md).

**Signed-device and production acceptance are pending.** They follow the [rollout and two-phone runbook](../../runbooks/family-live-activity-push.md) under a separate approval. Apple provisioning, which currently lacks `aps-environment`, is a blocker for that approval. Local evidence is not phone delivery.

## Intent and scope

When one caregiver changes a feeding or sleep activity, another caregiver's Lock Screen and Dynamic Island should reflect that change without opening Baby Tracker. A new feed or sleep should also create a Live Activity remotely. This is an automatic enhancement for the server selected by the app's existing built-in endpoint and immutable server identity. Respect iOS Live Activities authorization; add no preference, opt-in screen, or notification-settings feature inside the app.

Other self-hosted servers retain local Live Activities and their existing foreground/background sync behavior. They do not depend on the official server, forward family data to it, or require Apple push credentials. This change does not introduce a public push relay or a custom self-hosted APNs setup flow.

Success requires actual start, update, and end delivery between two signed iPhones with the receiving app backgrounded or not running. Simulator builds, fake APNs tests, and server acceptance of a push do not prove phone delivery. Delivery remains subject to iOS authorization, network availability, ActivityKit lifetime and push budgets; there is no guaranteed delivery deadline.

## Current source evidence

Inspected on 2026-10-03:

- `Packages/BabyTracker/Sources/BabyStore/LiveCardBoard.swift` creates activities without a push type and exposes a memory-backed board for deterministic controller tests.
- `BabyStore/LiveActivityController.swift` handles card adoption, duplicate cleanup, feeding breaks and sleep completion locally.
- `BabyStore/TimerActivityAttributes.swift` defines the shared content-state contract and backward-compatible decoding. Existing activity attributes identify baby and timer kind, but not server, family, or session generation.
- `BabySync/FamilySyncEngine.swift` maintains foreground live sync. `announceRemoteTimers` only creates cards for remotely appeared timers while foregrounded.
- `SettingsFeature/FamilySyncComposition.swift` pins `https://baby-sync.example-tailnet.ts.net` and server instance `<instance-uuid>`. This identifies the intended server; it is not proof of current production configuration.
- `Server/internal/httpapi/operational.go` advertises server capabilities. Authentication mode is separate from whether this particular deployment can deliver pushes.
- `Server/internal/sync/mutation.go` has a best-effort post-commit observer. This alone cannot durably guarantee enqueueing push work.
- `Apps/BabyTracker.entitlements` does not currently declare an APNs environment.

## Approach

Use individual ActivityKit APNs pushes from the existing Go server. Obtain push-to-start tokens and per-activity update tokens through ActivityKit. Persist a durable delivery queue in PostgreSQL alongside committed family changes, with a bounded worker and an injectable APNs transport.

Alternatives considered:

1. Silent background notifications followed by sync: still depends on the app receiving execution time and does not directly solve suspended-app card updates.
2. ActivityKit broadcast channels: suited to large audiences; unnecessary channel lifecycle and access-management complexity for small private families.

The direct-token approach makes each registration independently revocable and keeps family access checks in the existing server boundary.

## Availability and device lifecycle

The app automatically registers when all conditions hold: the confirmed endpoint matches the built-in origin and instance identity; the server advertises the versioned Live Activity push capability; there is a current authenticated family membership; and iOS permits Live Activities. A self-hosted server advertising the same capability does not by itself pass the built-in identity gate.

The server advertises the capability only when explicitly configured with valid push settings for this deployment. Do not couple it to `BT_MODE=hosted`, which also controls unrelated identity and security behavior. Apple credentials, bundle topic, and sandbox/production environment are operator configuration, not user-facing preferences. Missing configuration means capability absent and existing sync remains available; enabled but malformed configuration fails validation clearly.

Persist registrations scoped to server instance/recovery epoch, authenticated account, device installation and family. Bind tokens to their APNs environment, payload contract version and registration generation. Derive the principal from normal authentication, verify baby access, and never trust a request's family identifier as authority. Token upserts and removals are idempotent and constrained to the registering device.

Observe push-to-start token rotation, new activities, update-token rotation and activity end/dismissal. Register initial token values and subsequent changes; retry failed registration through bounded background work and foreground catch-up. Observe remotely started activities during background launches without requiring the main screen to appear. Private server reachability is required to register tokens; it is not required for Apple to deliver a push to a token already registered.

Disable future delivery and remove registrations on sign-out, account/family switch, revoked membership, server identity/recovery change, or invalid APNs token. Recheck authorization at dispatch, not just registration. Remove local cards on sign-out/realm switch. System authorization changes observed by the app invalidate registration. Already submitted pushes cannot be recalled, and the app cannot promise immediate observation of changes made while it is suspended.

## Presentation and action correctness

Remote presentation follows the existing controller rules:

- Feed and sleep may each have a card for a baby; one must not accidentally end the other while both run.
- Feeding transitions preserve breast side, timed bottle, completed components, pause state and elapsed-time anchors.
- Stopping a feed shows its valid continuation break, or ends the card when none exists. Starting sleep ends a feeding break card. Waking ends sleep immediately.
- A scheduled server reconciliation ends an expired break even if no later family write occurs. Include a stale date so obsolete break controls stop being actionable if delivery is delayed.
- New cards represent current running activities, not historical entries or an already expired break. A fresh device registration may catch up to current running activities; historical writes must not produce a burst of starts.
- Preserve the user's dismissal for that activity generation; do not continuously recreate a dismissed card for the same session.

Add a versioned presentation contract with server/family identity, logical activity generation and revision metadata. Keep existing activities decodable and retire or reconcile legacy cards safely when enabling push. A logical generation distinguishes a new sleep/feed from a delayed command for a previous one, while feeding continuation may reuse its still-open card.

Build payloads from committed canonical family state, not arbitrary client-supplied presentation JSON. Keep localized labels on-device where practical; define and test any required per-device locale handling. Share golden JSON fixtures between Go encoding and Swift decoding, including Foundation date encoding, optional fields, feeding components and payload size limits. Bound component summaries to ActivityKit's payload budget while retaining accurate current status. Timer ticks are rendered from timestamps locally; do not send per-second pushes.

A pushed card is presentation, not a write into SwiftData. Before a Lock Screen control mutates shared state, validate the card's realm, baby and activity generation and perform bounded catch-up. If freshness/identity cannot be established, do not act on an obsolete local timer; hand off to the app with an actionable explanation. Preserve existing local-only/self-hosted action behavior. Current timed-bottle Lock Screen stops save the baby's usual amount and enqueue the app's later "Set amount" flow; preserve that behavior. Viewers receive presentation but must not gain write controls or mutation authority.

Planning source clarification: `TrackingPreferences` stores feeding-break limits per device (5–120 minutes in five-minute increments, default 30), so device registration carries that presentation preference and updates it when changed. This adds no new setting. The timed-bottle wording above was corrected to match the inspected `OutOfAppFeedActions` implementation rather than the older amount-entry-before-stop behavior.

## Durable delivery and ordering

Persist push invalidation work in the same transaction as relevant committed changes, or through an equivalently durable journal cursor. Include timer, feed/session, baby deletion and presentation-affecting edits; access changes invalidate registrations. The implementation plan must identify all mutation entry points, including imports and maintenance, rather than relying only on the HTTP sync handler.

The worker coalesces by device and logical activity, reads the latest authorized committed state, then emits start/update/end. Superseded starts are dropped: a timer that already stopped must not be newly displayed. Durable leases permit worker restart and bounded retry without allowing concurrent sends for the same target.

Maintain ordering and terminal-state rules per generation. Account for multiple commits within one APNs timestamp second; a stale retry must not overwrite a newer state or resurrect an ended generation. Local reconciliation must likewise avoid replacing a newer pushed state with an older local snapshot before catch-up. Persist delivery progress and token-generation changes. Registration of a rotated or newly created activity token triggers a latest-state reconciliation so changes during registration are not lost.

APNs submission and database acknowledgment cannot be atomic. Do not claim exactly-once remote starts. Use a stable logical generation, conservative retry handling after ambiguous start outcomes, and activity discovery/reconciliation to eliminate duplicates when execution is available. Define and test the ambiguity policy in the implementation plan; transient duplicates under lost acknowledgments remain a device-verification case.

Use bounded retry/backoff with jitter, APNs expiry appropriate to the current session, and separate handling for permanent token errors, throttling, transient failures and credential/configuration errors. Start payloads include Apple's required alert; routine updates omit alerts/sounds. Do not add separate ordinary notifications for the same changes.

## Server boundaries and operations

Keep four responsibilities explicit: authenticated registration storage, canonical presentation projection, durable delivery scheduling, and APNs transport. The worker must not hold a family write transaction or authorization lock across a network request. Fresh authorization immediately before dispatch limits revocation races; document that a request already in flight cannot be recalled.

Bound workers, database connections, batch sizes, payload size and retention. Push failure must not make care recording fail or block ordinary family sync. Store private signing material only through the existing protected configuration/deployment mechanism; never commit or log it. Do not log tokens, baby names, care content or raw push payloads. Operational counters cover queue age/depth, attempts, token invalidation and response classes.

APNs needs outbound HTTPS from the app/worker container, not public inbound ports or a public family API. Keep the existing private Tailscale endpoint. Coordinate migrations and shared configuration seams with the capacity/MCP work in progress; allocate migration numbers from the actual integration base.

## Verification and rollout

Automated acceptance includes:

- Swift eligibility, no-setting behavior, unsupported-server fallback, authorization changes, registration retry/rotation, background activity observation and legacy decoding.
- Go registration ownership, cross-family denial, revocation, sandbox/production separation, durable enqueue rollback/commit behavior and queue recovery after restart.
- Cross-language fixtures for breast-side switches, mixed feeding, timed bottles, pauses, feeding breaks, sleep completion, edited start times and bounded large summaries.
- Ordering tests for rapid start/stop, stop followed by new start, same-second updates, token rotation during send, stale retries, local/pushed revision races, duplicate discovery, dismissal and expiry.
- Lock Screen controls with stale local data, family changes, viewer access, offline catch-up failure and timed-bottle completion.
- Fake APNs contract tests for headers, token-based authentication, payloads, response classes, retry bounds and redacted diagnostics.

Run applicable Swift/server/protocol checks and inspect the whole diff before integration. Build only on the shared simulator `platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853`; stop if unavailable or incompatible.

Physical acceptance uses two signed phones: foreground baseline, backgrounded receiver, locked receiver, terminated-app receiver, delayed registration/reconnect, receiver dismissal, server restart, sign-out and membership removal. Verify actual card content and controls, not just APNs success responses. Record delivery times without turning a few observations into a delivery SLA. Distinguish OS-supported termination behavior from guaranteed delivery.

Production setup and rollout follow a separate concrete approval: signing/provisioning and APNs key readiness; source identity; migration and rollback procedure; server capability evidence; queue health; bounded logs; and two-device results. No Apple account configuration, production mutation, deployment or phone installation is authorized merely by approving this spec.

## Sources

- [Apple: starting, updating and ending Live Activities with push notifications](https://developer.apple.com/documentation/ActivityKit/starting-and-updating-live-activities-with-activitykit-push-notifications)
- [Apple: displaying live data with Live Activities](https://developer.apple.com/documentation/activitykit/displaying-live-data-with-live-activities)
- [Apple: Live Activities design guidance](https://developer.apple.com/design/human-interface-guidelines/live-activities)

Apple documentation was consulted during the design investigation. Verify exact SDK APIs, APNs details and signed provisioning during implementation; these sources do not establish current deployment readiness.
