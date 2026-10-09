# Baby icons and shared switcher rollout — 2026-10-09

The user approved the deploy and the merge ("deploy and merge"). The server went first, because a phone on this build that syncs with a schema-28 server has its baby mutations quarantined for good (plan Task 9 Step 5).

## Pre-merge evidence (branch `feat/baby-icons`, head `fce422d5`, which includes `main` at `6546a98c`)

- `make test`: OK, 1673 tests.
- App build on the shared simulator: succeeded.
- `make server-protocol-e2e`: 15 tests in 8 suites passed, against schema 29.
- `make server-test`: all packages passed except the Live Activity composition timing tests in `cmd/baby-tracker-server`. These fail intermittently under load on both the branch and `main`. Run alone on a quiet machine, `TestLiveActivityCompositionAuthorityProfiles` passed 3 of 3 on the branch and 3 of 3 on `main`.
- Simulator end-to-end check: [screenshots](2026-10-08-baby-icons/README.md).
- Reviews:
  - every task passed its own spec and quality review;
  - the final whole-branch review found I1 (deploy order) and I2 (the launch fill-in before the first pull);
  - the fix wave addressed both, and the scoped re-review confirmed it.

## Server (Unraid `baby-tracker-sync`)

**Preflight**
- Revision `998501b`, schema 28, all three containers healthy.
- `998501b..fce422d5` changes only this feature's server files.
- Compose runs with three files: `Server/compose.yaml`, `deploy/unraid/compose.yaml` and `deploy/unraid/live-activity-push.yaml`.

**Source**
- `git archive fce422d5 Server` has SHA-256 `023b30701c99c7f33d54e1c200bdc7168f712a0742b2ebe45f702fe2cec040a3`. It matched after upload.
- The staged tree has compatibility `{"min":29,"max":29}`.

**Backups** (mode 600; 709 `pg_restore --list` entries each)

| File | SHA-256 |
|---|---|
| `backup.pre-fce422d.dump` | `e2b74973ca4560d642b8c5f165498376d50dc9f8bd84d5157910e39b94e04208` |
| `backup.pre-fce422d.ledger.tar` (taken live; the ledger can change while it is read) | `8fe6cfa6673645963bc9c74f8f5443da9052af0ece600c0b07eccdf3df173bd7` |
| `backup.cutover-fce422d.dump` (app stopped) | `e358ac456d2e89c607fb838377db08c7cac874864d7e41e696cd308a4f290c72` |
| `backup.cutover-fce422d.ledger.tar` (app stopped) | `9a53c56bddaaee5194c24fc547a3015a3d50595563428fe4ea530c7049be5c9a` |

**Rollback set:** `source.998501b-rollback-fce422d` and `deployment.env.998501b-rollback-fce422d`.

**Config:** `provision-unraid-secrets.sh` was run with `BT_ALLOW_SOURCE_UPDATE=1`. Only `BT_SOURCE_SHA`/`BT_IMAGE_NAME` changed, and `compose config -q` passed.

**Image**
- `baby-tracker-server:fce422d…` is `sha256:792a55cdb0677db08910dcdc3b438735604cad291a0e350bff4294169e1684f5`, with label revision `fce422d51ef4fbd4ac98a4bab4e4375ff6e762a6`.
- The `998501b` image is retained.

**Cutover**
- Stopped only the app, then took the cutover dump and ledger tar.
- `compose run --rm --no-deps migrate` exited 0, and the migrate log has no errors.
- Schema went 28 → 29 (`000029_baby_icon.sql`), and `babies.icon` is present.
- `compose up -d --wait app`: all three containers are healthy, and the app runs `fce422d…`.

**After the deploy**
- `verify-unraid-deployment.sh verify` passed: private HTTPS health and readiness 200, the live socket gives 401 for a bogus bearer, no host ports, unrelated containers unchanged.
- First 5 minutes: 43 log lines, all INFO; status counts 42×200 and 1×401 (the verifier). 0 restarts.

## Merge

`main` was fast-forwarded from `6546a98c` to `fce422d5`. The main checkout's uncommitted `TASKS.md`, `README.md`, `docs/images/` and `work/` were left untouched.

## Phones

- **Build:** signed Debug build of `fce422d5` from the clean worktree, `xcodebuild -scheme BabyTracker -configuration Debug -destination 'generic/platform=iOS'`. `codesign --verify --deep --strict` passes, and the team is `TEAM123456`.
- **Install:** installed in place on both Phone B and Phone A. Local data is kept, and the store migrates V9 → V10 on first launch. Both apps were launched with `devicectl`.
- **After launch, on the server:**
  - 2 living babies.
  - The baby with Boys set got `boy.1`.
  - The baby with no Boys/Girls has no icon and shows the neutral face, as designed. It gets an icon once Boys/Girls is set in Settings › Babies.
  - 121 log lines in 3 minutes, all INFO.

## Still open

- The user's own check on both phones:
  - the icons and headers;
  - Boys/Girls for the second baby;
  - a rename on one phone keeps the icon on the other.
- Never install a pre-V10 build on these phones again.
