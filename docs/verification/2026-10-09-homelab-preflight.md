# Homelab preflight: the R1 baseline, pinned to a read-only script

This records the baseline that runbook R1's re-check compares against
before the cutover. No secret value and no row content is recorded here or
printed by the tooling below — only counts, presence flags, compose file
paths, and match/mismatch verdicts.

**History.** The coordinator's original read-only preflight (2026-10-09)
found the homelab at revision `998501b`, schema 28. While Task 1 was still
in flight, `main`'s baby-icons rollout (`main:docs/verification/2026-10-09-baby-icons-rollout.md`)
deployed `fce422d5` to the homelab, taking it to schema 29
(`000029_baby_icon.sql`) — the Task 1 review's E1 escalation caught this.
The revision 3 amendment re-vendored `legacy-chain/manifest.json` to the
29-row `fce422d5` history (Task 3's amendment) and re-pinned this document
to match. Fix round 2's re-review (C2) found that the read-only guard's
`PGOPTIONS` value was sent unquoted, so the remote shell split it and
every psql call would have failed "No such container" on the real
homelab — fixed, and re-proven through a POSIX shell, not a hand-retyped
string. **Fix round 3's re-review then found that fix round 2's own fix
for an absent app container (N1) had gone too far: an unreadable or empty
compose-files/image-revision label passed with exit 0 whenever the app
happened to be running** — fixed below (see "the read-only re-check tool,"
R2-C3), along with two other findings: the deployed revision itself is
now pinned (R2-I5), not merely cross-checked against itself, and the
household and compose-files gates are each narrower and more precise than
they were (the household ruling, and R2-N6). **Fix round 4's re-review
then found the compose expectation from fix round 3 still didn't match
how the homelab actually runs compose — on a healthy homelab the script
would have printed `compose_match=mismatch` and exited 3** — fixed below
(see "Compose files" and R3-C4), along with a second finding: a
connectivity blip on the one inspect that checks whether the app
container exists was misdiagnosed as a stopped app rather than a read
failure (R3-N8).

## The baseline (`fce422d5`, schema 29 — 2026-10-09)

Every fact below names its source: the coordinator's original morning
preflight, or `main`'s own rollout record. They split into two groups
(fix round 2, I4): **gating invariants**, where R1 must confirm every one
live and a difference is a STOP (apply the D2 re-pin rule — re-vendor the
manifest, re-pin `EXPECTED_SOURCE_SHA`, or treat the homelab as
`not_the_homelab_legacy_state`, per `legacyimport.Detect` — rather than
forcing the cutover through); and **informational facts**, which are
recorded and printed but are *expected* to have drifted since the morning
preflight through ordinary use, and so never gate anything.

### Gating invariants

- **Revision:** `fce422d5` (includes `main` at `6546a98c`). Source: rollout
  record. `git archive fce422d5 Server` has SHA-256
  `023b30701c99c7f33d54e1c200bdc7168f712a0742b2ebe45f702fe2cec040a3`. The
  script now pins this as `EXPECTED_SOURCE_SHA=fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6`
  (fix round 3, R2-I5): both `deployment.env`'s `BT_SOURCE_SHA` and the
  running image's own `org.opencontainers.image.revision` label must equal
  it, not merely equal each other. **A new deploy changes `BT_SOURCE_SHA`
  and the image label together, even with no migration** (the
  `998501b → 6546a98c` case the re-review named); when that happens, this
  pin — and this document — must be updated by hand after reviewing the
  diff, per the plan's re-pin rule, before R1 can pass again.
- **Schema:** 29 rows; row 29 is `000029_baby_icon.sql`, sha256
  `8a296b40a8a1aba5e8d4f0256a8bb2aa050c50c8ea30a6d9ae2d6cfd9e087288`
  (rows 1-28 unchanged from the coordinator's original read; see
  "Schema 28 → 29" below). `babies.icon` is present. Source: rollout record
  plus `Server/internal/legacyimport/legacytest/testdata/legacy-chain/manifest.json`
  (re-vendored by Task 3's revision 3 amendment). The staged tree's
  compatibility is `{"min":29,"max":29}`.
- **Compose files:** the deployed tree is a `git archive Server` unpack
  under `$BT_DEPLOY_ROOT/source` (not a rollback tree — a rollback copy is
  named `source.<old>-rollback-<new>`, which is a different, unrelated
  directory), and compose runs with `-f` pointed at that tree's
  `Server/compose.yaml`, `Server/deploy/unraid/compose.yaml` and
  `Server/deploy/unraid/live-activity-push.yaml` (main's
  `docs/runbooks/family-live-activity-push.md:229-245`, `compose_push`;
  corrected by fix round 4, R3-C4 — an earlier draft of this document
  omitted the `Server/` level between `source/` and `deploy/unraid/`,
  which does not exist on the real deployed tree and would have reported
  a mismatch, exit 3, against a perfectly healthy homelab). So the label's
  real, absolute paths all read
  `<deploy root>/source/Server/compose.yaml`,
  `<deploy root>/source/Server/deploy/unraid/compose.yaml` and
  `<deploy root>/source/Server/deploy/unraid/live-activity-push.yaml`,
  unchanged by the rollout. Source: the runbook above, and plan global
  constraints. The preflight script reads these back from the app
  container's own `com.docker.compose.project.config_files` label and
  compares each one against its exact path relative to the deploy root
  (fix round 3, R2-N6: not just a basename — a same-named file in the
  wrong directory is a mismatch). **Until R1 has read the live label at
  least once, treat a `compose_match=mismatch` alone as something to
  check by eye against the printed `compose=` value, rather than an
  automatic STOP; every other gate stays strict regardless.**
- **APNs presence:** `deployment.env` holds `BT_APNS_BUNDLE_ID`,
  `BT_APNS_KEY_ID` and `BT_APNS_TEAM_ID` (values not read, presence only).
  `main`'s overlay mounts `${BT_DEPLOY_ROOT}/secrets/apns_key.p8` as the
  `apns_key` secret, and Live Activity pushes are live. Source:
  coordinator's original preflight.
- **Projection equal:** the security projection head must equal the ledger
  head (generation, revision and event hash). Source: the preflight script
  reads both live; there is no separate baseline value to record.
- **Instance ID and recovery epoch,** agreeing across `deployment.env`,
  `server_state` and the ledger's own identity file (fix round 2, I4): a
  disagreement means a recovery, a swap, or corruption happened since the
  baseline, not ordinary drift.
- **The household must not shrink below 2 accounts, 1 family, 2 living
  babies** (the household ruling, fix round 3; this homelab is a known,
  fixed single-family household — D2: "Only my homelab server"). These
  three thresholds are pinned in the script itself. **An increase is
  informational, not a STOP** — see below.
- **Three import-refusal preview counts, each must be 0** (follow-up: the
  import tasks need these ahead of the cutover, since without them nobody
  learns before it whether the importer's refusal paths will fire on the
  real homelab; as of Task 15, these three now match
  `authority_load.go`'s `refuseUncarriable` exactly, not just an
  approximation of it):
  - `identities_material_missing`: `account_identities` rows with
    `material_missing` true (the importer's `ErrIdentityMaterial`);
  - `identities_key_mismatch`: `account_identities` rows (joined to
    `account_security_state` on `account_id`) whose paired
    `identity_key` is not — Postgres's NULL-safe `IS DISTINCT FROM` —
    the `'hmac-sha256:'||identity_key_id||':'||hex(identity_digest)` the
    live row would recompute. This is Task 15's own refusal predicate
    (a `ProjectionMismatch`, not `ErrIdentityMaterial`); a `NULL`
    `identity_key` is only one way it can mismatch, which is what an
    earlier draft of this count checked for alone;
  - `devices_foreign_instance`: `devices` rows whose `server_instance_id`
    disagrees with the server's own instance (the importer's
    `ErrForeignDevice`).

  Any other value means the importer would refuse the run outright; R1
  should learn this now, read-only, rather than discover it only when the
  importer itself refuses during the cutover.

### Informational (recorded, printed, never gates)

- **Three more import-refusal preview figures** (follow-up): these
  describe what the import will carry and transform, not a reason to
  refuse.
  - `prekap_lineages`: live lineages (the active sessions' lineage roots)
    whose absolute expiry is more than the lineage root's `created_at`
    plus 365 days — lineages that began before the one-year refresh cap
    (`legacytest.Spec.PreCapLineages`, issued the way the server did
    before `6aa153dc`).
  - `recovery_active_rotated_count` and `recovery_receipt_present_count`:
    active rotated sessions (`status='active'` with a
    `parent_session_id`), and how many of those still have a
    `refresh_recovery_receipts` row.
  - `recovery_ledger_refresh_attempt_count`: the ledger's own count of
    rotations that carry a `refresh_attempt_id`, read with a read-only,
    recursive `grep -c` over the ledger directory and summed locally —
    never a plain `grep` that would print a matching line's content.

- **A household increase** (the household ruling, fix round 3): a
  caregiver joining adds an account; a new baby adds a living baby; a
  second family is possible too. None of these affect the import, which
  carries every account, family and baby and verifies the live data
  itself, so none are a STOP — only a *decrease* against the pinned
  thresholds is (see "Gating invariants" above). The script prints
  `household=equal|grew|shrank`.
- **`babies_with_icon`:** 1, as of the rollout (the baby with Boys/Girls
  set got an icon; the other has none and shows the neutral face — no icon
  *value* is ever recorded in this document or printed by the script, only
  the count). Source: rollout record, "After the deploy." **This is
  expected to become 2**, not a drift to flag: the rollout record's "Still
  open" section names "Boys/Girls for the second baby" as outstanding, and
  normal use of the app keeps syncing — the rollout's own "After launch"
  section logged 121 lines in 3 minutes from the two phones.
- **Database/ledger sizes and row counts:** as of the morning preflight
  (*before* the rollout, and already stale by the time of this writing —
  fix round 2, I4 corrects an earlier claim that these were "unaffected"
  by a schema-only rollout; the rollout itself wrote to the database, and
  ordinary use keeps adding `mutation_journal` rows, sessions and Live
  Activity rows regardless of schema): 2 accounts' worth of activity,
  1,421 `mutation_journal` rows, a 26 MB database, an 81 MB `/database`
  volume, `security-ledger`/`ledger-head` present with many earlier pre-
  and post-cutover dumps and ledger tars. R1 reads and prints the live
  values; none of them are expected to equal these exactly, and none gate.
- **Ledger-format drift `5b80a71f..998501b`: none,** as of the morning
  preflight — `security/{event.go,commands.go,localstore/}` unchanged, the
  only handler change (`MembershipView.Generation`) in-memory and never
  persisted. Not re-verified by the rollout. R1 still reads the live
  ledger head itself, so any drift since would surface as the gating
  `projection≠equal`, independent of this historical note.
- No secret value is recorded here: backup file hashes and the image
  digest are not secrets, and no row content from `babies` or any other
  care table is recorded — only counts.

### Schema 28 → 29 (for reference: `legacy-chain` rows 1-27 are untouched since the coordinator's original read)

- Rows 1-27 are byte-identical to this branch's 000001-000027.
- Row 28 is `000028_live_activity_push.sql`, sha256
  `43b37464fd4570ae65eb2a84e3579e7e76dbc9e6c4b4ca3d598a2a74f3bc521e`.
- Row 29 (added by the baby-icons rollout) is `000029_baby_icon.sql`,
  sha256 `8a296b40a8a1aba5e8d4f0256a8bb2aa050c50c8ea30a6d9ae2d6cfd9e087288`.

This matches the plan's D2 ruling: "Only my homelab server." The starting
state `legacyimport.Detect` accepts is exactly one pinned
`schema_migrations` history, row for row (version, name, sha256), vendored
and digest-pinned in `legacy-chain/manifest.json` — now 29 rows, matching
`fce422d5`.

## The pinned re-check tool

`Server/scripts/homelab-preflight.sh` turns the baseline above into a
script that runbook R1 runs again, unchanged, right before the cutover, to
confirm nothing drifted since the baseline was taken. It:

- refuses to run unless `BT_PREFLIGHT_APPROVED=yes`, and refuses to run
  under `bash -x` (fix round 1, M3: that would trace the ledger hash and
  digests to stderr);
- reads the homelab over `ssh -o BatchMode=yes -o ConnectTimeout=10 root@192.0.2.10`,
  through a single `ssh_remote` function — nothing else in the script names
  `ssh`, and a static test pins that. Every remote command is one of 22
  exact forms (follow-up: grew from 20 with one more `psql` aggregate for
  the import-refusal preview counts, and one `grep -rc` over the ledger
  directory):
  `docker exec -e PGOPTIONS=-cdefault_transaction_read_only=on ... psql -XAtq ... -c "SELECT ..."`,
  a `grep`/`grep -c`/`grep -rc`, `jq -c`, `test -f`, `stat -c` (mode and
  owner only), `du -sb`, `find ... -type f`, `df -Pk`, `docker inspect -f`,
  or `command -v jq`;
- **the read-only mechanism, and why the `PGOPTIONS` value has no space
  (fix round 1 + fix round 2's re-review, C2/I1/C1):** every `psql` call
  runs `docker exec -e PGOPTIONS=-cdefault_transaction_read_only=on <container> psql -XAtq -v ON_ERROR_STOP=1 ... -c "SELECT ...;"`
  — one statement only, read-only from connection time, with `-q` so no
  command tag is ever printed. The brief's original form,
  `SET default_transaction_read_only = on; SELECT ...` in one `-c` string,
  does not make the `SELECT` read-only and prints a command tag; fix round
  1's fix for that, `PGOPTIONS='-c default_transaction_read_only=on'`
  (quoted, with a space), is itself split by the *remote* shell (sshd runs
  the command string through `sh -c`), handing `docker exec` a bogus
  container name. The no-space form has nothing for any shell to split,
  quoted or not. Verified against a throwaway local `postgres:17.6-alpine`
  container by constructing the *exact* string the script sends (not a
  hand-retyped equivalent — that is exactly how the first proof missed the
  space) and piping it through `sh -c`: the buggy form reproduces
  "No such container"; the fixed form refuses an `INSERT` with
  **SQLSTATE `25006`**. `TestHomelabPreflightGoldenCommandsParseUnderAPOSIXShell`
  is the test that actually catches this class of bug, running every
  golden command through `sh -c` with argv-recording fakes rather than
  relying on a byte-for-byte comparison that would just as easily pin a
  bug as catch one;
- compares the live `schema_migrations` rows against the pinned manifest
  — both the row count and the rows themselves come entirely from the
  manifest file, nothing is hard-coded to a particular schema version —
  (`digests=match`/`mismatch`);
- compares the live `security_projection_head` against the ledger's
  `ledger-head/head.json` (`projection=equal`/`behind`/`ahead`/`diverged` —
  fix round 1, M1: a projection with the same generation:revision but a
  different hash is named `diverged` rather than folded into `ahead`;
  exit 4 on anything but `equal`);
- reports the babies live/icon counts (`babies_live=<n> babies_with_icon=<n>`,
  counts only, never an icon value); cross-checks the instance ID and
  recovery epoch across all three sources (`identity_match=<match|mismatch>`);
  and checks the household against the pinned thresholds
  (`household=<equal|grew|shrank>` — only `shrank` gates, fix round 3);
- previews three of the importer's refusal paths, matching Task 15's
  `refuseUncarriable` exactly (follow-up): `identities_material_missing`,
  `identities_key_mismatch` and `devices_foreign_instance` must each be
  0, or the run exits 3 — the importer would refuse outright.
  `prekap_lineages`,
  `recovery_active_rotated_count`, `recovery_receipt_present_count` and
  `recovery_ledger_refresh_attempt_count` (the last read with a
  recursive, read-only `grep -c` over the ledger directory, summed
  locally from its per-file counts, never printing a matching line) are
  informational only, describing what the import will carry;
- **is the app container itself readable, and only then, is its compose-
  files label and image-revision label trustworthy (fix round 3, R2-C3,
  correcting a fail-open in fix round 2's N1 fix):**
  - the one inspect that only needs the container to *exist*
    (`{{.State.Running}}`) is read first. **Only docker's own exit 1 on
    this inspect ("no such container") means the container is genuinely
    absent** (fix round 4, R3-N8, correcting a misdiagnosis in fix round
    3: any *other* nonzero exit — in particular ssh's own exit 255 on a
    connectivity blip — is a hard failure, exit 2, exactly like every
    other read in this script, not an informational "stopped"). When the
    container is genuinely absent: `app_status=stopped` (exit 5), and the
    compose-files and image-revision reads are allowed to come back
    `unknown` (never read, or read but empty) without failing the whole
    run — fix round 2's N1 fix, kept;
  - but **if that one inspect succeeds — the container exists, whether
    running or stopped — the compose-files and image-revision reads must
    each succeed and be non-empty, exactly like every other read, or the
    whole run exits 2.** Fix round 2's bug was treating every failure of
    these two reads as "unknown," even while the container was reachable;
    an unknown gating invariant then passed with exit 0 whenever the app
    happened to be running — a single ssh blip on exactly these two reads
    would have produced a green R1 with two invariants unchecked;
  - `compose_match` and `image_revision_match` are each `match`,
    `mismatch`, or `unknown` (only reachable via the absent-container path
    above) — never silently passing;
  - the image's own build-time revision label is pinned against
    `EXPECTED_SOURCE_SHA` (R2-I5 above), not merely cross-checked against
    `deployment.env`'s `BT_SOURCE_SHA`, which could itself have drifted to
    the same new, unreviewed value;
- reports whether all three APNs settings are present, each counted by
  its own `grep -c` so a duplicated line still reads as present (fix round
  1, M6; exit 6 if any is absent) — never their values;
- fails closed on every other remote read: any unexpected failure (an
  unreachable host, a query error, a missing `jq` on the homelab) exits 2
  with a message naming what could not be read, never a misreported
  verdict and never an unbound-variable crash (fix round 1, I3). stderr is
  discarded on every remote call, success or failure (fix round 2, N2): a
  successful command's warning (for example a psql collation-version
  notice) must never be mixed into the data this parses, and a failing
  command's stderr is not needed for the fail-closed message, which
  already names what was being read. Every numeric, UUID- and hash-shaped
  value read from the homelab is validated against that shape before it
  is compared or used in bash arithmetic (fix round 1, M2);
- prints one `key=value` line per fact, and ends with a composite summary
  line carrying every gating `*_match` flag, so an exit 3 names which
  invariant failed without needing to re-derive it from the raw values
  (fix round 3, R2-C3):
  `preflight: schema=<max> digests=<match|mismatch> head=<g>:<r> projection=<equal|behind|ahead|diverged> mode=<…> source=<sha> source_match=<match|mismatch> image_revision=<sha> image_revision_match=<match|mismatch|unknown> apns=<set|missing> app=<running|stopped> compose=<paths> compose_match=<match|mismatch|unknown> identity_match=<match|mismatch> household=<equal|grew|shrank>`.
- Exit codes: `0` match, `1` refused to start (approval/manifest/xtrace,
  before any remote call), `2` could not read the homelab's state — now
  including an unreadable or empty compose-files/image-revision label
  *while the app container exists* (fix round 3, R2-C3), and an
  unrelated failure reading the `State.Running` inspect itself, such as
  ssh's own exit 255 (fix round 4, R3-N8) — (fail closed, never shares a
  code with 1 or with 3/4/5/6), `3` a gating-invariant mismatch (schema,
  the pinned revision — either `BT_SOURCE_SHA` or the image label —
  compose files, instance ID/epoch, or a household *decrease*), `4`
  projection head differs from the ledger head, `5` app not running, or
  docker's own inspect reports no such container (exit 1 specifically —
  any other failure reading that inspect is exit 2, not 5, fix round 4,
  R3-N8), `6` APNs settings missing. Priority when more than one verdict
  applies: 3 > 4 > 5 > 6.

`Server/scripts/homelab_preflight_test.go` proves this without ever
touching the homelab, under the real system `/bin/bash` (always 3.2 on
macOS; fix round 1, M4). It runs the script with a stub `ssh` on a `PATH`
containing *only* that stub directory — plus symlinks to the handful of
real local tools the script and the stub need (`bash`, `jq`, `grep`,
`tail`, `cut`, `awk`, `tr`, `dirname`, `cat`) — so the real system `ssh`
has no entry anywhere on `PATH` and can never be reached regardless of
lookup order (fix round 1, I2). A dedicated test additionally asserts that
`command -v ssh`, run under this same restricted `PATH`, resolves to the
stub path itself. The stub records the full argument list of every
invocation and replays canned output by exact, literal string comparison
against the same commands a golden list in the test asserts — not a regex
or glob, so a command that merely resembles an allowed one (for example
one that smuggles a `COPY ... TO PROGRAM` alongside an allowed `SELECT`)
has no matching golden entry and is rejected. The stub distinguishes
"unset, use the healthy default" from "set to the empty string" for the
compose-files and image-revision replies (`${VAR-default}`, not
`${VAR:-default}`, fix round 3, R2-C3), so a test can inject the
empty-label case a real `docker inspect` can produce. The canned replies
are realistic (fix round 2, N5): `BT_MODE=self_hosted` and `PostgreSQL
17.6`. A separate static test asserts the script names `ssh` exactly
once, as a bare command inside `ssh_remote`, and never by an absolute path
anywhere, including comments. And, as described above,
`TestHomelabPreflightGoldenCommandsParseUnderAPOSIXShell` runs every
golden command through an actual POSIX shell with argv-recording fakes.
It cannot check the arguments `stat` receives in the one `test -f ... &&
stat -c ...` command (`test` is a shell builtin with precedence over
`PATH`, and the path does not exist on the test machine, so the `&&`
short-circuits before `stat` ever runs — R2-N7, noted in the test rather
than worked around, since faking that specific absolute path's existence
was judged out of proportion to what it would add); the command's exact
text is still pinned by the exact-sequence test below.

The tests cover: the approval gate and the xtrace refusal, each before any
remote call; `command -v ssh` resolving to the stub; every golden command's
real argv shape under a POSIX shell; the exact 22-call sequence and
arguments for a matching state; a schema mismatch (998501b's 28 rows, a
changed row-29 digest, and an extra 30th row) exiting 3; a manifest one row
longer than the vendored one (built from a temp file) matching and
mismatching, proving the row count is not hard-coded; APNs presence by
count, including a duplicated key line; the babies live/icon counts
(informational: a higher `babies_with_icon` does not change the exit
code); each import-refusal preview count (`identities_material_missing`,
`identities_key_mismatch`, `devices_foreign_instance`) exiting 3 on its own,
isolating the cause with `digests=match` still holding; the other four
import-refusal preview figures printed correctly and never affecting the
exit code, including the ledger's refresh-attempt count summed correctly
across multiple files and correctly zero when no file matches at all; a
failed import-counts query or ledger grep exiting 2, not 0; a household
decrease (no accounts, no families, one or zero living
babies) exiting 3, and a household increase (a second account, family, or
baby) exiting 0 with `household=grew`; an identity mismatch exiting 3; an
image-revision mismatch against the pinned SHA, and a `BT_SOURCE_SHA`
mismatch against the same pin even when the image label agrees with it
(the `998501b → 6546a98c` case), each exiting 3 with the schema itself
still matching; a compose-files mismatch, specifically a same-named file
in the wrong directory (R2-N6) and a rollback-tree-shaped label (R3-N9),
each exiting 3; the real deployed-tree label (`source/Server/...`,
R3-C4) exiting 0 with `compose_match=match`; a behind, an ahead, and a
diverged projection head, each exiting 4; an absent app container failing
all three docker inspect calls and still exiting 5 with
`compose_match=unknown`/`image_revision_match=unknown` and the summary
line printed; a stopped container, and docker's own "no such container"
(exit 1) on the running inspect, each exiting 5; an unrelated failure
reading that same inspect (ssh's own exit 255) exiting 2, not 5 (R3-N8);
a failed or empty compose-files/image-revision read *while the app is
running* exiting 2, not 0 (R2-C3); a stderr warning on an otherwise-
successful run never polluting the parsed output; a failed projection
query, size query, counts query, babies query, deployment.env read, and
an unreachable host, each exiting 2 and never misreporting a verdict, an
`unknown` value, or an unbound-variable crash; a malformed numeric field
failing closed before arithmetic; and a missing `jq` on the homelab
exiting 2 before the schema read is even attempted.

## Verification run (follow-up: import refusal counts)

- `(cd Server && go build ./... && go vet ./... && go vet -tags identitytest ./...)`: clean.
- `gofmt -l Server/scripts`: clean.
- `go test ./scripts -count=1` (full package, no database): 181/181
  passed, no regression (up from 166/166 in fix round 4).
- `go test ./scripts -run TestHomelabPreflight -count=1`: 94/94 passed (28
  top-level tests, counting subtests — up from 83/25).
- `go test ./scripts -run TestHomelabPreflightGoldenCommandsParseUnderAPOSIXShell -count=15`:
  345/345 passed — the two new remote commands (the import-counts
  aggregate and the ledger grep) don't reintroduce the fix-round-3
  `find | wc -l` flaky race.
- One pre-existing finding, fixed while adding these counts: the static
  forbidden-substring scan (a secondary, redundant check alongside the
  byte-for-byte golden-list comparison) included a bare `">"`, which would
  have flagged the new, genuinely-needed SQL comparison
  (`s.absolute_expires_at > root.created_at + interval '365 days'`) as if
  it were shell redirection. Removed just that one entry, with a comment
  explaining why it is safe to drop (the exact-sequence test is the real
  control; nothing can reach the remote host except the literal strings
  written in the golden list, however this scan is edited).
- Mutation-checked the new gating logic (each applied, confirmed red,
  reverted, confirmed green again):
  1. Dropped `identities_material_missing != "0"` from the exit-3
     condition → its subtest in
     `TestHomelabPreflightImportRefusalCountsGateExitThree` failed ("should
     exit 3, got `<nil>`").
  2. Dropped `identities_key_mismatch != "0"` the same way → that subtest
     failed identically.
  3. Changed the ledger per-file summing from accumulating
     (`total += per_file_count`) to overwriting
     (`total = per_file_count`) → `TestHomelabPreflightImportRefusalInformationalCountsArePrinted`
     failed: a two-file total of 5 became 3 (the last file's count only).

## Alignment: the identity-key count matches Task 15's real refusal

Task 15 (`authority_load.go`'s `refuseUncarriable`, landed at `675897c5`)
confirmed the first and third of the three gating counts already matched
its real predicates exactly (`account_identities WHERE material_missing`;
`devices WHERE server_instance_id <> <server instance>`). The second
count, originally `identities_null_key` (`account_security_state WHERE
identity_key IS NULL`), only caught the `NULL` subset of Task 15's actual
refusal: any `account_identities` row (joined to `account_security_state`
on `account_id`) whose `identity_key` is not — Postgres's NULL-safe `IS
DISTINCT FROM` — the `'hmac-sha256:'||identity_key_id||':'||hex(identity_digest)`
the live row would recompute. Renamed to `identities_key_mismatch` and
switched to that exact predicate and join (read from
`authority_load.go:81-90`), gating the same way. `!=` stayed in place for
the unrelated `devices_foreign_instance` comparison; `IS DISTINCT FROM`
needed no `!=`/`<>` workaround at all, since it is spelled out in words
and so was never at risk of tripping the forbidden-substring scan's bare
`">"` the fix round 4 follow-up had to work around.

- `go test ./scripts -count=1` (full package): 181/181 passed, no
  regression.
- `go test ./scripts -run TestHomelabPreflight -count=1`: 94/94 passed
  (unchanged count — this was a rename and a predicate swap, not new
  coverage).
- `(cd Server && go build ./... && go vet ./... && go vet -tags identitytest ./...)`: clean.
- `gofmt -l Server/scripts`: clean.
- Mutation-checked (coordinator's instruction: confirm a nonzero
  `identities_key_mismatch` — standing in for a non-`NULL` but mismatched
  key, which the stub cannot distinguish from any other nonzero value
  since it never runs real SQL — still gates): dropped
  `"$identities_key_mismatch" != "0"` from the exit-3 condition →
  the renamed subtest in
  `TestHomelabPreflightImportRefusalCountsGateExitThree` ("an identity
  whose identity_key does not match...") failed ("should exit 3, got
  `<nil>`"); reverted, confirmed green again.

No homelab host was ever contacted; the exact predicate and join came
from reading `Server/internal/legacyimport/authority_load.go` at
`675897c5`, already on this branch.

No homelab host was ever contacted while building or testing this: every
test run's `ssh` resolved to the stub on a `PATH` with no other entry for
`ssh` anywhere, and the script itself was never invoked without that stub
in front of it. The exact table and column names for the new counts
(`account_identities.material_missing`, `account_security_state.identity_key`,
`devices.server_instance_id`, `sessions.absolute_expires_at`/
`parent_session_id`, `refresh_recovery_receipts.parent_session_id`) came
from reading the vendored legacy chain under
`Server/internal/legacyimport/legacytest/testdata/legacy-chain/` and the
plan's own Task 13 text, not from any live access.
