# Owner session incident — 2026-09-28

The later [one-year session ceiling rollout](2026-09-28-one-year-session-ceiling.md) supersedes the 100-year policy described below. The details here record the incident and its earlier deployment as they occurred.

## Observed state before recovery

- The owner reported a sign-in prompt after the `d3ccf22` app install and server rollout. The supplied Caregivers screenshot shows the shared-family screen and “Sign in again to keep syncing,” not initial family setup.
- Before further sign-in or reset, `devicectl device copy from` preserved the owner iPhone's App Group data at `~/Documents/Baby Tracker Backups/owner-pre-reauth-2026-09-28` with owner-only permissions. Its five files matched the temporary receive byte for byte, and `PRAGMA quick_check` on the copied SwiftData store returned `ok`.
- The copy has a family session record, an active shared realm, an allowed ordinary-sync gate, two active babies, 48 active feeds, 20 sleep entries, 12 diapers, and 2 supplements. The outbox has 131 acknowledged, 2 uncertain feed-deletion, and 21 superseded timer mutations. This is a point-in-time copy, not proof that no older record was lost.
- Read-only production SQL found one active family, two active memberships, 50 active feeds, 20 sleep entries, 12 diapers, and 2 supplements. The owner sessions were past their `reauthenticate_at` deadlines, while the caregiver membership remained active. The deployed server revision was `d3ccf22`, with healthy app, database, and Tailscale containers.

## Cause and fix

- The self-hosted auth service assigned a hard 24-hour absolute session deadline. The update's app launch exposed the already-expired session. The previous rollout's code diff did not change auth lifetimes or clear device data.
- New self-hosted sessions retain 10-minute access tokens with single-use refresh rotation. Refresh and absolute deadlines share a distant 100-year protocol ceiling, so inactivity itself does not force another Apple prompt. Explicit logout, revocation, recovery-epoch changes, and refresh-token reuse still invalidate access. Hosted sessions retain their existing policy. The intermediate `a81f86c` rollout still imposed a one-year unused-device deadline; the follow-up removes it to match the owner's one-time sign-in requirement.
- Existing sessions retain their issued deadlines. A phone with an already-expired session needs one final Apple sign-in after the new server revision is deployed; the app's existing reauthentication path retains the shared realm and local records. If the screen instead offers family recovery or forfeiture, stop before selecting either action and investigate.

## Verification

- The new disposable-PostgreSQL regression failed before the server change because the issued session still had a 24-hour deadline, then passed after the change.
- The complete auth package and full server `test-db.sh` race/PostgreSQL suite passed; `go vet ./...` and the two-architecture `make server-build` passed. Seventy focused Swift session and sync tests passed. The owner later confirmed the physical-device Apple sign-in completed and History looks right.
- A follow-up regression reproduced the intermediate one-year inactivity expiry by advancing an unused session two years, then passed after `refreshTTL` was aligned with the 100-year absolute ceiling. The full `test-db.sh` race/PostgreSQL suite, `go vet ./...`, and amd64/arm64 `make server-build` passed again for `90b6a34`. The Swift follow-up edits change comments and a test suite label only; no app binary was updated in this rollout.

## Production rollout

- The pre-rollout verifier passed at `d3ccf22`: private HTTPS health/readiness 200, live socket route 401 for a bogus bearer, no host ports, and unchanged unrelated-container baseline. Schema was 19.
- Restricted rollback artifacts under `/mnt/user/appdata/baby-tracker-sync`: `backup.pre-a81f86c.dump`, `source.d3ccf22-rollback-a81f86c`, and `deployment.env.d3ccf22-rollback-a81f86c`. `pg_restore --list` read the dump; its SHA-256 was `b8d0f8d720290a4f1875b0ef09ce7c188d624320592b1eb3b5f1b65a50a2e6c6`.
- `git archive a81f86c Server` was transferred with matching SHA-256 `362fc358361bb7f7f8a6241f4d3635f602b5014b128e8b98e55abc7286224d36`. The staged Compose configuration passed validation and the built image's revision label matched `a81f86cad9477dc395fe542f7ee5bd4b56ecc2f2`.
- Only the app service was recreated. Post-rollout, the app was healthy, the verifier passed, `/v1/server` advertised the exact `a81f86c` revision over private HTTPS, and schema remained 19. The database still held one active family, two active memberships, 50 active feeds, 20 sleep entries, 12 diapers, and 2 supplements. Bounded app logs had zero WARN/ERROR lines.
- At the time of this intermediate rollout, the live Apple sign-in and resulting new session deadline had not yet been observed.
- Before the final rollout, the `a81f86c` deployment verifier passed. A separate restricted rollback set was created: `backup.pre-90b6a34.dump`, `source.a81f86c-rollback-90b6a34`, and `deployment.env.a81f86c-rollback-90b6a34`. `pg_restore --list` accepted the dump; SHA-256 was `3b35a2b73282e635f4882c764855a7ec1bdea9662c2159de25b943b2ed7b7681`.
- The exact `git archive 90b6a34 Server` SHA-256 was `cd4d46195dd79bdaa81a469be44d6bafd0eb9e0bb1b98f26051d45fb827af377` locally and on Unraid. The staged Compose configuration and image revision label passed validation. Only the app service was recreated. Post-rollout, the verifier passed, the app was healthy, and `/v1/server` advertised `90b6a34831acb2e59494a3c7005d2175570a0d3b` over private HTTPS. Schema remained 19; the database had one active family, two active memberships, 50 active feeds, 20 sleep entries, 12 diapers, and 2 supplements. Bounded app logs had zero WARN/ERROR lines.
- After signing in, the owner reported that History looks right. Read-only production SQL showed a new active owner session created at 2026-09-28 16:38:52 UTC, with refresh and reauthentication deadlines both at 2126-09-04 16:38:51 UTC. The server still advertised `90b6a34`.
- Active totals after sign-in were one family, two memberships, 49 feeds, 21 sleep entries, 13 diapers, and two supplements. Two feed rows were tombstoned after sign-in; their IDs exactly matched the two `feed/delete/inFlightOrUncertain` operations in the preserved pre-sign-in phone outbox. Three feed rows, one sleep row, and one diaper row had acceptance timestamps after sign-in. This supports resumed sync but does not prove every historical entry was ever present. The owner iPhone backup and both production rollback sets remain preserved.
