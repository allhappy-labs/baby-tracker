# Swiss Growth Reference Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Status:** Approved by the user in conversation on 2026-09-30, including pre-implementation clarifications for birth-row validation, provenance packaging, and full published-centile parity.

**Execution outcome:** Implementation and all four task reviews, whole-branch review and scoped correction rereview are complete through `f2d3047`, with runtime/device/production limits open. [Current verification](../../verification/2026-09-30-growth-measurements.md) and [tracker](../../../TASKS.md) record the final outcome; unchecked runtime acceptance steps below are not claimed passed.

**Goal:** Implement and verify attributed, on-device Swiss PEZZ 2025 weight and length/height percentiles while keeping head circumference as raw tracking only.

**Architecture:** Generate a small, reviewed CSV of the 124 birth–five LMS rows from the user-supplied official supplement; bundle the CSV as a GrowthFeature resource. The calculator maps the recorded-zone calendar date to bracketing source ages, linearly interpolates L/M/S, and computes a descriptive percentile without storing or syncing it. Existing check-ins, charts, sync, and export remain the raw-data path.

**Tech Stack:** Swift 6, SwiftUI/Charts, Swift Package resources, Python 3 standard library and macOS PDFKit for the offline generator, Swift Testing, existing iOS simulator/server checks.

**Spec:** `docs/superpowers/specs/2026-09-30-swiss-growth-reference-amendment.md`, superseding the WHO clauses of `docs/superpowers/specs/2026-09-30-growth-measurements-design.md`; source evidence in `docs/research/2026-09-30-swiss-growth-reference-data-audit.md`.

## Global Constraints

- Use only supplementary S1/S2/S5/S6 of Eiholzer et al., *Updated Swiss Growth References 2025*, DOI `10.3390/jcm14165912`, under CC BY 4.0; no paid, WHO, head, or separate Swiss-society chart data.
- Cover birth through the fifth **calendar** birthday inclusive; use the record's stored time zone, the approved corrected-age rule until (excluding) the third chronological birthday, and no extrapolation beyond source nodes.
- Apply +1 cm only to the comparison value for standing length before the second **reference-age calendar** birthday; show no percentile for lying length from that birthday onward. The form default uses the second **chronological** birthday.
- Show a raw chart only after two observations of its type; show a reference band only for a contiguous run of at least two eligible observations. Head readings retain chart/history/sync/export but never get a percentile.
- Keep published precision in generated rows: M/P values as printed to three decimals, L/S as printed to four; identify every derived interpolation as an estimate. Show `<3rd` and `>97th` for values beyond the published centile range rather than a precise extrapolated tail rank.
- Source ZIP/PDF stays in ignored `.superpowers/sdd/2026-09-30-growth-measurements/source/`; commit only the small derivative CSV, generator, manifest, and attribution. Do not change the app-code license or imply publisher endorsement.

## Review Focus

- A birth on 29 February or the 31st must use calendar month anniversaries, including the exact fifth birthday, without a 365-day or 30-day shortcut (Task 2 test).
- A reading just before a due date or across stored/device time zones must not acquire a premature percentile (Task 2 test).
- Missing, duplicated, re-ordered, or nonfinite source rows must suppress the comparison rather than borrow a neighboring row (Tasks 1–2 tests).
- A height measurement near 1.2 years or weight near 4.25 years must remain finite through the published model joins; the UI must say the daily result is estimated (Tasks 2–3 tests).
- Head-only check-ins and a run interrupted by an ineligible length measurement must preserve raw charts without a head band or a bridged length band (Task 3 tests).

---

### Task 1: Verified Swiss table resource

**Files:**
- Create: `scripts/extract-swiss-pdf-text.swift` (PDFKit text extraction only)
- Create: `scripts/generate-swiss-growth-reference.py` (ZIP/PDF hash, S1/S2/S5/S6 parsing, validation, deterministic CSV)
- Create: `Packages/BabyTracker/Sources/GrowthFeature/SwissPEZZ2025.csv`
- Create: `Packages/BabyTracker/Sources/GrowthFeature/SwissPEZZ2025-ATTRIBUTION.md`
- Create: `Packages/BabyTracker/Sources/GrowthFeature/SwissPEZZ2025-manifest.json` (deterministic provenance: schema version, source/member names and hashes, CSV hash, table mapping, extraction/age/interpolation change notice; no machine paths or timestamps)
- Create: `scripts/tests/test_generate_swiss_growth_reference.py`
- Modify: `Packages/BabyTracker/Package.swift`

**Interfaces:** Generator CLI `python3 scripts/generate-swiss-growth-reference.py --source ZIP --output CSV [--check]`; `--check` compares generated bytes with the committed CSV. CSV columns `metric,sex,month,printedYears,p3,p10,p25,m,p75,p90,p97,l,s`; metric values `weight,lengthHeight`, sex values `girls,boys`, month nodes `0...24,30,36,42,48,54,60`. Task 2 consumes this bundled CSV: month is an integer from the exact allowed grid, printedYears is finite and nonnegative (both are zero at birth), centiles/M and S are finite and strictly positive, and L is finite with either sign or zero. Exact printed ages must match their prescribed month mapping. Metric and sex are validated enums, not numeric fields. `--check` also validates the deterministic manifest and attribution hashes.

- [ ] **Step 1: Write parser tests** for exact mapping of one S1 and one S6 row, 31 ordered nodes per table, duplicate/missing/nonfinite rejection, source hash mismatch, and byte-identical regeneration. Use small text fixtures for error cases.
- [ ] **Step 2: Run** `rtk proxy python3 -m unittest scripts.tests.test_generate_swiss_growth_reference -v`; expect parser tests to fail because the generator is absent.
- [ ] **Step 3: Implement** the two scripts. Require ZIP SHA-256 `0def88b3a5e0096b54064b7d9459e683081364943cab341e179f947a46d61336`; select member `jcm-3796415 Supplemental material to Manuscript Cohort 2019+2025 Supp. Table S1-S8 Figure S1-S3. (Edited).pdf` and verify its SHA-256 `fb05a53a8ffbbac45201ad8407ff8c0810abd1fa15e38e4d964681f6043f141a`. Parse only rows at ages 0–5; assert 124 rows, all table headers/units, the printed age sequence, P ordering, M=P50, finite L/S, and LMS reconstruction tolerance 0.01 cm / 0.003 kg at all seven published centiles (P3/P10/P25/P50/P75/P90/P97). Do not round further or synthesize daily rows. Include the article, author/publisher, CC BY 4.0 URL, source/derived hashes, and extraction/month-anniversary/interpolation change notice in attribution; generate the deterministic manifest. Declare the CSV, attribution, and manifest explicitly as GrowthFeature bundle resources. ZIP/PDF inputs must never enter those declarations or a built app.
- [ ] **Step 4: Run** the parser tests and `rtk proxy python3 scripts/generate-swiss-growth-reference.py --source .superpowers/sdd/2026-09-30-growth-measurements/source/jcm-14-05912-s001.zip --output Packages/BabyTracker/Sources/GrowthFeature/SwissPEZZ2025.csv --check`; expect pass. Cross-check S1/S2 P3/M/P97 at the seven ages in the audit against main article Table 3 to 0.1 cm, and the audit's S5/S6 goldens exactly at printed precision.
- [ ] **Step 5: Commit** the scripts, tests, CSV, manifest, attribution, and package declaration as `feat(growth): bundle attributed Swiss reference rows`.

### Task 2: Calendar-age Swiss calculation

**Files:**
- Create: `Packages/BabyTracker/Sources/GrowthFeature/SwissGrowthReference.swift` (resource decoding/validated node lookup)
- Modify: `Packages/BabyTracker/Sources/GrowthFeature/GrowthReferenceCalculator.swift`
- Modify: `Packages/BabyTracker/Tests/GrowthFeatureTests/GrowthReferenceCalculatorTests.swift`
- Create: `Packages/BabyTracker/Tests/GrowthFeatureTests/SwissGrowthReferenceTests.swift`

**Interfaces:** Keep public `GrowthReferenceCalculator.result(...) -> GrowthReferenceResult`; default `init()` loads the bundled reference. Keep the internal injected row closure but change its integer argument from **day** to **source month node**. `SwissGrowthReference.row(metric: GrowthMetric, sex: GrowthReferenceSex, month: Int) -> GrowthLMSRow?` never returns a head row. Add unavailable reasons `noReferenceForMetric` and `unsupportedLengthPosition`.

- [ ] **Step 1: Write failing Swift tests** for all four source mappings, source-node median and all seven published-centile parity (0.01 cm / 0.003 kg), L≈0 and negative L, missing/corrupt/bracketing rows, fifth birthday and one day later, 29-Feb/31st-month anniversaries, stored-zone and due-date boundaries, second-birthday +1 cm vs older lying, and finite results on each side of the 1.2/4.25-year joins. Assert head is `noReferenceForMetric` even with a synthetic row provider. Replace obsolete WHO day-730/731 fixtures.
- [ ] **Step 2: Run** `rtk proxy swift test --package-path Packages/BabyTracker --filter GrowthReferenceCalculatorTests` and the `SwissGrowthReferenceTests` filter; expect failures on new Swiss assertions.
- [ ] **Step 3: Implement** validated CSV loading and calculator changes. Convert the stored-zone measurement timestamp to `LocalDate`; choose birth/due origin using the existing third-chronological-birthday rule. Bracket each date by `Calendar.date(byAdding: .month, value: node, to: origin)` for source month nodes, with UTC date-only arithmetic; interpolate L/M/S linearly using elapsed days between the two calendar anniversaries. At a node use its exact printed row; on the fifth birthday use month 60; reject any missing/invalid bracket rather than crossing it. Preserve stable LMS arithmetic and raw values; +1 cm applies only to eligible standing height.
- [ ] **Step 4: Run** both focused Swift filters; expect pass. Inspect resource loading in the simulator build so a test-only working-directory fallback cannot mask a missing shipped CSV.
- [ ] **Step 5: Commit** as `feat(growth): calculate Swiss reference percentiles on device`.

### Task 3: Swiss presentation and head-only tracking

**Files:**
- Modify: `Packages/BabyTracker/Sources/GrowthFeature/GrowthViewModel.swift`
- Modify: `Packages/BabyTracker/Sources/GrowthFeature/GrowthView.swift`
- Create: `Packages/BabyTracker/Sources/GrowthFeature/GrowthReferenceInfoView.swift`
- Modify: `Packages/BabyTracker/Sources/GrowthFeature/Localizable.xcstrings`
- Modify: `Packages/BabyTracker/Sources/LoggingFeature/MeasurementSheet.swift`
- Modify: `Packages/BabyTracker/Tests/GrowthFeatureTests/GrowthViewModelTests.swift`
- Modify: `Packages/BabyTracker/Tests/GrowthFeatureTests/GrowthViewTests.swift`
- Modify: `Packages/BabyTracker/Tests/LoggingFeatureTests/MeasurementSheetTests.swift`
- Modify: `UITests/BabyTrackerUITests/GrowthFlowUITests.swift`

**Interfaces:** Keep existing `GrowthPoint` and chart/history/navigation interfaces. `GrowthViewModel` assigns head `.calculated(.unavailable(.noReferenceForMetric))` without requiring a legacy profile. `GrowthMetricPresentation.referenceText(_:)` says “No reference percentile in this release” for head; Swiss results say “Estimated Swiss PEZZ 2025 percentile …”, with `corrected age` where applicable. Source information is reachable from Growth details and displays attribution/license/source and interpolation/corrected-age limitations.

- [ ] **Step 1: Write failing presentation/form tests** for head-only chart/history with no percentile or band, interrupted eligible reference runs, source-info accessibility, all displayed locale strings, percentile tails (`<3rd`, `>97th`), and form defaults immediately before/on the second chronological birthday including leap years. Update focused UI assertions for Swiss labels.
- [ ] **Step 2: Run** focused `GrowthViewModelTests`, `GrowthViewTests`, and `MeasurementSheetTests` with `rtk proxy swift test --package-path Packages/BabyTracker --filter NAME`; expect new assertions to fail.
- [ ] **Step 3: Implement** source-specific labels, source information view, accessible unavailable reasons, and birthday-based form default. Replace every user-visible WHO reference label in Growth with Swiss; keep Insights' approved weight card/Open growth details layout and Home Measure entry. Present whole-number estimated ranks within P3–P97 and threshold labels outside that published range. Maintain raw measurement and corrected-age wording; add translations to every supported Growth catalog locale.
- [ ] **Step 4: Run** the focused Swift filters and focused `GrowthFlowUITests` simulator run from the existing project scheme; expect pass and inspect screenshots/AX labels for the source-info and head states.
- [ ] **Step 5: Commit** as `feat(growth): present Swiss insights and head tracking`.

### Task 4: Verification evidence and branch review

**Files:**
- Modify: `docs/superpowers/specs/2026-09-30-swiss-growth-reference-amendment.md` (approval/artifact status only)
- Modify: `docs/verification/2026-09-30-growth-measurements.md`
- Modify: `TASKS.md`

**Interfaces:** No product API. Evidence must distinguish source verification, package/simulator tests, server tests, UI results, and physical-device/release status.

- [ ] **Step 1: Verify** deterministic data `--check`, source/derivative hashes and deterministic manifest, attribution and CC notice in the app resource/info view; inspect the built simulator app and resource bundles to confirm the CSV/notice/manifest are present and ZIP/PDF inputs are absent; inspect the complete data/license diff and ensure neither ZIP/PDF nor WHO data is tracked.
- [ ] **Step 2: Run** `rtk make test`, applicable server protocol E2E/build, iOS simulator build, and focused Growth UI path. Update `Makefile` expected test count only for actual new tests. Record exact commands, results, and known unrelated failures in verification notes and `TASKS.md`.
- [ ] **Step 3: Run** an independent whole-branch review against the approved spec and this plan, fix actionable defects with targeted regressions, then rerun affected gates.
- [ ] **Step 4: Commit** verification/tracker/status changes as `docs(growth): record Swiss reference verification`. Leave branch integration and release to a separate user decision.
