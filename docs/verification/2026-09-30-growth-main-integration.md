# Growth integration into local main — 2026-09-30

User authorized local main integration after Swiss implementation/reviews; no push, deployment or release-readiness claim is authorized. Integration parents are main `12beedc` and Growth `bde0174`, merge-base `6794ffd`. An isolated integration checkout protects unrelated tracked and untracked work in the main checkout.

## Resolutions

- Keep the already committed timer-pause migration `000023_timer_pause.sql` byte-identical; move the unpublished Growth migration to `000024_growth.sql`. Growth inherits schema-23 privileges and adds its grants at 24. Exact compatibility min/max is 24; all executable current-schema tests/script expectations are adjusted. A new Go regression validates both migration identities/digests and rejects schema 23 for the combined binary.
- The first merged Swift gate failed compilation: main's new timer-pause fixture used the old baby-creation API. Add the required reference sex/due-date arguments to that fixture; assertions remain unchanged.
- The next run passed every executed test but failed the literal guard (expected 1103, observed 1108). Main added seven declarations relative to the base, while its Makefile total had increased by only two. Combined expected total is 1108 (Growth run 1101 plus main's seven additions); the corrected `rtk make test` exits 0 with `OK: 1108 tests`.
- Independent integration review inspected automatic overlap and resolutions: timer pause, bottle summaries, menu stability and Growth remain intact; no Critical/Important findings. Follow-up review confirmed the fixture correction and other new main test fixtures/routes.

## Verified gates and local integration

- `rtk make test`: exit 0, 1108 tests on the combined tree.
- Generator source `--check`: sandbox-only Swift extraction failed before verification; authorized rerun passes, 124 rows and all provenance artifacts byte-identical.
- `rtk proxy python3 -m unittest scripts.tests.test_generate_swiss_growth_reference -v`: all 15 pass, no skips, via temporary ignored link to retained source; link removed afterward.
- `rtk proxy xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -derivedDataPath /path/to/artifact -quiet build-for-testing`: exit 0. Product/UI harness compilation only. Compiler warnings and exact error-looking `error: the following command failed with exit code 0 but produced no further output` diagnostics remain; origin unresolved. This is not a warning-free build or UI runtime acceptance.
- `rtk proxy make server-test`: exit 0; all packages pass against disposable PostgreSQL, including records 92.560s, sync 98.026s, migrate and database role/schema gates.
- `make server-protocol-e2e` executed with raw log capture through `rtk proxy python3`: exit 0. Actual image `sha256:cba1f6019e33ca36433aa4f8d0bd7d788f28cbc20b9ff644dc1b08cb5f3bfbdc`, source merge commit `47ac2a6cbdc449ecd4986a838db4d8482bdd557f`, arm64 and schema 24 verified. Restart/receipt and two-caregiver Swift client proofs passed; disposable protocol project reports zero containers/volumes. Complete raw log: `/path/to/artifact`. Local infrastructure only.
- Built app CSV/notice/manifest are byte-identical; app contains no ZIP/PDF, UI harness exists, all 24 migration digests match.
- Local main advanced from `12beedc` to merge commit `47ac2a6` after all gates passed. A follow-up records outcomes and removes two accidentally staged generator bytecode cache files, adding ignore rules; unrelated main edits are restored separately and remain uncommitted. No push or deployment.

The official supplement ZIP/PDF stays ignored in the original Growth worktree and outside Git/app resources. Main's unrelated edits remain excluded from integration commits. The original Growth worktree/branch is intentionally retained to preserve the source artifact. The temporary integration checkout is disposable after successful finalization.

Current Swiss iOS source-info/head/routed/AX5 and all-locale rendered acceptance remain unverified; no new runtime attempt is claimed. Physical-device, production, owner app download/share and independent old-binary migration evidence remain open as detailed in [Swiss verification](2026-09-30-growth-measurements.md). Historical schema-23 Growth evidence applies only to the pre-integration branch; combined current schema is 24. Earlier intermittent server failure causes remain unresolved.
