# Caregiver sync verification — 2026-09-26

## Status: AUTOMATED GATES CLOSED — TWO-PHONE MANUAL CHECK OPEN

Before this work the app never ran sync: family creation and invitations reached
the server, but no record ever left a phone. Plan
`docs/superpowers/plans/2026-09-25-caregiver-sync-wiring.md` wired it end to end
and was verified as below. The manual two-phone check has not been run yet.

## Automated evidence (source `a5c6948`, the deployed revision)

| Gate | Result |
| --- | --- |
| `make test` | `OK: 673 tests` |
| `make build` | `BUILD SUCCEEDED` |
| UI suite (dedicated simulator, erased) | `Executed 59 tests, with 0 failures` |
| `make server-test` | exit 0, 29 packages `ok`, 0 `FAIL` |
| `make server-smoke` | exit 0, cleanup proof clean |
| `make server-protocol-e2e` | exit 0; two-caregiver proof steps 1–13 |

The first `server-protocol-e2e` run failed in `internal/realtime`
`TestPingDeadlineAndGracefulThenForcedShutdown` ("socket lifetime sent no ping", a
40 ms timing check) while the UI suite was loading the machine. The package is
untouched by this branch. The re-run on an idle machine passed in full.

Two-caregiver steps proven against the real server image, with the app's own
composition, Keychain token path, Apple sign-in requests, engine and store:

1. Owner logs a baby (unicode name), a composite feed, a legacy feed, a sleep and a diaper while local-only.
2. Owner signs in through `GET /v1/server`, `POST /v1/auth/challenge` and `POST /v1/auth/apple` as a new account.
3. Owner creates the family, imports history, and renews to a family session.
4. Joiner signs in through the same path as a new device.
5. The joiner redeems a minted invitation link and takes the snapshot.
6. Joiner has the same baby, composite components, legacy feed upgraded to the server's components, sleep and diaper.
7. Owner's edit of the imported diaper reaches the joiner.
8. Joiner's bottle edit, diaper delete and nap start reach the owner (running timer, same start).
9. Owner stops the nap: no running timer and exactly one completed sleep on both phones.
10. Owner's feed timer, with a side switch and a bottle, is running on the joiner with the same components.
11. Joiner stops it: exactly one completed feed, same id and components, on both phones.
12. Owner's delete of the legacy feed reaches the joiner.
13. Joiner relaunches from the same Keychain and defaults and keeps syncing in both directions.

## Deployment evidence (Unraid, 2026-09-26)

- Source: `git archive a5c6948 Server`, sha256 `fef6028f…4a34e`, identical on both ends; previous source kept as `source.e3c5271-rollback` with `deployment.env.e3c5271-rollback`.
- The running app image `baby-tracker-server:a5c6948…` has revision label `a5c694872bbe3773e891867088b004c4318b397d`. The db and Tailscale sidecar were untouched.
- Schema `17`; authority `<instance-uuid>:1` (unchanged instance, so no re-confirmation on the phones).
- `verify-unraid-deployment.sh verify`: private HTTPS health/readiness 200; no host ports; unrelated container snapshot unchanged.
- From the Mac over Tailscale: `/healthz` 200, `/readyz` 200, `/v1/server` 200 with the same instance id and origin.
- App log since the restart: 0 ERROR/WARN lines.
- Tailnet policy: the sidecar's packet filter is allow-all within the tailnet, so `iphone181` (developer@) can reach `baby-sync`. That is broader than the plan's intended caregiver-only grant (follow-up).

## What automation cannot prove

- The hosted identity-test adapter stands in for real Apple sign-in. The self-hosted `identity_token_only` mode and the 24 h re-sign-in are covered by unit tests only.
- Tailscale TLS to real phones, Live Activity cards and background behaviour.

## Device checklist (for the owner and Marta)

- [ ] Both phones have this build (`Phone A` installed 2026-09-26; `Phone B` needs its device added to the signing profiles first).
- [ ] Owner: Settings → Caregivers → confirm server → name → Sign in with Apple → Create family. Status shows "Synced HH:MM".
- [ ] Owner: invite a caregiver (QR).
- [ ] Marta: "Join a family" from the first screen (or Settings) → confirm server → name → Sign in with Apple → scan QR → Join. Her Home shows the owner's baby and history.
- [ ] Log a feed on each phone; it appears on the other within about 20 s with the app open.
- [ ] Start a nap on one phone; the other shows it running with the same start; stop it on the other; one completed sleep on both.
- [ ] Force-quit and reopen both; they keep syncing.
- [ ] Airplane mode on one phone, log a diaper, turn airplane mode off; it arrives once.
