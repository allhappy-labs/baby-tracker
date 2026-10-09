# History and family export rollout — 2026-09-28

## Source and local verification

- Scoped commit: `d3ccf2270637cfb2461a9202a11d152b5d5e3a11` on `fix/history-family-export`. Relative to the previous deployed `9ba399d`, the server diff changes only the export rate-limit storage-scope mapping and its database regression test.
- The regression against disposable PostgreSQL failed before the mapping change with `rate_limit_unavailable`, then passed. The affected `limits`, `httpapi`, and `export` Go suites and `go vet` passed.
- Focused Swift tests (46), iOS Simulator build, and three targeted UI tests passed. The History and leave-family screenshots were inspected; the leave sheet's initial detent was shortened after the first capture.
- Signed generic iOS build passed `codesign --verify --deep --strict`. The app and widget provisioning profiles contain both physical-device UDIDs.

## Production server

- Pre-rollout verifier passed at `9ba399d`: private HTTPS health/readiness 200, live socket 401 for a bogus bearer, no published host ports, and unchanged unrelated-container baseline.
- Restricted rollback artifacts on Unraid: `backup.pre-d3ccf22.dump`, `source.9ba399d-rollback`, and `deployment.env.9ba399d-rollback` under `/mnt/user/appdata/baby-tracker-sync`. `pg_restore --list` read the database dump.
- `git archive HEAD Server` SHA-256 was `83d62db24ea9d88d5fbff3768d642451b09064060be0d755b21d982aa009056d`, identical after transfer. The staged image label and running app-container label both report the full `d3ccf22` SHA.
- The app service alone was recreated. The database and Tailscale containers remained healthy, schema stayed at 19, and the post-rollout verifier passed all checks. A bogus bearer reached `POST /v1/export` over private HTTPS and received 401. Bounded app-container logs showed no WARN or ERROR lines.
- No owner export job existed at the last aggregate database check. A real authenticated owner request and download remain to be verified on the installed app.

## Phones

- `devicectl device install app` reported successful installation of `com.olhapi.babytracker` on `Phone A` and `Phone B`; both subsequent installed-app queries found the bundle.
- `devicectl device process launch` succeeded on `Phone A`. Marta's phone denied launch because it was locked; a second attempt returned the same denial. The user declined further agent-driven device checks.
- Installation and launch do not prove the History chips, leave confirmation, or export behavior on the physical devices. The targeted simulator captures cover the first two screens. The user elected to test export on their own iPhone; no authenticated live export or download is claimed here.
