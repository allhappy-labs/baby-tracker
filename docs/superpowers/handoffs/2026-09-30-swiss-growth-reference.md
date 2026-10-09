# Swiss Growth Reference Implementation Handoff

Later user-authorized local main integration is recorded in [integration verification](../../verification/2026-09-30-growth-main-integration.md). Combined current schema is 24 (timer pause 23, Growth 24); the branch evidence below retains its original commit context. No push, deployment or release-readiness claim.

Date: 2026-09-30

Recipient: the next Baby Tracker Growth verifier or coordinating agent

Status: amendment and revised plan approved; Swiss implementation, all four task reviews, whole-branch review and scoped correction rereview completed; I1/M1/M2 addressed in `f2d3047` with no new Critical/Important findings or breakage. Runtime/device/production acceptance remains open.

## Start here

Work in the isolated repository worktree `.worktrees/growth-measurements` on branch `feat/growth-measurements`. Start with [current branch verification](../../verification/2026-09-30-growth-measurements.md) and `TASKS.md` for gates and remaining acceptance. The [approved Swiss amendment](../specs/2026-09-30-swiss-growth-reference-amendment.md), [implemented Swiss plan](../plans/2026-09-30-swiss-growth-reference.md), and [official supplement audit](../../research/2026-09-30-swiss-growth-reference-data-audit.md) retain the binding decisions and provenance. The amendment supersedes the WHO-specific reference clauses of the [original Growth design](../specs/2026-09-30-growth-measurements-design.md).

Historical handoff context: this file originally accompanied plan `26f7a99` before its review. The user subsequently approved the revised plan and chose subagent-driven execution; Tasks 1–4 have been implemented and reviewed. Do not restart those tasks or ask for already-granted plan approval.

## Current implementation boundary

The branch already implements dated check-ins with any combination of weight, length/height, and head circumference; local storage, family sync, deletion/export semantics, Home's Measure action, and the Insights weight card that opens Growth details. Each raw trend chart requires two readings of its type. The last raw-feature review and verification are in [Growth branch verification](../../verification/2026-09-30-growth-measurements.md).

**Swiss percentiles are implemented.** Commits `e1a34ab`, `8a3e59b`, and `1045b0d` supply 124 deterministic S1/S2/S5/S6 rows, provenance/attribution, a validated bundled loader/calendar calculator, and localized Swiss presentation/source information. Head circumference remains raw-only. The final review correction aligns baby creation/settings selectors and six-locale catalogs with the Swiss reference without changing stored/wire identity or measurements. The test-only export readiness correction is `a7b75c5`; no product scheduling changed.

The independent whole-branch review of `6794ffd..0573afb` found no Critical defects and requested I1 (form labels), M1 (build diagnostics), and M2 (this stale handoff). Those scoped corrections are committed in `f2d3047`; independent rereview of `0573afb..f2d3047` confirms all addressed and no new Critical/Important findings or breakage. This is not a release-readiness or integration claim.

The main checkout is separate and has unrelated dirty `TASKS.md` and `work/` content. Preserve it. The Growth worktree was clean before this handoff documentation. No branch merge, push, deployment, public release, or physical-device installation has occurred for this feature.

## Source artifact and data rights

The user saved the official publisher supplement. Its ZIP is locally available at `.superpowers/sdd/2026-09-30-growth-measurements/source/jcm-14-05912-s001.zip` inside the Growth worktree. This ignored source location is input to the generator, **not** an app resource or a file to commit. The ZIP SHA-256 is `0def88b3a5e0096b54064b7d9459e683081364943cab341e179f947a46d61336`; `unzip -t` passed and the table PDF was inspected with PDFKit. The audit records the PDF member names and hashes, exact S1/S2/S5/S6 layout, 31 birth-through-five nodes per table, and source-node goldens. If that ignored ZIP is missing in a later workspace, recover the user-supplied file or ask for it; do not replace it with a browser route that the Chrome security policy forbade.

The MDPI article and its published supplement use CC BY 4.0 per the publisher's policy; no separate restriction was found in the supplied PDFs. The implemented resource bundles only a deterministic derivative of the four required tables with article/author/publisher credit, source and license links, hashes, and a clear extraction/interpolation change notice. The app's original code can retain its own license. Do not bundle the separate pädiatrie schweiz chart PDF, WHO tables, or a head reference. The rights audit and recorded app-resource inspection support this approved use; they do not establish final distribution-contract/legal release clearance.

## Binding behavior and calculation decisions

- Weight and length/height use sex-specific Swiss PEZZ 2025 values from birth through the fifth calendar birthday. Head circumference stays recordable, synced, and charted without a percentile or reference band.
- Keep raw measurement values unchanged. Use the measurement's stored time zone to derive the date. If a later expected due date exists, use corrected reference age until the third chronological birthday and label that comparison.
- The supplement provides discrete age nodes rather than a daily curve. The plan maps source nodes to calendar-month anniversaries and linearly interpolates L/M/S between adjacent nodes. This is an app-authored **estimate**; test both sides of the paper's height and weight model joins. Do not present interpolated ranks as exact published daily values.
- Before the second reference-age calendar birthday, add 1 cm to a **standing length comparison only**. At or after that birthday, a lying measurement has no percentile. The form defaults to lying/standing based on the second **chronological** birthday but preserves a parent's actual choice.
- Show descriptive ranks within the published P3–P97 interval and `<3rd`/`>97th` outside it. Never calculate a head percentile. Raw chart lines never connect invented observations; reference bands never bridge an ineligible observation.
- Source information and CC BY attribution must be reachable in Growth details and translated with the other Growth copy. The Swiss paper used term newborns; the corrected-age rule is an approved product application, not a claim of Swiss study validation or diagnosis.

## Execution and verification

The four implementation tasks, all four task reviews, independent whole-branch review and scoped correction rereview are complete. Current correction evidence, exact commands and final review findings/rulings are summarized in durable [branch verification](../../verification/2026-09-30-growth-measurements.md) and Git; review scratch is removed after completed review. Final outcome bookkeeping is recorded; runtime/device/production acceptance remains open as detailed below. Preserve unrelated main-checkout work and source artifacts. No merge/push/deploy/install/external contact is authorized by this handoff.

Current gates: final correction focused Swift tests pass (18 affected tests), the full package gate passes (1101 tests), and simulator product/UI-harness compilation plus static catalog/compiled-resource/hash/ZIP exclusion checks pass. These localization/documentation changes leave calculator, source generator/data, raw/sync APIs, and server code untouched; the final full server gate passed on `a7b75c5`, while the earlier intermittent deadline/export causes remain unresolved. Do not infer a source scheduling fix or repeat completed implementation merely from historical failures.

Historical raw-feature evidence: 1,079 Swift tests and focused Growth UI paths passed before the Swiss implementation. The broad UI run had seven untouched feeding mismatches (no baseline runtime reproduction), and a later GrowthFlow class attempt was killed. Current Swiss source-info/head UI stalled before tests; current Swiss routed/AX5 acceptance and every locale's rendered source/head/chart layout remain unverified. Physical-device provisioning/installation/acceptance, production schema-23/shared-family rollout, and app owner download/share are also unverified. Compilation and static localization checks do not establish runtime acceptance. Migration evidence uses a checkout-generated V6 store rather than a database archived from a released binary.

Branch integration and release require separate authorization and evidence. Keep these limits visible after completed review.
