Superseded: the homelab was cut over on 2026-10-10 by docs/verification/2026-10-10-homelab-cutover-runbook.md, which keeps Live Activity push on through deploy/unraid/push.yaml.

# Family Live Activity push — rollout and two-phone acceptance

**Status: not executed.** This runbook was written and checked against the tree on 2026-10-04 at branch `feat/family-live-activity-push`, `87c6f50`, and re-checked after the final review fix wave at its gate commit (recorded in the [verification note](../verification/2026-10-03-family-live-activity-push.md)). Nothing in it has been run against Apple, the phones or the Unraid server.

Every phase below that touches Apple, a phone or production needs its own explicit approval. Approving this document approves none of them.

To record a run, copy this file to `docs/verification/<yyyy-mm-dd>-family-live-activity-push-rollout.md`, fill in every `<…>`, and keep the STOP results. The style follows [the format-cleanup rollout](../verification/2026-10-02-format-cleanup-rollout.md): every gated step is a single `&&` chain that begins with its gate, and a failed gate prints `STOP` or skips what it guards. Do not continue past a failed gate.

Related documents:

- Design: [spec](../superpowers/specs/2026-10-03-family-live-activity-push-design.md), section "Verification and rollout".
- Local evidence: [verification](../verification/2026-10-03-family-live-activity-push.md).
- Operator reference: [`Server/README.md`](../../Server/README.md), "Live Activity push" and "Private Unraid deployment over Tailscale".

## Phases and approvals

| Phase | What it touches | Approval |
| --- | --- | --- |
| P0 Local evidence | The Mac only: gates, a local `linux/amd64` image | None beyond normal local work |
| P1 Apple provisioning | Apple Developer account: App ID capability, profile, APNs key | **Separate approval.** Currently a blocker. |
| P2 Project switch | One commit written by `scripts/live-activity-push-signing.sh enable`: the app target's entitlement lines in `project.pbxproj` and one Swift test | Code review |
| R1 Server upgrade, push off | Production database (schema 27 → 28) and server image | **Separate approval** |
| D Phone builds | Signed Debug build installed on both phones | **Separate approval** |
| R2 Enable push | Production secrets, `deployment.env` and the overlay | **Separate approval** |
| A Two-phone acceptance | Both phones and the family's real data | Part of the R2 approval |
| X Disable or roll back | Production | Covered by the R1/R2 approval; a binary rollback is not proposed |

Order: P0 → P1 → P2 → R1 → D → R2 → A. R1 can run before P1/P2. Push stays off until R2.

## Facts from the tree

All re-checked against the tree in the readiness plan's final fix wave (branch `feat/family-live-activity-push-ios`, the commits on top of `ad8ded5`), line numbers included.

| Item | Value | Where |
| --- | --- | --- |
| Built-in server identity | origin `https://baby-sync.example-tailnet.ts.net`, instance `<instance-uuid>` | `Packages/BabyTracker/Sources/SettingsFeature/FamilySyncComposition.swift:21-22` |
| App bundle and APNs topic | `com.olhapi.babytracker`; topic `com.olhapi.babytracker.push-type.liveactivity` | `project.pbxproj:468,505`; `Server/internal/liveactivity/apns.go:154` |
| Signing | team `TEAM123456`, manual signing. Debug and Release both use the profile `BabyTracker Development - TEAM123456`; the widget uses `BabyTrackerWidgets Development - TEAM123456`. Device builds of both targets sign with the identity `Apple Development: developer@example.com (CERT123456)` (`CODE_SIGN_IDENTITY[sdk=iphoneos*]`). | `project.pbxproj:470,507,538,570` (profiles); `:448,485,520,552` (identity) |
| Device entitlements | `Apps/BabyTracker.entitlements` (no `aps-environment`) | `project.pbxproj:446` (Debug), `:483` (Release) |
| Simulator-only entitlements | `Apps/BabyTrackerLiveActivityPush.entitlements` (`aps-environment` = `development`), selected only through `"CODE_SIGN_ENTITLEMENTS[sdk=iphonesimulator*]"` | `project.pbxproj:447,484` |
| Signing readiness tool | `scripts/live-activity-push-signing.sh` (`check`, `check --app`, `enable [--dry-run]`, `state`); `check` also verifies, read-only and by name only, that the pinned signing identity is present and valid in the keychain (`security find-identity -v -p codesigning`); fixture tests in `scripts/live-activity-push-signing_test.sh` (`make test-signing-script`) | P1, P2, D-3 |
| Environment resolver | Simulator → sandbox. No `embedded.mobileprovision` → production. Profile `aps-environment` `development` → sandbox, `production` → production. Any other value, or no key, → `nil`, and push stays inert. | `Packages/BabyTracker/Sources/BabySync/LiveActivityAPNsEnvironment.swift` |
| Schema | Exactly 28 (`"min": 28, "max": 28`). `000028_live_activity_push.sql` has SHA-256 `43b37464fd4570ae65eb2a84e3579e7e76dbc9e6c4b4ca3d598a2a74f3bc521e`. | `Server/internal/migrate/compatibility.json` |
| Older binaries | `2961ca1` accepts exactly 26; `main` (`8032e79`) accepts exactly 27. A binary rejects any applied migration it does not know, and any version outside its `[min,max]`. | `git show <rev>:Server/internal/migrate/compatibility.json`; `Server/internal/migrate/compatibility.go:43-78` |
| Migration transaction | Each file and its `schema_migrations` row commit in one transaction. There is no `CONCURRENTLY` in 28. | `Server/internal/migrate/migrate.go:109-120` |
| Server configuration | `BT_LIVE_ACTIVITY_PUSH_ENABLED`, `BT_APNS_TEAM_ID`, `BT_APNS_KEY_ID`, `BT_APNS_BUNDLE_ID`, `BT_APNS_KEY_FILE`, `BT_LIVE_ACTIVITY_DATABASE_URL_FILE`. An inline `BT_LIVE_ACTIVITY_DATABASE_URL` is rejected. | `Server/internal/config/live_activity.go` |
| Secret files (names only) | `$BT_DEPLOY_ROOT/secrets/live_activity_password` (db), `…/live_activity_database_url` and `…/apns_key.p8` (app; mounted as `apns_key`) | `Server/deploy/unraid/live-activity-push.yaml` |
| Secret file modes | App files must be regular, non-symlink and exactly `0600` (`exactOperatorSecretMode`), readable by UID 65532. The db password is read by PostgreSQL (UID 70; `provision-unraid-secrets.sh` creates `database/` as `70:70`). `provision-live-activity-secrets.sh` creates the three push files with exactly these owners and modes. | `Server/internal/config/operator.go:124`; `Server/scripts/provision-unraid-secrets.sh`; `Server/scripts/provision-live-activity-secrets.sh` |
| Key-arrival preflight | `baby-tracker-server live-activity-preflight [--environment sandbox\|production\|both] [--skip-apple] [--skip-database]`: key file, identifiers, topic, one dummy-token push per selected environment through the production transport and JWT (`400 BadDeviceToken` = PASS for the key, Team ID and Key ID — a dummy token cannot prove the topic, which R2-7's first accepted send does; 403 = key/team/key ID; `TopicDisallowed`/`BadTopic`/`DeviceTokenNotForTopic` = topic; 429 = retry; no answer = connectivity), and the worker database checks when `BT_LIVE_ACTIVITY_DATABASE_URL_FILE` is set. Exit 0 only when every requested check passed; never prints a key, token or URL. | `Server/cmd/baby-tracker-server/live_activity_preflight.go`; `Server/internal/liveactivity/preflight.go` |
| Database principals | group `baby_tracker_live_activity` (`init-roles.sql`); login `baby_tracker_live_activity_app` (`init-users.sh`). The login stays passwordless until `/run/secrets/live_activity_password` is mounted. | `Server/deploy/postgres/` |
| Upgrade rehearsal | `Server/scripts/rehearse-live-activity-upgrade.sh --dump … --ledger-tar … --actor-id-pepper … [--old-tree --old-image --new-tree --image]`, or `--synthetic`: the R1 cutover and R2's database half on a disposable copy, every resource removed afterwards. Static tests in `Server/scripts/rehearse_live_activity_upgrade_test.go`. | R1-3b |
| Capability | `live-activity-push-v1` in `GET /v1/server` `.capabilities` | `Server/internal/protocol/live_activity.go:9` |
| APNs | `https://api.sandbox.push.apple.com` and `https://api.push.apple.com`; 10 s request bound; no internal retries | `Server/internal/liveactivity/apns.go:21-24` |
| Worker logs | `live activity worker activation deferred` (`code`, `retry_in`; boot found the database or readiness not ready), `live activity worker stopped` (`reason=local_dependency_unavailable`, `code=<fixed code>`), `live activity worker resume failed` (`code`, `retry_in`) and `live activity worker resumed`. Per send APNs did not accept: `live activity push not accepted` (`event`, `kind`, `class`, `status`, `reason`, `attempt`, `sent`). Per job parked on its own failure: `live activity job parked` (`kind`, `code`, `attempt`, `retry_in`). Per stale lease the worker could not release: `live activity stale lease not released` (`kind`, `code`). No line carries a token, an id, a name or an error message. | `Server/cmd/baby-tracker-server/live_activity.go`; `Server/internal/liveactivity/worker.go` |
| APNs answer per attempt | `live_activity_history`: `event` at intent, then `result` (`accepted`, `invalidToken`, `retryable`, `configurationFailure`, `ambiguous`), `http_status` and an allowlisted APNs `reason`. `live_activity_delivery.last_reason`: the latest non-accepted class, `dispatch_failed` for a parked job, or `transient_conflict` for a job that lost a database race (SQLSTATE class 40) and is retried after the shortest backoff. | `Server/migrations/000028_live_activity_push.sql` (`live_activity_history`, `app.live_activity_target`) |
| Response classes | `BadDeviceToken` (also a token from the other APNs environment) and `Unregistered`/410 → `invalidToken`: that token is cleared. `DeviceTokenNotForTopic`, `TopicDisallowed`, `BadTopic` and any other 4xx such as 403 `InvalidProviderToken` → `configurationFailure`: no token is cleared and no generation fenced. 429 and 5xx → `retryable`. No answer → `ambiguous`. | `Server/internal/liveactivity/apns.go:168-186,255-268` |
| APNs priority | 10 for starts, ends and updates that change phase, activity kind or write access; 5 for content-only edits in the same phase | `Server/internal/liveactivity/lifecycle.go` (`updatePriority`) |
| Request log | JSON (`slog`): `msg="request completed"`, `route` (the pattern, never ids) and `status`. Every 5xx the shared error writers send adds one line: `msg="request failed"` at ERROR, or `msg="request abandoned by client"` at INFO when the client had already gone (`client_gone=true`, a force-quit or a lost connection; the response goes nowhere), with `route`, `code` (what the client saw, usually `service_unavailable`) and `cause` (a bounded name of the underlying failure: SQLSTATE and Postgres text, `context canceled`, a Go type; never provider or client text). | `Server/internal/httpapi/middleware.go` (`withMiddleware`, `logServerFault`) |

## Known limits

Accept these before approval.

- **Delivery is not guaranteed and has no SLA.** Apple's acceptance (HTTP 200) is not delivery. Only a card observed on the receiving phone proves delivery.
- **App-ended cards keep receiving sends.** Retirement is client-side only. APNs sends continue to an app-ended card's token until that generation ends. This is a ruled trade-off.
- **Revocation completes at the minute sweep.** Logout, removal and account deletion are refused at dispatch at once (zero sends were observed meanwhile). The rows are retired within about 60 s; 55–57 s locally.
- **Sign-out sends no client DELETE.** It relies on `/v1/auth/logout` plus the server sweep.
- **A removal does not wait for push.** `DELETE /v1/live-activities/device/{id}` is a repository-only path with no worker, APNs key or push configuration behind it: it succeeds on a server where push is disabled (X-1) and on an enabled server whose worker is stopped, with the same authentication, authorization and version idempotency. `PUT` stays refused with 503 and the capability stays absent meanwhile; the minute authority sweep still revokes any registration whose session or membership ended.
- **Apple-credential revocation is not observed here.** It relies on Apple server notifications, and `/v1/auth/apple/notifications` is registered only in hosted mode, which this deployment does not use. Registrations then end with the session's authority.
- **An enabled-mode boot that is not yet ready does not exit.** When the database, migrations, recovery reconciliation, worker privileges or the pool budget are not ready at boot, the server starts not ready (as a disabled one would), logs `live activity worker activation deferred` with a fixed code, and supervision retries with 2–60 s backoff until `live activity worker resumed`; push is unavailable and unadvertised meanwhile. Only malformed configuration (an unreadable key, missing APNs settings, a worker URL that does not parse) exits the process; a restart loop is therefore a configuration STOP; see R2-6.
- **Enabled startup can take about 25 s in the worst case.** Local validation is bounded, and the first worker pass has a 10 s + 5 s budget. The image healthcheck allows about 35 s.
- **A wedged removal (Task 8, deferred).** A removal the server refuses non-retryably stays pending on the phone and fails that phone's registration passes closed. The symptom is a phone that stops registering.
- **A sheet collision (Task 10, minor).** The handoff alert and the bottle "Set amount" sheet can collide on one activation.
- **Device-only assumptions.** These are unproven: ActivityKit timestamp handling (equal stamps, and whether `Activity.request` floors later updates at device time), ended-but-visible cards in `Activity.activities`, locked-after-first-unlock access to the ledger and Keychain, and `continueInForeground`. Rows A16–A21 test them.
- **The rehearsal has run only on synthetic data so far.** `Server/scripts/rehearse-live-activity-upgrade.sh` rehearses the whole R1 cutover (restore, db recreate, `init-users.sh` rerun, `migrate`, exact roles and privileges, the old image's refusal, readiness with push off, R2's database half) on a disposable copy; `--synthetic` has passed on the Mac (`linux/arm64`, the old image built with the identitytest adapter so the seeding client can sign in). Its run against the real backup is R1-3b, on the Unraid host, with the production `linux/amd64` images.
- **The Linux secret ownership is untested.** The local push profile ran on OrbStack, whose bind mounts map ownership. `provision-live-activity-secrets.sh` applies and verifies the owners and modes R2-2 names, but its tests ran on macOS temporary directories without root (ownership skipped, intended owners asserted in `--dry-run` output), not on Linux.
- **The preflight has not contacted Apple.** `live-activity-preflight` is proven against an in-process HTTP/2 TLS server for every answer class and against the disposable database for the worker checks. Its first real answer from Apple is R2-4.
- **The production APNs environment is out of scope.** The tree has no distribution signing. A TestFlight or App Store build has no embedded profile, and the resolver returns production. That path needs its own acceptance.

## STOP conditions (summary)

Each one is gated again where it applies.

1. **Preflight (R1-1).**
   - Production `source_version` is not the recorded `$old_sha`.
   - The instance is not `<instance-uuid>`.
   - The schema is not 27.
   - The verifier fails.
   - `$old_sha`'s manifest is not `{"min":27,"max":27}`. If it is 26, the format-cleanup rollout (27) must complete first under its own runbook. This runbook never applies 27 implicitly.
2. **Image (P0-3, R1-5).**
   - The revision label is not `$new_sha`.
   - The platform is not `linux/amd64`.
   - The binary contains `__identitytest` or `identitytest.invalid`.
   - The Unraid binary's SHA-256 differs from P0's. This is a STOP until explained.
   - The image is `sha256:9a98587f…`, the `e974fa1` image from Task 11. Never deploy it.
3. **Roles or migration (R1-3b, R1-6).**
   - The rehearsal on the backup (R1-3b) prints anything but `REHEARSAL: PASS` and `CLEANUP: removed …`. Its `STOP:` line names the step; nothing in production has changed yet.
   - The `init-users.sh` rerun fails.
   - The roles are not exactly as expected.
   - `migrate` exits non-zero.
   - The schema is not 28.
   - The migrations are not contiguous.
   - The digest of 28 differs.
4. **R1 start.**
   - `--wait` fails, `/readyz` is not 200, or the instance or epoch changed.
   - The capability is advertised while push is meant to be off.
5. **Signing (D-3).**
   - `codesign` and the embedded profile disagree, or either lacks `aps-environment` = `development`.
   - The build is not from a clean tree at the reviewed commit.
6. **Enable (R2).**
   - R2-2's dry run names other files or owners than expected, or the script refuses.
   - R2-4's or R2-6's `live-activity-preflight` exits non-zero; its lines name the mismatch (key/team/key ID, bundle/topic, connectivity, worker login, schema). A `[RETRY] … 429 TooManyProviderTokenUpdates` is Apple's 20-minute provider-token throttle: wait 20 minutes and rerun once, not a configuration STOP.
   - The capability is absent 60 s after `--wait`.
   - The app restart count rises: the process exits only on malformed configuration.
   - `live activity worker stopped` or `live activity worker activation deferred` appears without a later `resumed`.
   - Any 64+-character lowercase-hex run appears in the app log, which would be a possible token. Do not print the line.
   - The bounded queue thresholds in [Diagnostics](#diagnostics-bounded) are exceeded.
   - [R2-7](#r2-7-first-sends-stop-gate) finds a send APNs did not accept. The recorded class says which mismatch it is:
     - `invalidToken` 400 `BadDeviceToken`: an APNs environment mismatch. The token belongs to the other environment than the one the phone registered (D-3 and A1 must agree on `sandbox`). The token is cleared: `lap_tokens` shows `no-start-token` for the device, or `cleared` for a card.
     - `configurationFailure` 400 `DeviceTokenNotForTopic`, `TopicDisallowed` or `BadTopic`: the bundle ID (`BT_APNS_BUNDLE_ID`, the topic) is wrong for these tokens. The R2-4 preflight cannot detect this (a dummy token is refused before or after the topic check, undocumented); this is the definitive topic verdict. No token is cleared; a start stays `startUnconfirmed` with tries ≥ 1, and updates and ends retry.
     - `retryable` 429 `TooManyProviderTokenUpdates` within 20 minutes of the preflight: expected provider-token throttle, cleared on retry; not a STOP unless it persists past that window (R2-7).
     - `configurationFailure` 403 (`InvalidProviderToken`, `Forbidden`): the key, Key ID or Team ID is wrong. A start stays `startUnconfirmed` with tries ≥ 1; updates and ends retry.
7. **Acceptance.**
   - A write appears that nobody made.
   - A card is resurrected after its end.
   - A viewer's action is written.
   - Any send reaches a phone after its logout or removal sweep.
   - Care content or a token appears in any server log.

On a STOP after R2, the first action is [X-1 Disable delivery](#x-1-disable-delivery-preferred).

## P0 Local evidence (Mac, before any approval request)

From a **clean checkout** of the reviewed commit, not this worktree. Parallel sessions may edit `main`.

```sh
new_sha=<40-character final reviewed sha>; short=${new_sha:0:7}
git worktree add /path/to/bt-lap-release "$new_sha"
cd /path/to/bt-lap-release && [ -z "$(git status --porcelain)" ] && echo 'CLEAN TREE'
gate_sha=<the fix wave gate commit from the verification note>
git merge-base --is-ancestor "$gate_sha" "$new_sha" && echo 'CONTAINS THE FINAL REVIEW FIX WAVE'
git diff --stat "$gate_sha" "$new_sha" -- Server   # empty, or every change re-gated below
git show "$new_sha:Server/internal/migrate/compatibility.json" | jq -c '{min,max}'   # {"min":28,"max":28}
```

- [ ] **Gates.** If `git diff --stat … -- Server` is empty, the fix wave's server gates stand (all recorded at `$gate_sha` in the [verification note](../verification/2026-10-03-family-live-activity-push.md)). If it is not empty, rerun `make server-test`, `make server-smoke`, `make server-protocol-e2e` and `make server-build`.
  - Always run `TMPDIR=$(mktemp -d /tmp/btlap.XXXXXX) make test` with an owned short `TMPDIR`; see the verification note on Task 1.
  - Always run `make build SIMULATOR='platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853'`.
  - Record the exit codes: `<…>`.
- [ ] **Fresh `linux/amd64` image.** Do not reuse `baby-tracker-server:rollout-e974fa1bb2ad` (`sha256:9a98587f…`): its label names `e974fa1`, not the final source.

```sh
docker buildx build --platform linux/amd64 --load --build-arg BT_SOURCE_SHA="$new_sha" \
  -t "baby-tracker-server:rollout-$short" -f Server/Dockerfile Server &&
docker image inspect --format '{{.Id}} {{.Os}}/{{.Architecture}} {{index .Config.Labels "org.opencontainers.image.revision"}}' \
  "baby-tracker-server:rollout-$short"
bin=/path/to/bt-server-$short.bin
cid=$(docker create "baby-tracker-server:rollout-$short") &&
docker cp "$cid:/usr/local/bin/baby-tracker-server" "$bin"; docker rm "$cid" >/dev/null
shasum -a 256 "$bin"
grep -a -c '__identitytest' "$bin"              # must print 0
grep -a -c 'identitytest.invalid' "$bin"        # must print 0
grep -a -c 'api.sandbox.push.apple.com' "$bin"  # at least 1
grep -a -c 'api.push.apple.com' "$bin"          # at least 1
rm -f "$bin"
```

  Record: image `<id>`, `linux/amd64`, label `<new_sha>`, binary SHA-256 `<…>`. The image stays local and is never pushed. Go builds with a pinned toolchain, `-trimpath` and `CGO_ENABLED=0`, so the binary R1-5 builds on Unraid should hash the same.

## P1 Apple provisioning (approval-gated; not done)

**Blocker.** On 2026-10-04 `scripts/live-activity-push-signing.sh check` decoded the local profiles read-only (receipt: the readiness ledger's `task-5-check-real.log`). Both installed `BabyTracker Development - TEAM123456` profiles (`c2e88728…`, expiring 2027-09-26, and `d8c2e41a…`, expiring 2027-09-24) lack `aps-environment`; the widget profiles do not need it. A device build therefore resolves `nil`, and push stays inert.

Re-run that check at any time. It reads `PROVISIONING_PROFILE_SPECIFIER` for every configuration of the app and widget targets out of `project.pbxproj`, decodes every installed profile with that name, and prints only name, UUID, team, app ID, expiry and `aps-environment` — never certificates. It also checks, read-only, that the signing identity each configuration pins for device builds (`CODE_SIGN_IDENTITY[sdk=iphoneos*]`, today `Apple Development: developer@example.com (CERT123456)`) is present and valid in this Mac's keychain (`security find-identity -v -p codesigning`), and prints the identity's name only:

```sh
scripts/live-activity-push-signing.sh check          # exit 0 ready, 1 blocked, 2 tool error
```

Today it ends with `blocked: profile "BabyTracker Development - TEAM123456" (<uuid>) lacks aps-environment` for each installed copy and `result: blocked (2)`; each configuration's `signing identity: "Apple Development: developer@example.com (CERT123456)" -> ok` line shows the certificate is in place. Every profile sharing the specifier's name must be ready: Xcode picks one of them, and the script does not guess which. A profile file that `security cms -D` cannot decode is also a blocker (`unreadable profile at <path>`): its name is unknown, so it is not skipped; so is one whose expiry cannot be read (`has an unparsable expiry`). `blocked: signing identity "…" is not present and valid in the keychain` means the certificate or its private key is missing or expired on this Mac — a device build cannot sign until it is installed or renewed (Xcode › Settings › Accounts, team `TEAM123456`); `blocked: could not list codesigning identities` means the keychain could not be read and has the same effect.

With approval, in the Apple Developer account of team `TEAM123456`. Apple's console wording may differ; confirm on the day.

1. **App ID `com.olhapi.babytracker`:** enable **Push Notifications**. ActivityKit push tokens belong to the app, so the widget App ID `com.olhapi.babytracker.widgets` needs no change; `check` reports the widget profiles with `aps-environment: not required`. Change it only if signing reports otherwise.
2. **Regenerate the development profile** under the same name, `BabyTracker Development - TEAM123456`, with both phones included. `PROVISIONING_PROFILE_SPECIFIER` then stays unchanged. Install it, remove the stale copies of the same name from `~/Library/Developer/Xcode/UserData/Provisioning Profiles` (the script lists their paths), and re-run `check`. Expected: every listed `BabyTracker Development - TEAM123456` profile shows `aps-environment: development -> ok`, every `signing identity` line ends `-> ok`, and the last line is `result: ready (every device configuration of BabyTracker has a push-capable profile)`.
3. **APNs authentication key.** Reuse an existing team APNs key if there is one. Otherwise create one for Apple Push Notifications service with the sandbox environment (sandbox and production also works), and with topic `com.olhapi.babytracker` if Apple asks for a topic.
   - Record the 10-character Key ID and the Team ID; neither is a secret.
   - Download the `.p8` once into the password manager. It never goes into the repository, a command argument, a log, or this document.
   - The repository does not create Apple keys.

## P2 Project switch (one reviewed commit)

Device builds currently sign with `Apps/BabyTracker.entitlements`. The push file is selected only for the simulator SDK. The switch is applied by the script, never by hand:

```sh
scripts/live-activity-push-signing.sh enable --dry-run   # prints the exact diff; writes nothing
scripts/live-activity-push-signing.sh enable             # refuses unless `check` is ready for every device configuration
scripts/live-activity-push-signing.sh state              # project: enabled / pinning test: enabled
```

What `enable` writes, and nothing else:

```diff
-				CODE_SIGN_ENTITLEMENTS = Apps/BabyTracker.entitlements;
-				"CODE_SIGN_ENTITLEMENTS[sdk=iphonesimulator*]" = Apps/BabyTrackerLiveActivityPush.entitlements;
+				CODE_SIGN_ENTITLEMENTS = Apps/BabyTrackerLiveActivityPush.entitlements;
```

- Both app-target configurations (Debug and Release) of `BabyTracker.xcodeproj/project.pbxproj`; the widget target is untouched. The `"CODE_SIGN_ENTITLEMENTS[sdk=iphonesimulator*]"` conditional becomes redundant once the base setting names the push file, so it is removed rather than left as a second source of truth.
- The two entitlement files differ only by `aps-environment` (`pushEntitlementsAreExactlyTheAppEntitlementsPlusTheDevelopmentGateway`).
- `pushEntitlementsAreSelectedOnlyForTheSimulatorSDK` (`Packages/BabyTracker/Tests/BabySyncTests/LiveActivityPushEligibilityTests.swift`) pins the current split. `enable` replaces it in the same step with `pushEntitlementsSignEveryAppConfiguration`, which asserts the new state: two push lines, no base-file line, no simulator conditional. One test replaces one, so `EXPECTED_TESTS` is unchanged.
- `enable` refuses on a project or pinning test in any other state (`unexpected project state`), and a second run on a switched project is a no-op (`already enabled`). `--dry-run` while P1 is still blocked prints the diff and ends with `dry-run: enable would be refused`, exit 1.
- Gates, printed by `enable` as its `next` steps: `TMPDIR=$(mktemp -d /tmp/btlap.XXXXXX) make test`, `make build SIMULATOR='platform=iOS Simulator,id=D17101C9-01EB-488D-AFE1-58E6C089B853'`, the signed device build, and `check --app` on it (D-3). The script's own fixture tests: `make test-signing-script`.
- **Do not** run `enable` before P1. The script refuses anyway: without `aps-environment` in the profile, manual signing of a device build fails on the entitlement mismatch.
- Commit exactly the two files the script changed, as one reviewed commit (`git add BabyTracker.xcodeproj/project.pbxproj Packages/BabyTracker/Tests/BabySyncTests/LiveActivityPushEligibilityTests.swift`).

If P2 lands after P0, `$new_sha` becomes that commit, and P0 is repeated. The server tree is unchanged, so `git diff --stat` is empty and only the Swift gates rerun.

## Shell setup on Unraid

Use SSH to the Unraid host, approval-gated. Paste this once per session; every later block relies on it. Fill `old_sha` from R1-1.

```sh
export BT_DEPLOY_ROOT=/mnt/user/appdata/baby-tracker-sync
umask 077          # dumps, tars and new secrets are created 0600
set -o pipefail
new_sha=<40-character final reviewed sha>; short=${new_sha:0:7}
old_sha=<40-character sha production reports in R1-1>; old_short=${old_sha:0:7}
src="$BT_DEPLOY_ROOT/source"                    # the deployed tree compose runs from (confirm the path)
staged="$BT_DEPLOY_ROOT/source.$short-staged"
# deployment.env is read through --env-file only, never exported (see the
# format-cleanup runbook): exported BT_SOURCE_SHA/BT_IMAGE_NAME would override it.
unset BT_SOURCE_SHA BT_IMAGE_NAME
compose() {
  docker compose --project-name baby-tracker-sync --env-file "$BT_DEPLOY_ROOT/deployment.env" \
    -f "$src/Server/compose.yaml" -f "$src/Server/deploy/unraid/compose.yaml" "$@"
}
# The push overlay. While push is enabled, include it in every invocation that
# touches db or app (Server/README.md). Prefer --no-deps when changing only app.
compose_push() {
  docker compose --project-name baby-tracker-sync --env-file "$BT_DEPLOY_ROOT/deployment.env" \
    -f "$src/Server/compose.yaml" -f "$src/Server/deploy/unraid/compose.yaml" \
    -f "$src/Server/deploy/unraid/live-activity-push.yaml" "$@"
}
db_psql() { docker exec -i baby-tracker-sync-db-1 psql -X -q -v ON_ERROR_STOP=1 -U postgres -d babytracker "$@"; }
schema() { db_psql -At -c 'SELECT current_version FROM schema_compatibility WHERE singleton'; }
revision() { docker image inspect --format '{{with .Config.Labels}}{{index . "org.opencontainers.image.revision"}}{{end}}' "$1"; }
# /v1/server over the private HTTPS name, resolved the way verify-unraid-deployment.sh does.
server_json() {
  local ts ip
  ts=$(docker ps -q --filter label=com.docker.compose.project=baby-tracker-sync --filter label=com.docker.compose.service=tailscale)
  ip=$(docker exec "$ts" tailscale ip -4)
  curl -fsS --connect-timeout 5 --max-time 15 --resolve "baby-sync.example-tailnet.ts.net:443:$ip" \
    https://baby-sync.example-tailnet.ts.net/v1/server
}
rollback_src="$BT_DEPLOY_ROOT/source.$old_short-rollback-$short"
rollback_env="$BT_DEPLOY_ROOT/deployment.env.$old_short-rollback-$short"
cutover="$BT_DEPLOY_ROOT/backup.cutover-$short.dump"
ledger_tar="$BT_DEPLOY_ROOT/backup.cutover-$short.ledger.tar"
dump="$BT_DEPLOY_ROOT/backup.pre-live-activity-push-$(date -u +%Y%m%d).dump"      # R1-2
ledger_pre="$BT_DEPLOY_ROOT/backup.pre-live-activity-push-$(date -u +%Y%m%d).ledger.tar"
```

## Diagnostics (bounded)

Paste these after the shell setup.

- They print timestamps, 8-character prefixes of random device, job and generation ids, enums and counts.
- They never print tokens, names, notes, amounts or payloads.
- Every query is bounded.
- They run as the database's `postgres` operator, so row-level security does not hide rows.

```sh
# Queue health, one line.
lap_queue() {
  db_psql -At -F ' ' <<'SQL'
SELECT 'jobs='||count(*),
  'due_now='||count(*) FILTER (WHERE dirty_version > completed_version AND not_before <= now()),
  'scheduled='||count(*) FILTER (WHERE dirty_version > completed_version AND not_before > now()),
  'leased='||count(*) FILTER (WHERE lease_id IS NOT NULL),
  'stale_leases='||count(*) FILTER (WHERE lease_id IS NOT NULL AND lease_deadline < now()),
  'retrying='||count(*) FILTER (WHERE target_state ? 'retry_at'),
  'parked='||count(*) FILTER (WHERE last_reason = 'dispatch_failed'),
  'not_accepted='||count(*) FILTER (WHERE last_reason IS NOT NULL AND last_reason NOT IN ('dispatch_failed', 'transient_conflict')),
  'max_tries='||coalesce(max((target_state->>'attempts')::int), 0),
  'oldest_due_s='||coalesce(extract(epoch FROM now() - min(not_before) FILTER (WHERE dirty_version > completed_version AND not_before <= now()))::int, 0)
FROM live_activity_delivery;
SELECT 'history_1h', event, count(*) FROM live_activity_history WHERE recorded_at > now() - interval '1 hour' GROUP BY event ORDER BY event;
SQL
}
# Registrations and card tokens per phone.
lap_tokens() {
  db_psql -At -F ' ' <<'SQL'
SELECT 'device', left(device_id::text, 8), environment, CASE WHEN revoked THEN 'revoked' ELSE 'live' END,
  CASE WHEN push_token IS NULL THEN 'no-start-token' ELSE 'start-token' END, registration_version
FROM live_activity_devices ORDER BY 2, 6 LIMIT 20;
SELECT 'card', left(dv.device_id::text, 8), t.kind, t.state, CASE WHEN t.server_retired THEN 'server-retired' ELSE '-' END,
  CASE WHEN t.token IS NULL THEN 'cleared' ELSE 'token' END, t.token_version
FROM live_activity_tokens t JOIN live_activity_devices dv ON dv.registration_id = t.registration_id
ORDER BY 2, 3 LIMIT 50;
SQL
}
# Send attempts since a UTC mark (take the mark with: mark=$(date -u +%Y-%m-%dT%H:%M:%SZ)):
# time device kind event class status reason. The row is written immediately
# before the send (its time is not Apple's acceptance time); APNs' answer is
# added when it arrives. `unanswered` is a send in flight, or one whose process
# stopped before the answer was saved (ambiguous).
lap_events() {
  db_psql -At -F ' ' -v since="$1" <<'SQL'
SELECT to_char(h.recorded_at AT TIME ZONE 'UTC', 'HH24:MI:SS.MS'), coalesce(left(dv.device_id::text, 8), 'gone'),
  coalesce(d.kind, '-'), h.event, coalesce(h.result, 'unanswered'), coalesce(h.http_status::text, '-'), coalesce(h.reason, '-')
FROM live_activity_history h
LEFT JOIN live_activity_delivery d ON d.job_id = h.job_id
LEFT JOIN live_activity_devices dv ON dv.registration_id = d.registration_id
WHERE h.recorded_at >= :'since'::timestamptz
ORDER BY h.recorded_at LIMIT 50;
SQL
}
# APNs' answers since a UTC mark, counted by event, class, HTTP status and reason.
# Anything but `accepted` (and `unanswered` for a send still in flight) needs reading.
lap_results() {
  db_psql -At -F ' ' -v since="$1" <<'SQL'
SELECT event, coalesce(result, 'unanswered'), coalesce(http_status::text, '-'), coalesce(reason, '-'), count(*)
FROM live_activity_history WHERE recorded_at >= :'since'::timestamptz
GROUP BY 1, 2, 3, 4 ORDER BY 1, 2, 3, 4 LIMIT 50;
SQL
}
# Each job's state now: device kind generation status pending tries retry dismissed version apns_ts open not_before
lap_jobs() {
  db_psql -At -F ' ' <<'SQL'
SELECT left(dv.device_id::text, 8), d.kind, left(coalesce(d.target_state->>'generation_id', '-'), 8),
  coalesce(d.target_state->>'status', '-'), coalesce(d.target_state->>'pending_event', 'none'),
  coalesce(d.target_state->>'attempts', '0'),
  CASE WHEN d.target_state ? 'retry_at' THEN 'retry' ELSE '-' END,
  CASE WHEN d.target_state->>'dismissed_generation' = d.target_state->>'generation_id' THEN 'dismissed' ELSE '-' END,
  coalesce(d.target_state->>'presentation_version', '0'), coalesce(d.target_state->>'last_timestamp', '0'),
  CASE WHEN d.dirty_version > d.completed_version THEN 'open' ELSE 'done' END,
  to_char(d.not_before AT TIME ZONE 'UTC', 'HH24:MI:SS')
FROM live_activity_delivery d JOIN live_activity_devices dv ON dv.registration_id = d.registration_id
ORDER BY 1, 2 LIMIT 50;
SQL
}
# App log: levels, route/status, worker lifecycle and a possible-token count. Never raw lines.
lap_logs() {   # $1 = docker --since value, e.g. 10m
  docker logs --since "$1" baby-tracker-sync-app-1 2>&1 | jq -rR '(fromjson? | .level) // "NON-JSON"' | sort | uniq -c
  docker logs --since "$1" baby-tracker-sync-app-1 2>&1 | jq -rR 'fromjson? | select(.msg == "request completed") | "\(.route) \(.status)"' | sort | uniq -c
  docker logs --since "$1" baby-tracker-sync-app-1 2>&1 | jq -rR 'fromjson? | select((.msg // "") | startswith("live activity")) | "\(.msg) \(.class // .code // "") \(.status // "") \(.reason // "")"' | sort | uniq -c
  echo "hex64_lines=$(docker logs --since "$1" baby-tracker-sync-app-1 2>&1 | grep -cE '[0-9a-f]{64,}')"   # must be 0
  docker inspect --format 'restarts={{.RestartCount}} health={{.State.Health.Status}}' baby-tracker-sync-app-1
}
```

**Proposed health thresholds** while push is enabled. These are this runbook's choices; adjust them in the approval, not on the day. Check every minute for the first 30 minutes.

- `stale_leases` > 0 on two consecutive checks.
- `retrying` > 0 for more than 5 minutes.
- `parked` > 0: a job failed on its own data; read `live activity job parked` codes in `lap_logs`.
- Any `invalidToken` or `configurationFailure` in `lap_results` (see STOP 6 for what each means).
- `max_tries` ≥ 5.
- `oldest_due_s` > 120.
- `restarts` above its value right after start.
- `hex64_lines` > 0.
- More `stopped` than `resumed` worker lines.
- Any `ERROR` line, or any 5xx on `/v1/live-activities/…`.

Each one is a STOP: do [X-1](#x-1-disable-delivery-preferred) first, then investigate.

### Reading a job

Every send attempt is a `lap_events` row, and APNs' answer is recorded on it: the class, the HTTP status and Apple's reason when Apple gave an allowlisted one (`Server/internal/liveactivity/worker.go`, `queue.go`; `app.live_activity_target`). Read the class there directly; `lap_jobs` then shows what the worker did with it:

| Class in `lap_events` | What follows in `lap_jobs` / `lap_tokens` |
| --- | --- |
| `accepted` 200 | `pending` = `none`, `tries` = `0`. Apple took it; this is not proof of delivery. After a start, `status` stays `startUnconfirmed` until the new card registers its token. |
| `retryable` 429/5xx | `pending` still set, `retry` present, `tries` ≥ 1; retried with backoff (at most a minute apart). |
| `ambiguous` (no status), or `unanswered` | No answer arrived. An update or end retries. A start stays `startUnconfirmed` with `tries` ≥ 1 and is never repeated blindly; one retry follows a foreground absence proof. |
| `configurationFailure` 4xx | As ambiguous: a start stays `startUnconfirmed`, an update or end retries. No token is cleared and no generation fenced. See STOP 6 for the reasons. |
| `invalidToken` 400 `BadDeviceToken` / 410 `Unregistered` | That token is `cleared` (a card) or the device shows `no-start-token` (a start); after an update or end the job also shows `dismissed`. |

`lap_jobs` also shows a job parked on its own failure: `lap_queue` counts it as `parked` and its `last_reason` is `dispatch_failed`. A `last_reason` of `transient_conflict` is a job that lost a database race (SQLSTATE class 40) and is claimed again after the shortest backoff; one that persists across runs is a STOP like a parked job.

`apns_ts` (`last_timestamp`) is the Unix second the server stamped on the payload. `version` (`presentation_version`) is the card's server revision. Record both.

## R1 Server upgrade to schema 28, push off

Needs approval. The app stays stopped only from R1-6 step 1 to step 4. The phones keep their offline queue meanwhile.

### R1-1 Read-only preflight

```sh
"$src/Server/scripts/verify-unraid-deployment.sh" verify
server_json | jq '{source_version, instance_id, recovery_epoch, capabilities}'
epoch=$(server_json | jq -r .recovery_epoch)
[ "$(server_json | jq -r .source_version)" = "$old_sha" ] &&
[ "$(server_json | jq -r .instance_id)" = <instance-uuid> ] &&
[ "$(schema)" = 27 ] &&
[ "$(docker inspect --format '{{.Config.Image}}' baby-tracker-sync-app-1)" = "baby-tracker-server:$old_sha" ] &&
[ "$(revision "baby-tracker-server:$old_sha")" = "$old_sha" ] &&
[ "$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')" = linux/amd64 ] &&
echo "R1-1: PASS (epoch $epoch)" || echo 'STOP: preflight'
```

On the Mac, check the running binary's exact-schema compatibility before anything else:

```sh
git show "$old_sha:Server/internal/migrate/compatibility.json" | jq -c '{min,max}'   # must be {"min":27,"max":27}
```

- Result: source `<old_sha>`, schema `<27>`, epoch `<…>`, old manifest `<{"min":27,"max":27}>`.
- *A 26 means format cleanup has not shipped. STOP: run its own runbook first.*
- *The old binary cannot read schema 28. That is why [X-4](#x-4-binary-rollback-not-proposed) is not proposed.*

### R1-2 Backup

The dump and the ledger/head tar are taken back to back while the app runs; they feed the rehearsal (R1-3b). Take them while the phones are idle: a security event (a sign-in, a membership change) between the two would make the rehearsal's readiness check fail (R1-3b). To retake the pair, first move the stale one aside — the guard below refuses while either file exists: `mv "$dump" "$dump.stale" && mv "$ledger_pre" "$ledger_pre.stale"` — then run this block again.

```sh
[ ! -e "$dump" ] && [ ! -e "$ledger_pre" ] &&
docker exec baby-tracker-sync-db-1 pg_dump -Fc -U postgres -d babytracker > "$dump" &&
tar -C "$BT_DEPLOY_ROOT" -cf "$ledger_pre" security-ledger ledger-head &&
sha256sum "$dump" "$ledger_pre" &&
docker exec -i baby-tracker-sync-db-1 pg_restore --list < "$dump" | wc -l
```

**Backup and restore requirements:**

- Dumps are `0600` (`umask 077`) and never leave the Unraid host. The rehearsal runs on the host for that reason.
- Keep this dump, its ledger tar, the R1-6 cutover dump and the cutover ledger tar at least until acceptance (A) closes.
- A database restore must be coordinated with `security-ledger/` and `ledger-head/` from the same stopped point (Server/README.md, "Network, state and backups"). That is why R1-6 tars them next to the cutover dump.
- The restore path (a fresh database created by the init scripts, then `pg_restore --single-transaction --exit-on-error`, then the `init-users.sh` rerun) is the format-cleanup S-7 procedure; R1-3b rehearses it for 27 and the upgrade to 28 on this very dump.

| Dump | When | SHA-256 | `pg_restore --list` entries |
|---|---|---|---|
| `backup.pre-live-activity-push-<yyyymmdd>.dump` | Before R1 | `<…>` | `<…>` |
| `backup.pre-live-activity-push-<yyyymmdd>.ledger.tar` | Right after the dump | `<…>` | — |

### R1-3 Stage the reviewed source

On the Mac:

```sh
git archive --format=tar "$new_sha" Server > "/path/to/bt-source-$short.tar" && shasum -a 256 "/path/to/bt-source-$short.tar"
```

Upload it as `$BT_DEPLOY_ROOT/source-$short.tar` (approval-gated), then on Unraid:

```sh
sha256sum "$BT_DEPLOY_ROOT/source-$short.tar"     # must equal the Mac's
[ ! -e "$staged" ] && mkdir "$staged" && tar -x -C "$staged" -f "$BT_DEPLOY_ROOT/source-$short.tar" &&
[ "$(jq -c '{min,max}' "$staged/Server/internal/migrate/compatibility.json")" = '{"min":28,"max":28}' ] &&
grep -q baby_tracker_live_activity "$staged/Server/deploy/postgres/init-roles.sql" &&
[ -f "$staged/Server/deploy/unraid/live-activity-push.yaml" ] && echo 'STAGED: OK'
```

### R1-3b Rehearse the upgrade on the backup (STOP gate)

Before anything in production changes, run the whole R1 cutover against the R1-2 backup, on the Unraid host, in a disposable compose project of its own (`bt-rehearse-…`, no published ports except the app's loopback port, nothing named `baby-tracker-sync`). `Server/scripts/rehearse-live-activity-upgrade.sh` restores the dump into a fresh PostgreSQL created by the **deployed** tree's init scripts (as production's db was), records every table's row count and content digest, then performs R1-6 steps 2–4 with the **staged** tree exactly as written below — `up -d --no-deps --force-recreate --wait db`, the `init-users.sh` rerun and its role check, `run --rm --no-deps migrate`, schema exactly 28 with the contiguity and digest checks — and verifies on top: every pre-existing table unchanged in rows and digest, the effective privileges exactly the manifest's (an `init-users.sh` rerun on 28 and `check-principals.sh`), the exact role and membership inventories, the deployed image's `migrate` refusing 28 (`database_newer_than_binary: version 28`) and its server staying `/readyz` 503, the new image ready with push off (`/readyz` 200, the dump's instance and epoch, no `live-activity-push-v1`), then R2's database half (a generated worker password mounted, `init-users.sh` rerun, `live-activity-preflight --environment sandbox --skip-apple` passing its worker-database check with a disposable key that is nobody's credential). It needs the deployment's `actor_id_pepper` because readiness binds to it; it reads the file and never prints it. It generates every other secret for the run. Everything it creates is removed on exit, success or failure, and it fails if anything is left behind.

```sh
"$staged/Server/scripts/rehearse-live-activity-upgrade.sh" --dump "$dump" --ledger-tar "$ledger_pre" \
  --actor-id-pepper "$BT_DEPLOY_ROOT/secrets/actor_id_pepper" \
  --old-tree "$src" --old-image "baby-tracker-server:$old_sha" --new-tree "$staged" \
  --work-dir "$BT_DEPLOY_ROOT" \
  > "$BT_DEPLOY_ROOT/rehearsal-$short.log" 2>&1; echo "exit=$?"
tail -n 3 "$BT_DEPLOY_ROOT/rehearsal-$short.log"
grep -c 'STOP:' "$BT_DEPLOY_ROOT/rehearsal-$short.log"
```

- `--old-tree "$src"` is the deployed source (R1-4 has not moved it yet); `--old-image` is the running image, so the old tree is not rebuilt. The new image is built from `$staged` under a run-specific tag and removed afterwards; R1-5 then builds production's own `baby-tracker-server:$new_sha` from `$src` with the real `BT_SOURCE_SHA`, a separate build (its layers may or may not be cached; the proof is R1-5's, not this one). The run's temporary directory (secrets, a copy of the ledger, logs) goes under `$BT_DEPLOY_ROOT` because Unraid's `/tmp` is RAM-backed; it is removed on exit.
- **Expected:** exit `0`; the last lines `REHEARSAL: PASS (schema 27 -> 28, roles and privileges exact, old image refused, new image ready with push off, preflight database checks passed, care rows unchanged)` and `CLEANUP: removed containers, volumes, network, built images and the temporary directory of run …`; `0` STOP lines. Record the log's `restored:`, `rows:`, `OLD IMAGE:`, `NEW IMAGE:` and `PREFLIGHT:` lines (counts, hashes and identifiers only): `<…>`.
- **Any other outcome is `STOP: rehearsal`.** Nothing in production has changed. The `STOP:` line names the failed step:
  - `pg_restore failed` or `restored schema is …`: the backup is not a complete schema-27 dump; retake R1-2.
  - `init-users.sh rerun failed` / `roles after the rerun are …`: the staged init scripts do not fit this database; do not continue to R1-6.
  - `migrate failed`, `schema after migrate is …`, `digest of 28 differs`: the staged tree is not the reviewed one; compare `$staged` with `$new_sha` on the Mac.
  - `rows changed across migration 28` / `care rows changed during the rehearsal`: migration 28 touched care data on this database. Escalate; do not run R1-6.
  - `new app never became ready; security verify: …`: `actor_id_key_mismatch` means the pepper file is not the deployment's; `trusted recovery manifest mismatch` or another chain error means the ledger tar and the dump disagree (a security event between them). Move the stale pair aside as R1-2 says, retake it while the phones are idle and rerun; if it still fails, STOP and escalate.
  - `live-activity-preflight --skip-apple failed`: the worker login or its inventory is wrong in the staged tree; R2 would fail the same way.
  - `CLEANUP: FAIL (left behind …)`: remove the named `bt-rehearse-…` resources by hand (`docker ps -a`, `docker volume ls`, `docker network ls`, `docker image ls --filter 'reference=bt-rehearse-*'`); they are disposable and never production's.
- On the Mac, `Server/scripts/rehearse-live-activity-upgrade.sh --synthetic` runs the same procedure against a schema-27 database built from `8032e79` and seeded through its API; it is the P0 evidence that the tool itself works, not a substitute for this step.

### R1-4 Rollback set, then the new source and config

This is the format-cleanup S-5 block. The old source moves aside and its config is copied; nothing below edits these rollback copies.

```sh
[ ! -e "$rollback_src" ] && [ ! -e "$rollback_env" ] &&
mv "$src" "$rollback_src" && cp -p "$BT_DEPLOY_ROOT/deployment.env" "$rollback_env" &&
cp -a "$staged" "$src" &&
BT_SOURCE_SHA="$new_sha" BT_ALLOW_SOURCE_UPDATE=1 "$src/Server/scripts/provision-unraid-secrets.sh" &&
grep -qx "BT_SOURCE_SHA=$new_sha" "$BT_DEPLOY_ROOT/deployment.env" &&
grep -qx "BT_IMAGE_NAME=baby-tracker-server:$new_sha" "$BT_DEPLOY_ROOT/deployment.env" &&
diff <(grep -v '^BT_SOURCE_SHA=\|^BT_IMAGE_NAME=' "$rollback_env" | sort) \
     <(grep -v '^BT_SOURCE_SHA=\|^BT_IMAGE_NAME=' "$BT_DEPLOY_ROOT/deployment.env" | sort) &&
compose config -q && echo 'CONFIG: OK'
```

### R1-5 Build and image proof

The app keeps running the old image. Unraid builds natively for its `linux/amd64` engine.

```sh
compose build app migrate &&
[ "$(revision "baby-tracker-server:$new_sha")" = "$new_sha" ] &&
[ "$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "baby-tracker-server:$new_sha")" = linux/amd64 ] &&
[ "$(revision "baby-tracker-server:$old_sha")" = "$old_sha" ] &&
[ "$(docker inspect --format '{{.Config.Image}}' baby-tracker-sync-app-1)" = "baby-tracker-server:$old_sha" ] &&
docker image inspect --format '{{.Id}} {{.Os}}/{{.Architecture}} {{index .Config.Labels "org.opencontainers.image.revision"}}' \
  "baby-tracker-server:$new_sha" "baby-tracker-server:$old_sha"
bin="$BT_DEPLOY_ROOT/bt-server-$short.bin"
cid=$(docker create "baby-tracker-server:$new_sha") &&
docker cp "$cid:/usr/local/bin/baby-tracker-server" "$bin"; docker rm "$cid" >/dev/null
sha256sum "$bin"                                   # must equal P0's binary SHA-256
grep -a -c '__identitytest' "$bin"                 # must print 0
grep -a -c 'identitytest.invalid' "$bin"           # must print 0
grep -a -c 'api.sandbox.push.apple.com' "$bin"     # at least 1
rm -f "$bin"
```

- Image proof: new `<image id>`, `linux/amd64`, label `<new_sha>`; retained `<image id>`, label `<old_sha>`.
- Binary SHA-256 `<…>`; P0's was `<…>`.

### R1-6 Cutover

**Step 1.** Stop only the app, then take the cutover dump and the ledger/head tar at one stopped point.

```sh
[ "$(docker inspect --format '{{.Config.Image}}' baby-tracker-sync-app-1)" = "baby-tracker-server:$old_sha" ] &&
compose stop app &&
docker exec baby-tracker-sync-db-1 pg_dump -Fc -U postgres -d babytracker > "$cutover" &&
sha256sum "$cutover" &&
docker exec -i baby-tracker-sync-db-1 pg_restore --list < "$cutover" | wc -l &&
tar -C "$BT_DEPLOY_ROOT" -cf "$ledger_tar" security-ledger ledger-head &&
sha256sum "$ledger_tar" && echo 'CUTOVER BACKUP: OK'
```

**Step 2.** Install the new roles. Migration 28 grants to `baby_tracker_live_activity`, which only the new `init-roles.sql` creates. The db container still bind-mounts the init scripts of the source it was created from, so recreate it first.

`init-users.sh` runs as one transaction. Without the overlay, the worker login stays passwordless, so it cannot log in.

```sh
[ -s "$cutover" ] && [ -s "$ledger_tar" ] &&
compose up -d --no-deps --force-recreate --wait db &&
compose exec -T db grep -q baby_tracker_live_activity /docker-entrypoint-initdb.d/10-init-roles.sql &&
compose exec -T -u postgres db /docker-entrypoint-initdb.d/20-init-users.sh &&
[ "$(db_psql -At -F ' ' -c "SELECT rolname, rolcanlogin, rolpassword IS NULL FROM pg_authid WHERE rolname IN ('baby_tracker_live_activity','baby_tracker_live_activity_app') ORDER BY rolname" | tr '\n' ';')" = \
  'baby_tracker_live_activity f t;baby_tracker_live_activity_app t t;' ] &&
[ "$(schema)" = 27 ] && echo 'ROLES: OK (worker login passwordless)'
```

**Step 3.** Migrate. Only migration 28 applies.

```sh
[ "$(schema)" = 27 ] &&
compose run --rm --no-deps migrate &&
[ "$(schema)" = 28 ] &&
[ "$(db_psql -At -c 'SELECT count(*)=current_version AND min(version)=1 AND max(version)=current_version FROM schema_migrations CROSS JOIN schema_compatibility WHERE singleton GROUP BY current_version')" = t ] &&
[ "$(db_psql -At -c 'SELECT sha256 FROM schema_migrations WHERE version = 28')" = 43b37464fd4570ae65eb2a84e3579e7e76dbc9e6c4b4ca3d598a2a74f3bc521e ] &&
echo 'SCHEMA 28: OK'
```

**Step 4.** Start the app with push off.

```sh
[ "$(schema)" = 28 ] && compose up -d --no-deps --wait app
```

*On a STOP:*

- *Before step 1:* nothing in production changed. Do [X-3](#x-3-r1-failed-before-schema-28) if R1-4 ran.
- *At step 2 or 3, with the schema still 27:* `init-users.sh` and migration 28 are each one transaction. Confirm it first:

  ```sh
  [ "$(schema)" = 27 ] && [ "$(db_psql -At -c 'SELECT count(*) FROM schema_migrations WHERE version = 28')" = 0 ] && echo 'SCHEMA 27 UNTOUCHED'
  ```

  Then do X-3. If step 2 committed, the two new roles remain. They are cluster-wide, own nothing and are not in the old binary's checks, but the old binary's readiness with them present is unverified. If `/readyz` is not 200 after X-3, STOP and escalate; do not drop roles unreviewed.
- *After step 3 (schema 28):* the old binary refuses schema 28. Fix forward on the new image; push is off anyway. See [X-4](#x-4-binary-rollback-not-proposed).

| Artefact | SHA-256 | Entries |
|---|---|---|
| `backup.cutover-<short>.dump` | `<…>` | `<…>` |
| `backup.cutover-<short>.ledger.tar` | `<…>` | — |

### R1-7 Verify (push off)

```sh
"$src/Server/scripts/verify-unraid-deployment.sh" verify &&
[ "$(server_json | jq -r .source_version)" = "$new_sha" ] &&
[ "$(server_json | jq -r .instance_id)" = <instance-uuid> ] &&
[ "$(server_json | jq -r .recovery_epoch)" = "$epoch" ] &&
[ "$(server_json | jq -r '.capabilities | index("live-activity-push-v1") == null')" = true ] &&
echo 'R1: PASS (schema 28, push off)'
lap_queue          # jobs=0 … (no registrations can exist while push is off)
lap_logs 10m       # level/status counts only; hex64_lines=0; restarts unchanged
```

- [ ] Both phones sync as before (old builds): log one test diaper on each and see it on the other: `<…>`.
- Result: `<R1: PASS …>`; log counts `<…>`.

## D Phone builds (approval-gated; needs P1 and P2)

Build from a clean checkout of the reviewed commit that includes P2. The deployed server tree must match: `git diff --quiet "$new_sha" "$phone_sha" -- Server`.

```sh
phone_sha=<40-character reviewed sha including P2>
git worktree add /path/to/bt-lap-phone "$phone_sha" && cd /path/to/bt-lap-phone &&
[ -z "$(git status --porcelain)" ] && git diff --quiet "$new_sha" "$phone_sha" -- Server &&
xcodebuild -scheme BabyTracker -configuration Debug -destination 'generic/platform=iOS' \
  -derivedDataPath /path/to/bt-lap-phone-build -quiet build
```

### D-3 Signed entitlement and resolver input

```sh
app=/path/to/bt-lap-phone-build/Build/Products/Debug-iphoneos/BabyTracker.app
codesign --verify --deep --strict --verbose=2 "$app"
scripts/live-activity-push-signing.sh check --app "$app"
shasum -a 256 "$app/BabyTracker"
```

`check --app` runs `codesign -d --entitlements - --xml` on the app and the widget, decodes `embedded.mobileprovision`, and applies the resolver's rules (`ProvisioningProfileEnvironmentResolver`: simulator → sandbox; no embedded profile on a device build → production; otherwise the profile's `aps-environment`, anything else → `nil`).

- **Expected.** `codesign aps-environment: development`, `embedded profile: "BabyTracker Development - TEAM123456" (<uuid>), … aps-environment: development`, `resolver: sandbox (embedded profile aps-environment development)`, `widget aps-environment: none (expected; …)`, and `result: ready`, exit 0. The application-groups and keychain-access-groups lines must still name `group.com.olhapi.babytracker`. `check --app` repeats the project check first, so its `signing identity: "…" -> ok` lines appear as in P1.
- **Blockers the script raises itself:** `embedded profile "…" (<uuid>) is not the app target's profile "BabyTracker Development - TEAM123456"` (the bundle was signed with some other profile, such as an automatic team profile), `codesign (…) and embedded profile (…) disagree`, or `… is signed without aps-environment; push stays inert on this build`. Each is exit 1.
- **The runtime proof is A1:** each phone's registration shows `sandbox` on the server.
- *Any other combination is a STOP. Do not install.*

### D-4 Install

Install over the existing app. **Never uninstall:** an uninstall discards the phone's local history.

```sh
xcrun devicectl list devices
xcrun devicectl device install app --device <Phone A id> "$app"
xcrun devicectl device install app --device <Phone B id> "$app"
```

- [ ] With push still off on the server, both phones behave as before. A feed started on each phone shows its own local Live Activity, and the other phone's card appears through ordinary foreground sync.
- [ ] `lap_tokens` prints nothing: no registration happens while the capability is absent.
- Record: build exit `<0>`, `codesign` `<ok>`, entitlement `<development>`, profile `<development>`, executable SHA-256 `<…>`, installs `<ok/ok>`.

## R2 Enable push (approval-gated; needs R1, D and the P1 key)

### R2-1 Inputs

- The `.p8` file, uploaded to an operator-chosen transient path on the Unraid host outside `$BT_DEPLOY_ROOT/source*`: `apns_key_source=<path>`. It is copied over SSH, never pasted or passed as an argument.
- The Team ID and Key ID from P1. They are not secrets.

### R2-2 Provision the three secret files

`Server/scripts/provision-live-activity-secrets.sh` (as root, like `provision-unraid-secrets.sh`) generates the worker password on the host, builds the worker URL from it through the shell builtin `printf`, installs the `.p8` from the path argument, and sets the owner and mode each consuming container needs: `live_activity_password` `70:70 0600` (PostgreSQL), `live_activity_database_url` and `apns_key.p8` `65532:65532 0600` (the app). No secret value appears in a process argument list or in its output. It refuses a root without a `deployment.env` naming it, a symlinked root, a key that is not an unencrypted PEM P-256 private key, and any overwrite without `--force`; a rerun with the same inputs is a no-op. `--dry-run` prints the actions only.

```sh
secrets="$BT_DEPLOY_ROOT/secrets"
[ -f "$apns_key_source" ] && [ ! -L "$apns_key_source" ] &&
"$src/Server/scripts/provision-live-activity-secrets.sh" --dry-run --apns-key "$apns_key_source"
```

- Expected: `DRY RUN`, then `would create … live_activity_password (owner 70:70, mode 0600)`, `… live_activity_database_url (owner 65532:65532, mode 0600)` and `would install … apns_key.p8 (owner 65532:65532, mode 0600)`. Anything else is a STOP: the root or the key is not what R2-1 says.

```sh
"$src/Server/scripts/provision-live-activity-secrets.sh" --apns-key "$apns_key_source" &&
[ "$(stat -c '%u:%g %a' "$secrets/live_activity_password")" = '70:70 600' ] &&
[ "$(stat -c '%u:%g %a' "$secrets/live_activity_database_url")" = '65532:65532 600' ] &&
[ "$(stat -c '%u:%g %a' "$secrets/apns_key.p8")" = '65532:65532 600' ] && echo 'R2-2: PASS'
```

- Expected: `created …`, `installed …`, `PUSH SECRETS: OK`, then `R2-2: PASS`. The script leaves the source copy in place; remove it after R2-4 passes: `rm -f "$apns_key_source"`.
- `--force` rotates the worker password and its URL together (and replaces the key): only with db recreated and `20-init-users.sh` rerun afterwards, as R2-6 does.

The worker credential must differ from the runtime, projector, export and maintenance credentials. A newly generated password guarantees that.

### R2-3 Identifiers and composition

```sh
cp -p "$BT_DEPLOY_ROOT/deployment.env" "$BT_DEPLOY_ROOT/deployment.env.before-push-$short" &&
! grep -q '^BT_APNS_' "$BT_DEPLOY_ROOT/deployment.env" &&
printf 'BT_APNS_TEAM_ID=%s\nBT_APNS_KEY_ID=%s\nBT_APNS_BUNDLE_ID=com.olhapi.babytracker\n' '<team id>' '<key id>' \
  >> "$BT_DEPLOY_ROOT/deployment.env" &&
compose_push config --quiet && echo 'PUSH CONFIG: OK'
```

`provision-unraid-secrets.sh` rewrites only `BT_SOURCE_SHA`/`BT_IMAGE_NAME`, so later runs keep these lines.

### R2-4 Key-arrival preflight against Apple (STOP gate)

`baby-tracker-server live-activity-preflight` runs in a one-off app container with the overlay, so it reads the same `BT_APNS_*`, mounted key and worker URL file the enabled server will. It validates the key file (regular, `0600`, P-256), the Team ID, Key ID and bundle/topic, signs the real provider JWT, and sends **one** quiet Live Activity update per selected environment over HTTP/2 to Apple's fixed host for a syntactically valid all-zero dummy token. Apple's answer is the verdict: `400 BadDeviceToken` means the key, Team ID and Key ID were accepted and the dummy token was refused. Apple does not document whether a bad token is rejected before or after the topic is checked, so a PASS proves the key and does not contradict the topic; **the definitive topic proof is the first accepted real send in R2-7/A2**, where `DeviceTokenNotForTopic` or `TopicDisallowed` is STOP 6. It prints no key, token or URL, and exits 0 only when every requested check passed. The database check is skipped here on purpose: the worker login has no password until R2-6 recreates db.

**Run it once.** Each preflight process mints a fresh provider JWT, and Apple allows one provider-token update per key per 20 minutes (`429 TooManyProviderTokenUpdates`). Wait at least 20 minutes before rerunning it, and do not run it while the enabled app is running (the app holds its own token).

```sh
compose_push run --rm --no-deps app live-activity-preflight --environment sandbox --skip-database && echo 'R2-4: PASS'
```

- **Expected** (the D build is development-signed, so its tokens are sandbox):
  ```
  [PASS] key file: regular file, mode 0600, P-256 private key
  [PASS] identifiers: team <team id>, key ID <key id>, bundle com.olhapi.babytracker
  [PASS] topic: com.olhapi.babytracker.push-type.liveactivity
  [PASS] apple sandbox: 400 BadDeviceToken - key, team ID and key ID accepted; topic not contradicted (a dummy token cannot prove it)
  [SKIP] apple production: not selected
  [SKIP] worker database: skipped by --skip-database
  RESULT: PASS
  ```
- A TestFlight or App Store build would need `--environment production` (or `both`); that path is out of scope here (Known limits).
- **Any other result is a STOP** before the outage. The line says what to fix:
  - `[FAIL] apple sandbox: 403 InvalidProviderToken|ExpiredProviderToken|Forbidden - key, team ID or key ID rejected`: the `.p8`, `BT_APNS_TEAM_ID` or `BT_APNS_KEY_ID` do not belong together. Check the key's ID in Apple's console against R2-3, and that the key is enabled for APNs.
  - `[FAIL] apple sandbox: 400 TopicDisallowed|BadTopic|DeviceTokenNotForTopic - bundle ID or topic rejected for this key`: `BT_APNS_BUNDLE_ID` is not the App ID this key may push to.
  - `[RETRY] apple sandbox: 429 TooManyProviderTokenUpdates - rate limited, retry later`: a provider token for this key was already minted within 20 minutes (an earlier preflight, or the app). Wait at least 20 minutes and rerun once. Do not loop. A plain `429 TooManyRequests` is Apple's general throttle: same handling.
  - `[FAIL] apple sandbox: no answer - connectivity (DNS, TCP, TLS or timeout)`: the container cannot reach `api.sandbox.push.apple.com:443`. From the host, `curl -sS -o /dev/null --max-time 10 -w '%{http_code} HTTP/%{http_version}\n' https://api.sandbox.push.apple.com/` tells a host-level failure (`000`) from a container-network one.
  - `[UNKNOWN] …`: record the status and reason and STOP; do not guess.
  - `[FAIL] credentials: …` names the misconfigured variable or file; the Apple lines are then skipped.
- Record the full output (it contains no secret): `<…>`.

### R2-5 Backup

Repeat the R1-2 block, naming the dump `backup.pre-push-enable-<yyyymmdd>.dump`: `<sha256>`, `<entries>`.

### R2-6 Enable

This is a short outage. Recreating db mounts the password; `init-users.sh` then sets the worker login's password server-side. Before the app starts, the database preflight (the worker database: schema exactly 28, the exact `baby_tracker_live_activity` inventory, and the shared-snapshot proof that the worker and runtime logins use one database) must pass; it is the same check activation performs, run where a failure costs nothing but the outage. It runs with `--skip-apple`: Apple was proven in R2-4, and a second provider JWT within 20 minutes would only hit Apple's token-update throttle.

```sh
compose stop app &&
compose_push up -d --no-deps --wait db &&
[ "$(compose_push exec -T db stat -c '%u:%g %a' /run/secrets/live_activity_password)" = '70:70 600' ] &&
compose_push exec -T -u postgres db /docker-entrypoint-initdb.d/20-init-users.sh &&
[ "$(db_psql -At -c "SELECT rolpassword IS NOT NULL FROM pg_authid WHERE rolname = 'baby_tracker_live_activity_app'")" = t ] &&
compose_push run --rm --no-deps migrate &&
[ "$(schema)" = 28 ] &&
compose_push run --rm --no-deps app live-activity-preflight --skip-apple &&
compose_push up -d --no-deps --wait app && echo 'APP STARTED WITH PUSH'
mark=$(date -u +%Y-%m-%dT%H:%M:%SZ)
for i in $(seq 1 12); do
  server_json | jq -e '.capabilities | index("live-activity-push-v1") != null' >/dev/null && { echo 'CAPABILITY: live-activity-push-v1'; break; }
  sleep 5
done
"$src/Server/scripts/verify-unraid-deployment.sh" verify
lap_logs 5m        # restarts unchanged; worker: no 'stopped'; hex64_lines=0
lap_queue
```

- **Preflight.** Expected: the three local PASS lines, `[SKIP] apple sandbox: skipped by --skip-apple`, `[PASS] worker database: schema 28, exact baby_tracker_live_activity inventory, same database as runtime` and `RESULT: PASS`. A `[FAIL] worker database: …` is a STOP with the app still stopped: `sqlstate_28P01` means the password the db mounted is not the one in the URL file (rerun R2-2 with `--force`, then this block from the top); `database_role_separation_invalid` means the URL names the wrong login or the wrong database, or the inventory is not exactly 28's; `live_activity_schema_unavailable` means the schema is not 28; `connection` means db is not reachable from the project network. On a STOP, `compose up -d --no-deps --wait app` restarts the app without the overlay (X-1) and nothing else has changed.
- **Startup.** The worst case is about 25 s; see Known limits.
- **The first sends may be throttled.** The app mints its own provider JWT on its first send. Within 20 minutes of R2-4's preflight, Apple may answer that send `429 TooManyProviderTokenUpdates`: the worker records it as `retryable` and retries with backoff; it is expected and clears on retry, so it is not a STOP (see R2-7). Do not rerun the preflight while the enabled app is running.
- **If the capability does not appear within 60 s,** or `restarts` rises (only malformed configuration exits the process), STOP and do X-1.
- **Startup failures that exit the process:** malformed configuration only (an invalid key, a mode other than `0600`, a wrong owner, missing APNs settings, a worker URL that does not parse).
- **Startup conditions that defer activation** (the process serves; `live activity worker activation deferred` with the code below; the capability stays absent until `live activity worker resumed`):
  - migrations or readiness not yet ready (`dependency_unavailable`, `live_activity_schema_unavailable`);
  - a worker privilege inventory or shared-database mismatch (`database_role_separation_invalid`);
  - a PostgreSQL connection budget shortfall (`live_activity_database_budget_exceeded`: the runtime, projector, export and recovery pool maxima + 2 must fit `max_connections` minus the reserved connections).

  A deferral still present at 60 s is a STOP: do X-1 and fix the named condition.

  Read only the levels and the worker lines with `lap_logs`. If a configuration message is needed, read the single first `ERROR` line's `msg` field: `docker logs baby-tracker-sync-app-1 2>&1 | jq -rR 'fromjson? | select(.level == "ERROR") | .msg' | head -n 1`. Configuration errors omit paths and contents by design.
- Result: capability `<present at +… s>`, verifier `<pass>`, `<lap_logs>`, `<lap_queue>`.

### R2-7 First sends (STOP gate)

Run this right after R2-6 once both phones have registered (A1), again right after A2's first start and update, and at the end of every acceptance session. Every send so far must have been accepted, and nothing may have cleared a token.

```sh
# The 429 TooManyProviderTokenUpdates exclusion holds only within 20 minutes of the
# R2-4 preflight (the provider-token throttle); a job still retrying after that is a STOP.
[ -z "$(lap_tokens | grep -E '^device [0-9a-f]+ [a-z]+ live no-start-token ')" ] &&
[ -z "$(lap_jobs | awk '$4 == "startUnconfirmed" && $6 >= 1')" ] &&
[ -z "$(lap_results "$mark" | grep -vE '^[a-zA-Z]+ (accepted|unanswered) |^[a-zA-Z]+ retryable 429 TooManyProviderTokenUpdates ')" ] &&
echo 'R2-7: PASS (every send accepted, no token cleared)' || { echo 'STOP: R2-7'; lap_results "$mark"; }
```

- `$mark` is R2-6's. An `unanswered` row is a send still in flight: rerun after 10 s, and treat one that stays `unanswered` as ambiguous.
- A live device with `no-start-token` had its push-to-start token cleared (`invalidToken` on a start). A card's token is `cleared` after every end as well, so for cards the recorded class in `lap_results` is the signal, not `lap_tokens`.
- `startUnconfirmed` with `tries` ≥ 1 is a start Apple did not accept: an accepted start resets `tries` to 0.
- `retryable 429 TooManyProviderTokenUpdates` rows within 20 minutes of the R2-4 preflight are the expected provider-token throttle (R2-6) and are excluded by the gate. They must be followed by an accepted retry: while `lap_jobs` still shows a job in `retry`, rerun R2-7 after its `not_before`. A job still retrying more than 20 minutes after the preflight, or any other `retryable` reason, is a STOP. The Diagnostics threshold `retrying > 0 for more than 5 minutes` does not apply to these rows during that window.
- **The first `accepted` send is the topic proof** the R2-4 preflight could not give: a `configurationFailure` with `DeviceTokenNotForTopic`, `TopicDisallowed` or `BadTopic` here means `BT_APNS_BUNDLE_ID` is wrong (STOP 6).
- On STOP, the class, status and reason say which mismatch it is (STOP 6). Do [X-1](#x-1-disable-delivery-preferred) first, then fix the configuration or the build.
  - **If any `BT_APNS_*` value or the key file changed, rerun [R2-4](#r2-4-key-arrival-preflight-against-apple-stop-gate) before re-enabling** — push off (X-1 done) and no enabled app running (a disabled app mints no provider JWT), at least 20 minutes after the last provider JWT was minted (the enabled app's last send, or the previous preflight) — and re-enable with R2-6 only on `R2-4: PASS`. R2-6's own preflight runs `--skip-apple`, so without this the corrected key or identifiers would first be exercised by a live send to a real token.
  - A build fix alone (no `BT_APNS_*` or key change) re-enables with R2-6 directly.
  - A cleared token comes back when the phone registers again (foreground the app).
- Result: `<R2-7: PASS …>` after A1 `<…>`, after A2 `<…>`.

## A Two-phone acceptance

### Ground rules

- **Proof.** Delivery is proven only by a card observed on the receiving phone. `lap_events` and an "accepted" job show that the server sent and Apple accepted (HTTP 200); that is not delivery.
- **Timings.** Times are observations, not a delivery SLA. Record at least three observations for the background and locked rows if time allows.
- **What to record.**
  - Card state in words: kind, phase, side, paused or break, controls present.
  - The phone's clock time.
  - Server: `source_version`, `lap_events` since the row's mark, then `lap_jobs`, read with [Reading a job](#reading-a-job).
- **What never to record.** Baby names, notes, amounts, tokens or payloads. Screenshots stay on the phones.
- **Clocks.** At the start, record `date -u` on Unraid and each phone's clock, and the offsets: `<…>`. Both phones keep automatic time on. Low Power Mode is off; note it if it is on.
- **Roles.** **A** = Phone A (owner, sender). **B** = Phone B (caregiver, receiver), unless a row says otherwise. Both run the D build, are signed in to the family, are on Tailscale, and have Live Activities allowed in iOS Settings.
- **Per row.** Take `mark=$(date -u +%Y-%m-%dT%H:%M:%SZ)` before the action, then `lap_events "$mark"`, `lap_jobs` and `lap_tokens` after it.
- **Results.** A row is **Pass**, **Fail** or **Observed**. Observed applies to OS-dependent behaviour such as a terminated app; record what happened.

### Rows

| # | Setup | Action | Expected |
|---|---|---|---|
| A1 | Push on. Open A first, then B, unlocked and in the foreground. | None. Wait about 10 s on each. | `lap_tokens`: one `live` `sandbox` device with `start-token` per phone. A's prefix is the first to appear; record both. There is no settings prompt. This matches D-3 (`codesign` = profile = `development` → resolver sandbox). |
| A2 | B in the foreground, app open. | A starts a Left breast feed in the app. | B: one card for the feed and no duplicate after B's app reconciles. Server: a `start` for B's job, then accepted; no `start` for A's device (A's own start is suppressed). |
| A3 | B in the background, unlocked. | A switches to Right in the same session, then pauses. | B's card updates in place: same card, no new start alert. One update per change. |
| A4 | B locked (after first unlock). | A resumes, then stops. | B's card shows the break. With no further writes, the break ends at **B's own** break limit; record the expected and the observed end. |
| A5 | B terminated (swiped from the app switcher). | A starts sleep; later A wakes the baby. | Observed: whether the push-to-start card appears on B, and whether it ends without B being opened. iOS may not launch a force-quit app to register the new card's token. Record; this is not a guarantee. |
| A6 | B with Tailscale disconnected and internet on. | A starts a feed, then switches side. B reconnects Tailscale and opens the app. | The start reaches B: Apple needs no private network. The new card's update token cannot register until B reconnects, so updates may lag. After the reconnect, the card catches up to the current state with no second start. |
| A7 | B offline (Tailscale off), locked, with a pushed card for A's running feed. | B taps Pause on the Lock Screen. | No write: `lap_events` shows nothing new within 60 s, and A's feed keeps running. iOS asks to continue in the app (`continueInForeground`). Once open, the app shows "Open Baby Tracker to refresh this activity before changing it." once, with OK. Reconnect afterwards. |
| A8 | B: iOS Settings → Baby Tracker → Live Activities off; open the app. | A edits the running feed. | B's device becomes `revoked` (the app's DELETE). No event for B's jobs. Then turn it back on and open the app: a new `registration_version` goes `live`, the current activity catches up, and there is no burst of starts. |
| A9 | B swipes away the card for A's running feed. | A pauses, then stops; later A starts a new feed. | No event for that generation on B (`lap_jobs` shows `dismissed`). The new feed gives B a new card with a new generation prefix. |
| A10 | Both phones show A's running feed. | Within about 1 s: A taps Stop on A's Lock Screen while B taps Pause on B's. Repeat with A starting sleep while B starts a feed. | Both phones converge on the same final state, with no resurrected card and no lasting duplicate. A transient duplicate may end through a `duplicateEnd` event. |
| A11 | A starts a timed bottle in the app; B locked. | B taps Stop on B's card. | B's card ends without unlocking. The bottle is saved at the baby's usual amount, and B's app offers "Set amount" the next time it is on screen. A's card ends or updates through push. (Known minor: the handoff alert and the sheet can collide.) |
| A12 | A (owner) changes B to viewer in Caregivers. | A starts a feed. B tries any control still shown on an older card. | B's new card has no write controls (`canWrite=false`). Nothing is written: refused with HTTP 403 `read_only_caregiver`, or a handoff. Restore B's role afterwards. |
| A13 | Push on. | `compose_push restart app`; A pauses while the server is down. Wait until `lap_logs 2m` shows `health=healthy`. | B receives the pause after the server is back: the worker resumes queued work. The capability returns. |
| A14 | B signed in. | B signs out in Settings. A then edits. | B's local cards are removed. B's device is `revoked` within about 60 s (minute sweep), and no event for B appears after the logout. Sign B back in: a new registration goes `live`. |
| A15 | Optional, disruptive, last. Needs the user's OK. | A removes B from the family; later re-invites B. | B's registration is revoked within about 60 s, with zero sends meanwhile. A rejoin starts fresh and pulls shared data only. |
| A16 | A unlocked, then repeat locked. | Siri: "Start left breast feeding in Baby Tracker", later "Stop feeding in Baby Tracker". | The write happens (record whether the app UI opened), and B receives the start, then the update or end. Task 10 changed `supportedModes` of shared intent types. |
| A17 | A's Home Screen QuickLogWidget. | Tap L, then R, then sleep. | Each runs without opening the app; B receives each change. |
| A18 | A online, the app freshly started. | A starts a feed in the app, locks, and within about 5 s taps Stop on **A's own** card, before any push to A's card (`lap_events` shows no event for A's device). Repeat with A's Tailscale off. | A's card reflects the stop immediately in both runs. If it changes only when a later push arrives, ActivityKit **does** floor at `Activity.request` device time (Task 9 concern; change confined to `LiveActivityPushState.phoneStamp`). Record it as a defect. |
| A19 | B has a pushed card. | Right after a push visibly updates B's card (A switches side), B taps Pause on the Lock Screen within about 1 s. | B's card shows the pause at once (ActivityKit accepted an equal or intra-second stamp) and stays consistent after the next push. If it changes only after the server's push, record "equal stamp not accepted". |
| A20 | Whenever a card stays visible on a Lock Screen in an ended state (for example iOS's 8-hour limit during a long sleep). | Unlock that phone and open the app. | `lap_jobs`: that phone's job is **not** `dismissed` unless the parent swiped the card away. A `dismissed` without a swipe means ActivityKit omits ended-but-visible cards from `Activity.activities` (Task 8 inference wrong). |
| A21 | B restarted, unlocked once, then locked and untouched. | A starts sleep; later A wakes the baby. | B gets the start alert while locked. `lap_tokens` shows a new `card … token` row for B **while B is still locked**: the ledger and the Keychain (`AfterFirstUnlockThisDeviceOnly`) are readable. The wake ends B's card while it is still locked. Variant: restart without unlocking; record what arrives before the first unlock. |
| A22 | Push on, both phones idle. | Do [X-1](#x-1-disable-delivery-preferred). Foreground both phones; A starts a feed. Then re-enable with R2-6. | While disabled: the capability is absent, no new `lap_events`, and A's own local card works as before push. After re-enabling: the capability is back, the phones re-register, and no start burst. |

### Results

| # | Window (UTC) | B state | Card observed (state, phone time) | `lap_events` (time device kind event) | `lap_jobs` after (class) | Result |
|---|---|---|---|---|---|---|
| A1 | `<…>` | `<…>` | `<…>` | `<…>` | `<…>` | `<…>` |
| … | | | | | | |
| A22 | `<…>` | `<…>` | `<…>` | `<…>` | `<…>` | `<…>` |

- `source_version` during acceptance: `<new_sha>`.
- `lap_queue` and `lap_logs 60m` at the end: `<…>`.

## X Disable and rollback

**Never** disable or roll back by dropping tables, running `DELETE` on `live_activity_*`, reversing a migration, or `compose down --volumes`, and never delete family data.

### X-1 Disable delivery (preferred)

Recreate only the app without the overlay. Schema 28, the registrations, the queue and the secret files all stay. `--no-deps` keeps db, and its mounted password, untouched.

```sh
compose up -d --no-deps --wait app &&
[ "$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' baby-tracker-sync-app-1 | grep -c '^BT_LIVE_ACTIVITY_PUSH_ENABLED=true$')" = 0 ] &&
[ "$(server_json | jq -r '.capabilities | index("live-activity-push-v1") == null')" = true ] &&
[ "$(schema)" = 28 ] &&
"$src/Server/scripts/verify-unraid-deployment.sh" verify && echo 'PUSH DISABLED (schema 28 kept)'
```

What disabling does:

- Disabled startup opens no worker pool, so nothing is sent. `PUT /v1/live-activities/*` is refused with 503, `DELETE /v1/live-activities/device/{id}` still succeeds, and ordinary sync is unaffected.
- Each phone withdraws from push management when its next probe sees the capability gone, and goes back to local Live Activities. A pushed card already on a Lock Screen is not ended by the server; the phone's next foreground pass reconciles it. A22 tests this.
- Queued rows stay; with the worker off, nothing writes history.
- Optionally move `deployment.env`'s `BT_APNS_*` lines out. Without the overlay they reach no container.
- If key compromise is suspected, revoke the key in Apple's console (approval).
- To re-enable, run R2-6 — after R2-4 first if any `BT_APNS_*` value or the key file changed since the last `R2-4: PASS` (see R2-7's STOP).

### X-2 Rollback of R2 configuration only

There is nothing to restore: X-1 is the rollback. Keep `deployment.env.before-push-$short` for reference.

### X-3 R1 failed before schema 28

These are the format-cleanup S-7 steps 2, 4 and 5, with 27 in place of 26. If the failure came before R1-6, the old app may still be running; the restart below is then a brief, harmless outage.

```sh
compose stop app
[ -d "$rollback_src" ] && [ -f "$rollback_env" ] && [ ! -e "$BT_DEPLOY_ROOT/source.$short-failed" ] &&
mv "$src" "$BT_DEPLOY_ROOT/source.$short-failed" && cp -a "$rollback_src" "$src" &&
cp -p "$rollback_env" "$BT_DEPLOY_ROOT/deployment.env" &&
grep -qx "BT_IMAGE_NAME=baby-tracker-server:$old_sha" "$BT_DEPLOY_ROOT/deployment.env" &&
[ "$(revision "baby-tracker-server:$old_sha")" = "$old_sha" ] && echo 'CONFIG RESTORED'
```

```sh
[ "$(schema)" = 27 ] &&
compose up -d --no-deps --force-recreate --wait db &&
compose up -d --no-deps --no-build --wait app &&
"$src/Server/scripts/verify-unraid-deployment.sh" verify &&
[ "$(server_json | jq -r .source_version)" = "$old_sha" ] && echo "ROLLBACK: OK ($old_short, schema 27)"
```

### X-4 Binary rollback (not proposed)

Checked against the tree:

- `$old_sha`'s manifest is exact, `{"min":27,"max":27}` for `main`.
- `ValidateCompatibility` rejects an applied migration it does not know: `migration_compatibility: unknown or changed migration 28`.
- It also rejects any version outside `[min,max]`.
- On schema 28 the old binary therefore never becomes ready.

A binary rollback would need the cutover dump restored into a fresh database. That loses every write after the cutover, and it needs `security-ledger/` and `ledger-head/` restored from `$ledger_tar` at the same point, with the README's epoch and reconciliation rules.

It is therefore **not proposed**. Consider it only:

- if nothing was written after the cutover;
- under a separately reviewed plan, adapted from format-cleanup S-7 and rehearsed for 27/28;
- with explicit approval.

Otherwise fix forward with push disabled (X-1).

## Still open

- **P1 Apple provisioning:** blocked on approval.
- **P2 switch commit:** after P1, written by `scripts/live-activity-push-signing.sh enable`; the script refuses until `check` is ready.
- **R1, D, R2, A:** each needs its own approval.
- **The rehearsal on the production backup (R1-3b)** has not run; only `--synthetic` has, on the Mac.
- **The production APNs environment** (TestFlight or App Store signing) is out of scope.
