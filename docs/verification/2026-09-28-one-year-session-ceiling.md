# One-year self-hosted session ceiling — 2026-09-28

The owner chose a one-year ceiling instead of the 100-year policy introduced during the [sign-in recovery](2026-09-28-owner-session-incident.md). A self-hosted session now expires 365 days after its original Apple sign-in, regardless of refresh activity. The app keeps its current session today. Explicit logout, revocation, recovery-epoch change, and refresh-token reuse still invalidate a session earlier.

## Existing sessions

The owner had already signed in under the 100-year policy. Changing the issuance default alone would leave that session valid for 100 years. Schema 20 adds a read-only lookup for the lineage root's creation time. Authentication and refresh use the earlier of the persisted deadline and the root time plus 365 days. Refresh returns that effective deadline to the phone. Existing session rows and the immutable security ledger are not rewritten.

## Verification

- The existing-session regression failed before enforcement: a 100-year session still refreshed after one year. It passed with the new lookup. New sessions refresh before their first anniversary and return `reauthentication_required` at the ceiling.
- The full PostgreSQL race suite, `go vet ./...`, and amd64/arm64 `make server-build` passed. The disposable `make server-smoke` passed migration, schema 20 readiness, exact privilege checks, and cleanup.
- The release source commit was `6aa153dce4ff9208ee018593ca98d406dfc4f90c`. Its `Server` archive SHA-256 matched locally and on Unraid: `d3cb9da21bc3f1b018a49d895ceb4db169c53d86d8fa7eaeff7894a4df994cf6`.

## Production rollout

- The schema-19 deployment verifier passed before rollout. A separate restricted rollback set was created under `/mnt/user/appdata/baby-tracker-sync`: `backup.pre-6aa153d.dump`, `source.90b6a34-rollback-6aa153d`, and `deployment.env.90b6a34-rollback-6aa153d`. `pg_restore --list` accepted the dump, whose SHA-256 was `220fd44cb513efd7d2d92936a7d94f06f37176a14df27c1346b90d7c599bdd09`.
- The new image label and staged Compose configuration passed. During cutover, `docker compose run migrate` consumed the remaining stdin of the remote shell script. Migration 20 succeeded, but the app remained stopped. A read-only check established schema 20 and the new source revision, then the staged app was started explicitly. The post-cutover verifier passed.
- Private `/v1/server` advertised the exact `6aa153d` revision. App health and private HTTPS readiness passed, the runtime role had the lineage lookup grant, no host ports opened, and unrelated containers matched their baseline. Schema was 20. Bounded app logs had zero WARN/ERROR lines.
- The latest owner session remained active. Its original sign-in was 2026-09-28 16:38:51 UTC, giving an effective reauthentication deadline of 2027-09-28 16:38:51 UTC. Active totals were unchanged across cutover: one family, two memberships, 49 feeds, 22 sleep entries, 13 diapers, and two supplements. No post-rollout physical-phone check was performed.
