# Immunizations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A caregiver logs a vaccination (vaccine name, optional dose, time, note) for a baby; it syncs to the family, shows in History, the PDF, an Insights card with an Immunizations screen, and Ask.

**Architecture:** One new synced, baby-owned kind `immunization`. On the server it is care kind 9 in the existing `care_entities` table, added by migration `000033`. On the phone it is a SwiftData `ImmunizationEntry` in `SchemaV11`, a `HistoryItem` case like supplements, and a Home action `vaccine` in the arrangeable order. The card and screen live in InsightsFeature; the panel lives in LoggingFeature.

**Tech Stack:** Go 1.x server on PostgreSQL 17 (`Server/`), Swift 6 / SwiftUI (iOS 26) / SwiftData package (`Packages/BabyTracker`), Swift Testing, XCUITest.

**Spec:** `docs/superpowers/specs/2026-10-11-immunizations-design.md`. Read it first; this plan argues from it.

**Base:** `main` with the `home-buttons` branch merged (it adds `HomeActionKind`, `HomeLayout`, `MedsSheet`). This plan's file and line references were read on `home-buttons` at `de55081e` for the app and `main` at `6cf9f0ca` for `Server/`. Line numbers drift; every edit names the symbol as well, and the symbol wins.

## Global Constraints

- Wire shape, never changed once shipped: entity kind `immunization`, payload version 1, payload keys exactly `baby_id`, `given_at`, `time_zone_id`, `vaccine`, `dose`, `note`. `dose` and `note` are always present and may be `null`. The server refuses unknown keys.
- Server care kind code: `9`. Migration: `Server/migrations/000033_immunizations.sql`. Compatibility window: `min 33, max 33`.
- Phone schema: `SchemaV11` = `SchemaV10.models + [ImmunizationEntry.self]`, one lightweight stage. `SchemaV10` is not edited beyond its doc comment.
- `vaccine` passes `SupplementName.normalized` (≤ 64 scalars, ≤ 256 UTF-8 bytes, no control characters, not blank). `dose` is `nil` or 1…10.
- The phone refuses `given_at` after now and before the baby's birth day (calendar date in the entry's time zone). The server checks neither.
- Home action raw value: `vaccine`, appended last in `HomeActionKind`. Never rename it. Accessibility identifiers follow from it: `home.vaccine`, `more.vaccine`, `homeButtons.row.vaccine`. Panel prefix: `immunization` (`immunization.save`, `immunization.delete`, `immunization.picker`, `immunization.name`, `immunization.add`, `immunization.dose`, `immunization.dose.minus`, `immunization.dose.plus`, `immunization.time`, `immunization.details`, `immunization.note`, `immunization.validation.date`, `immunization.error`). Existing identifiers stay.
- No new capability, HTTP header or marker for the kind. It rides `records-v1`.
- The read-only MCP (`Server/internal/familyread`, `Server/internal/mcpapi`) is not touched.
- Never rewrite: `Server/internal/migrate/testdata/catalog-32-chain.golden`, `Server/internal/sync/testdata/golden/legacy-sync.json`, `Server/internal/familyread/testdata/golden/legacy-familyread.json`, `Server/internal/export/testdata/golden/legacy-export.json`, `Server/internal/care/testdata/legacy-rules/*.json`, and the golden scenario in `Server/internal/testkit/golden.go`.
- Panels are `EntryPanel` + `PanelSection` in a fitted sheet. No `Form` and no `NavigationStack` at a fitted sheet's root; both measure as zero.
- Strings never live in a view. Every new user-facing string goes through the target's copy enum or L10n helper and gets `de`, `fr`, `es`, `it`, `uk` translations in that target's `Localizable.xcstrings` via `python3 scripts/add-catalog-strings.py CATALOG ENTRIES.json`. Before adding a key, `grep` the catalog; the script replaces an existing key, so a key that exists is left out of the entries file.
- The app states facts only. No string says a vaccine is due, missing, late or complete.
- Ask: no new tool, no keyword routing, no validators beyond the existing kind whitelist.
- Tests assert logic, never layout geometry.
- `make test` asserts a literal total (`EXPECTED_TESTS` in `Makefile`). Each task says how many tests it adds; bump the number by that much in the same commit and put `(+N tests)` in the subject. A parameterised test counts once per argument. If the number on the branch differs from the one a task assumes, add the delta to whatever is there. Never run bare `swift test` as evidence; focused `swift test --filter` runs are for iteration only.
- Every `ModelContainer` in tests is built through `BabySchema.makeContainer` / `BabyStore.inMemory`.
- Simulator: only `platform=iOS Simulator,id=<simulator-udid>`. Run `pgrep xcodebuild` first; if it prints anything, wait. One run at a time. Never `make build` / `make test-ui` (they select by name).
- UI tests: only the named tests with `-only-testing:`, never the full suite.
- Work in worktree `.worktrees/immunizations` on branch `immunizations`; use `/usr/bin/git` there (RTK's rewrite trips the worktree guard). The main checkout may carry another session's uncommitted edits; do not touch them.
- Server tests need Docker: `make server-test` with `BT_TEST_PACKAGES` / `BT_TEST_RUN` to narrow. Tests that need no database run with `cd Server && go test ./internal/protocol/ -run … -count=1`.
- Commit subjects: `type(scope): plain statement of what is now true`. End every commit message with `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.
- No production SSH, deploy, phone install or merge to `main` without the user's explicit approval at that step (Task 15).

## Review Focus

1. **A shot dated before the baby's birth, or after now** — the phone refuses both on log and on edit, with the panel's inline message, and writes nothing. Tests in Task 5 (`aFutureOrPreBirthTimeIsRefusedAndWritesNothing`) and Task 8 (`dateRulesDisableSaveWithAMessage`).
2. **A remote shot this phone cannot read** — a page whose immunization lacks `given_at` or `vaccine`, or carries a dose that is not a whole number: the whole page is refused and the cursor does not advance; nothing half-applied. Test in Task 6 (`aMalformedRemoteShotRefusesThePage`).
3. **The same vaccine typed with other casing or spaces, on this phone or the other** — one option in the picker, one spelling, and the next dose counts both. Tests in Task 5 (`namesAreRealmWideMergedAndOrderedByLatestShot`, `nextDoseCountsThisBabyOnly`).
4. **A schema-32 database with real rows upgraded in place** — migration 000033 applies over existing care rows of every kind, the old rows still read, and a kind-9 row is then accepted in every partition. Test in Task 2 (`TestImmunizationMigrationOverABaselineWithRows`).
5. **The entry vanishes while its panel is open** — deleted on the other phone, or the baby deleted: Save and Delete refuse without writing, and the panel closes; the Immunizations screen drops the row. Tests in Task 8 (`aShotDeletedElsewhereClosesThePanel`) and Task 11 (`aDeletedShotLeavesTheScreen`).

---

### Task 0: Preconditions and worktree

**Files:** none changed in the main checkout.

- [ ] **Step 1: Confirm the base**

```bash
cd <repo>
/usr/bin/git log --oneline -1 main
grep -c "enum HomeActionKind" Packages/BabyTracker/Sources/BabyCore/HomeLayout.swift
ls Server/migrations
grep -n "EXPECTED_TESTS :=" Makefile
```

Expected: `HomeLayout.swift` exists and the count is `1`; `Server/migrations` holds exactly `000032_baseline.sql` and `embed.go`. If `HomeActionKind` is missing, `home-buttons` has not merged: **stop and tell the user**. If a `000033_*.sql` exists, another feature took the number: **stop and tell the user**. Note the `EXPECTED_TESTS` value; it is this plan's baseline `B`.

- [ ] **Step 2: Create the worktree and bring the spec and plan into it**

```bash
cd <repo>
/usr/bin/git worktree add .worktrees/immunizations -b immunizations
cp docs/superpowers/specs/2026-10-11-immunizations-design.md .worktrees/immunizations/docs/superpowers/specs/
cp docs/superpowers/plans/2026-10-11-immunizations.md .worktrees/immunizations/docs/superpowers/plans/
cd .worktrees/immunizations
/usr/bin/git add docs/superpowers/specs/2026-10-11-immunizations-design.md docs/superpowers/plans/2026-10-11-immunizations.md
/usr/bin/git commit -m "docs(immunizations): spec and plan

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

If the two files are already tracked on `main`, skip the `cp`, `add` and commit. If they are untracked in the main checkout, they stay there; tell the user at the end that they must be removed before the branch can merge.

- [ ] **Step 3: Baseline green**

Run: `cd .worktrees/immunizations && make test`
Expected: `OK: B tests`.

---

### Task 1: Server protocol — the `immunization` payload

**Files:**
- Create: `Server/api/fixtures/v1/immunization.json`
- Create: `Server/internal/protocol/immunization_test.go`
- Modify: `Server/internal/protocol/entities.go` (`SupplementPayload` neighbourhood ~`:123`, `knownEntityKind` ~`:291`, `decodeEntityPayload` ~`:306`)
- Modify: `Server/internal/protocol/mutations.go` (`decodeMutationPayload` ~`:129`, `validateRequiredPayloadShape` ~`:199`, `normalizeDecodedPayload` ~`:339`)
- Modify: `Server/internal/protocol/canonical.go` (payload switch ~`:142`, helpers ~`:193`)
- Modify: `Server/internal/protocol/sync.go` (two inline kind lists ~`:479-480`)
- Modify: `Server/api/fixtures/v1/manifest.json`
- Modify tests: `Server/internal/protocol/compatibility_test.go:546,568`, `canonical_test.go` (`TestCanonicalMutationHashOfEveryFixture` ~`:426`), `content_hash_test.go` (~`:22`), `payload_version_test.go:13-14,35-37,58`

**Interfaces:**
- Produces: `protocol.ImmunizationPayload{BabyID, GivenAt, TimeZoneID, Vaccine string; Dose *int; Note *string}`; fixture `immunization.json`; two pinned hashes (content, mutation) that Task 4 pins again in Swift.

- [ ] **Step 1: Write the fixture** (one line, no trailing newline)

```bash
cd Server/api/fixtures/v1
grep -l "000000000090\|90000000-0000-4000-8000" *.json   # expected: no output
printf '%s' '{"mutation_id":"01000000-0000-4000-8000-000000000090","entity_id":"90000000-0000-4000-8000-000000000001","entity_kind":"immunization","operation":"create","payload_version":1,"observed_entity_version":0,"client_mutation_time":"2026-09-20T14:10:00Z","import_id":null,"payload":{"baby_id":"10000000-0000-4000-8000-000000000001","given_at":"2026-09-20T09:30:00Z","time_zone_id":"Europe/Zurich","vaccine":"Prevenar 13","dose":2,"note":null}}' > immunization.json
shasum -a 256 immunization.json
```

Add `"immunization.json": "<that sha256>"` to `manifest.json`, keeping the file's existing ordering and formatting.

- [ ] **Step 2: Write the failing shape test** — `Server/internal/protocol/immunization_test.go`

```go
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestImmunizationPayloadShape(t *testing.T) {
	raw := readV1Fixture(t, "immunization.json")
	m, err := DecodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	p := m.Payload.(ImmunizationPayload)
	if p.Vaccine != "Prevenar 13" || p.Dose == nil || *p.Dose != 2 || p.Note != nil {
		t.Fatalf("%+v", p)
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(encoded, &fields)
	if len(fields) != 6 {
		t.Fatal(string(encoded))
	}
	without := func(key string) []byte {
		var envelope map[string]json.RawMessage
		json.Unmarshal(raw, &envelope)
		var payload map[string]json.RawMessage
		json.Unmarshal(envelope["payload"], &payload)
		delete(payload, key)
		envelope["payload"], _ = json.Marshal(payload)
		bad, _ := json.Marshal(envelope)
		return bad
	}
	// Every key is present on the wire; dose and note may be null, the rest may not.
	for _, key := range []string{"baby_id", "given_at", "time_zone_id", "vaccine", "dose", "note"} {
		if _, err := DecodeMutation(without(key)); err == nil {
			t.Fatalf("accepted missing %s", key)
		}
	}
	for _, key := range []string{"baby_id", "given_at", "time_zone_id", "vaccine"} {
		null := strings.Replace(string(raw), `"`+key+`":"`, `"`+key+`":null,"x":"`, 1)
		if _, err := DecodeMutation([]byte(null)); err == nil {
			t.Fatalf("accepted null %s", key)
		}
	}
	noDose := strings.Replace(string(raw), `"dose":2`, `"dose":null`, 1)
	if m, err := DecodeMutation([]byte(noDose)); err != nil || m.Payload.(ImmunizationPayload).Dose != nil {
		t.Fatalf("null dose: %v %v", m, err)
	}
	for _, bad := range []string{`"dose":2.5`, `"dose":"2"`, `"dose":true`} {
		if _, err := DecodeMutation([]byte(strings.Replace(string(raw), `"dose":2`, bad, 1))); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	padded := strings.Replace(string(raw), "Prevenar 13", "  Prevenar 13  ", 1)
	if trimmed, err := DecodeMutation([]byte(padded)); err != nil || trimmed.Payload.(ImmunizationPayload).Vaccine != "Prevenar 13" {
		t.Fatalf("trim %v %v", trimmed, err)
	}
	extra := strings.Replace(string(raw), `"note":null`, `"note":null,"logged_at":"2026-09-20T15:00:00Z"`, 1)
	if _, err := DecodeMutation([]byte(extra)); err == nil {
		t.Fatal("unknown field accepted")
	}
}
```

The delete arm is covered by `payload_version_test.go`'s delete map (Step 5).

- [ ] **Step 3: Run it to see it fail**

Run: `cd Server && go test ./internal/protocol/ -run TestImmunizationPayloadShape -count=1`
Expected: FAIL to compile, `undefined: ImmunizationPayload`.

- [ ] **Step 4: Implement**

`entities.go`, after `func (SupplementPayload) protocolPayload() {}`:

```go
type ImmunizationPayload struct {
	BabyID     string  `json:"baby_id"`
	GivenAt    string  `json:"given_at"`
	TimeZoneID string  `json:"time_zone_id"`
	Vaccine    string  `json:"vaccine"`
	Dose       *int    `json:"dose"`
	Note       *string `json:"note"`
}

func (ImmunizationPayload) protocolPayload() {}
```

- `knownEntityKind`: add `"immunization"` to the case list.
- `decodeEntityPayload`: add `case "immunization": target = ImmunizationPayload{}` beside the supplement case.

`mutations.go`:
- `decodeMutationPayload`, the delete arm: add `|| kind == "immunization"` to the kind list. Add an arm after the supplement one:

```go
	case (operation == "create" || operation == "update") && kind == "immunization":
		target = ImmunizationPayload{}
```

- `validateRequiredPayloadShape`, after the `SupplementPayload` case:

```go
	case ImmunizationPayload:
		required = []string{"baby_id", "given_at", "time_zone_id", "vaccine"}
		for _, key := range []string{"dose", "note"} {
			if _, present := object[key]; !present {
				return errInvalidRequest
			}
		}
```

- `normalizeDecodedPayload`, after the `SupplementPayload` case:

```go
	case ImmunizationPayload:
		value.GivenAt = normalizeTimestampForHash(value.GivenAt)
		value.TimeZoneID = normalizeString(value.TimeZoneID)
		value.Vaccine = strings.TrimSpace(normalizeString(value.Vaccine))
		value.Note = normalizeOptionalString(value.Note)
		return value
```

`canonical.go`, after the `SupplementPayload` case, and a helper beside `nullableString`:

```go
	case ImmunizationPayload:
		return withBaby(map[string]any{"dose": nullableInt(value.Dose), "given_at": normalizeTimestampForHash(value.GivenAt), "note": nullableNormalizedString(value.Note), "time_zone_id": normalizeString(value.TimeZoneID), "vaccine": normalizeString(value.Vaccine)}, value.BabyID), nil
```

```go
func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
```

`sync.go`: add `"immunization"` to both inline kind slices (`knownOuter`, `knownInner`).

- [ ] **Step 5: Extend the pinning tests**

- `compatibility_test.go`: both `21` literals become `22`.
- `payload_version_test.go`: add `"immunization": 1` to the current-version map and the delete-version map, and `"immunization.json"` to the fixture list.
- `content_hash_test.go`: add a vector beside the supplement one with canonical JSON
  `{"entity_kind":"immunization","payload":{"dose":2,"given_at":"2026-09-20T09:30:00.000000Z","note":null,"time_zone_id":"Europe/Zurich","vaccine":"Prevenar 13"},"payload_version":1}`
  and an empty hash for now.
- `canonical_test.go` `TestCanonicalMutationHashOfEveryFixture`: add `{"immunization.json", ""}` in the list's own shape.

Run: `cd Server && go test ./internal/protocol/ -count=1`
Expected: FAIL in exactly those two tests, each printing the hash it computed. If the content-hash test also reports a canonical JSON mismatch, the `canonical.go` map is wrong; fix that, not the vector. Paste the two printed hashes into the two tests. Record both values in the commit body: Task 4 pins the same literals in Swift, and they must come out equal there without being copied into Swift code first.

- [ ] **Step 6: Run to green**

Run: `cd Server && go test ./internal/protocol/ -count=1 && go vet ./internal/protocol/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
/usr/bin/git add Server/api/fixtures/v1 Server/internal/protocol
/usr/bin/git commit -m "feat(protocol): the immunization payload, its fixture and pinned hashes

content sha256: <hash>
mutation sha256: <hash>

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Server care rules and migration 000033

**Files:**
- Create: `Server/migrations/000033_immunizations.sql`
- Create: `Server/internal/migrate/testdata/catalog-33.golden` (recorded, Step 7)
- Create: `Server/internal/database/immunization_migration_test.go`
- Modify: `Server/internal/migrate/compatibility.json` (through the manifest tool)
- Modify: `Server/internal/care/entity.go` (kind constants ~`:17`, `kindNames` ~`:41`, `typedColumns` ~`:285`, comment ~`:13`), `validate.go` (after the supplement arm ~`:100`; delete allowlist ~`:153`), `apply.go` (`ActionFor` ~`:102`), `records.go` (`mutationBabyID` ~`:247`, `mutationBabyIDFromEntity` ~`:293`), `doc.go:2`
- Modify tests: `Server/internal/migrate/compatibility_test.go:55-88`, `apply_history_test.go:16-88`, `Server/internal/database/baseline_test.go:18-23,40-78`, `care_schema_test.go:24,27,147,190-232,476`, `roles_test.go`, `request_path_schema_test.go`, `import_schema_test.go`, `identity_schema_test.go`, `family_schema_test.go`, `authority_schema_test.go`, `refresh_recovery_schema_test.go`, `live_activity_schema_test.go`, `Server/internal/tenant/context_test.go:382`, `Server/internal/care/entity_test.go:143-176`, `validate_test.go` (~`:117`, new test)
- Modify: `Server/scripts/lib/disposable-sync.sh:324`, `Server/README.md:752-755`

**Interfaces:**
- Consumes: `protocol.ImmunizationPayload` (Task 1).
- Produces: `care.KindImmunization int16 = 9`; a database at schema 33 that accepts kind 9; the server validates and applies `immunization` create/update/delete.

- [ ] **Step 1: Write the migration**

`Server/migrations/000033_immunizations.sql`. Copy the `care_entities_kind_shape` expression **verbatim from `000032_baseline.sql:1612`** and change only `ARRAY[2, 3, 4, 5, 6, 8]` to `ARRAY[2, 3, 4, 5, 6, 8, 9]`. Nothing else in that expression changes: kind 9 has `label IS NULL` (the `(kind = 6) = (label IS NOT NULL)` clause already says so) and a `started_at`.

```sql
-- 000033_immunizations: care kind 9, immunization. given_at is stored in
-- started_at; the vaccine and dose live in wire. No table, policy or grant changes.
ALTER TABLE public.care_entities DROP CONSTRAINT care_entities_kind_check;
ALTER TABLE public.care_entities ADD CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 9)));

ALTER TABLE public.care_entities DROP CONSTRAINT care_entities_kind_shape;
ALTER TABLE public.care_entities ADD CONSTRAINT care_entities_kind_shape CHECK (<the baseline expression with ARRAY[2, 3, 4, 5, 6, 8, 9]>);

ALTER TABLE public.sync_page_bindings DROP CONSTRAINT sync_page_bindings_resume_kind_check;
ALTER TABLE public.sync_page_bindings ADD CONSTRAINT sync_page_bindings_resume_kind_check CHECK (((resume_kind >= 1) AND (resume_kind <= 9)));

ALTER TABLE public.import_record_manifests DROP CONSTRAINT import_record_manifests_entity_kind_check;
ALTER TABLE public.import_record_manifests ADD CONSTRAINT import_record_manifests_entity_kind_check
  CHECK ((entity_kind = ANY (ARRAY['feed'::text, 'sleep'::text, 'diaper'::text, 'timer'::text, 'supplement'::text, 'growth_profile'::text, 'measurement'::text, 'immunization'::text])));

COMMENT ON COLUMN public.care_entities.kind IS 'care kind: 1 baby, 2 feed, 3 sleep, 4 diaper, 5 timer, 6 supplement, 7 growth_profile, 8 measurement, 9 immunization';
COMMENT ON COLUMN public.sync_page_bindings.resume_kind IS 'care kind: 1 baby, 2 feed, 3 sleep, 4 diaper, 5 timer, 6 supplement, 7 growth_profile, 8 measurement, 9 immunization';

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 33,principal,object_kind,object_identity,privileges
 FROM public.schema_privilege_manifest WHERE schema_version=32;
UPDATE public.schema_compatibility SET current_version=33,
 introduced_by='000033_immunizations.sql',recorded_at=now() WHERE singleton;
```

The two care CHECKs are declared on the parent and on each of the 16 partitions in the baseline and merged at `ATTACH PARTITION`, so they are inherited and `DROP CONSTRAINT` on the parent removes them from every partition. Step 3's test proves it by inserting kind 9 into every partition; if the `DROP` fails with "cannot drop inherited constraint" or a partition still rejects kind 9, the constraints are local: drop and re-add them per partition (`care_entities_p00` … `care_entities_p15`) before the parent's, in the same file.

- [ ] **Step 2: Regenerate the compatibility manifest**

```bash
cd Server && go run ./cmd/baby-tracker-manifest --migrations migrations --previous internal/migrate/compatibility.json --output internal/migrate/compatibility.json --min 33 --max 33
cat internal/migrate/compatibility.json
```

Expected: `"min": 33, "max": 33`, the unchanged version-32 entry, and a second entry `{"version": 33, "name": "000033_immunizations.sql", "sha256": …}`. From here on, any edit to the `.sql` file needs this step re-run.

- [ ] **Step 3: Write the failing migration test** — `Server/internal/database/immunization_migration_test.go`

Read `Server/internal/testpg/database.go:60-80` for `NewThrough` / `MigrateThrough`, and an existing test in `care_schema_test.go` that inserts a raw `care_entities` row (the shape matrix at `:190-232` has the column list) before writing this. The test:

```go
// Review Focus 4: a schema-32 database with rows of every kind upgrades in
// place; the old rows read back unchanged and every partition then accepts kind 9.
func TestImmunizationMigrationOverABaselineWithRows(t *testing.T) {
	f := testpg.NewThrough(t, 32)
	// 1. Seed one family per partition target is not needed: seed 64 families,
	//    which covers all 16 hash partitions with overwhelming margin, each with
	//    a baby (kind 1) and one row of kinds 2,3,4,6,7,8 — use the same insert
	//    helper the shape matrix uses.
	// 2. Assert a kind-9 insert is refused at 32 (check_violation, 23514).
	// 3. f.MigrateThrough(ctx, 33).
	// 4. Assert count(*) per kind is unchanged and schema_compatibility.current_version = 33.
	// 5. For every seeded family insert a kind-9 row with baby_id, started_at,
	//    label NULL; assert all succeed, and that
	//    SELECT count(DISTINCT tableoid) FROM care_entities WHERE kind = 9  = 16.
	// 6. Assert kind 10 is refused, and kind 9 with started_at NULL is refused,
	//    and kind 9 with a label is refused (each 23514).
	// 7. Assert an import_record_manifests row with entity_kind 'immunization'
	//    passes its CHECK and 'pump' does not.
}
```

Write the body with the file's existing helpers; the seven numbered assertions are the test's contract and all seven must be present.

Run: `BT_TEST_PACKAGES=./internal/database/ BT_TEST_RUN=TestImmunizationMigrationOverABaselineWithRows make server-test`
Expected: PASS already if Step 1 is right (the migration exists). To see it fail first, temporarily run it before Step 1's file is present, or revert the `kind_check` line: it must then fail at assertion 5.

- [ ] **Step 4: Care rules**

`entity.go`:

```go
	KindImmunization int16 = 9
```

and `KindImmunization: "immunization"` in `kindNames`. Update the comment above the constants that says the codes are "fixed by migration 000030 section 1" to name the baseline and 000033.

`typedColumns`, after the `MeasurementPayload` case:

```go
	case protocol.ImmunizationPayload:
		kind = "immunization"
		r.BabyID, r.StartedAt = baby(value.BabyID), timestamp(value.GivenAt)
```

`validate.go`, after the `SupplementPayload` case:

```go
	case protocol.ImmunizationPayload:
		if mutation.EntityKind != "immunization" || (mutation.Operation != "create" && mutation.Operation != "update") {
			return protocol.ErrInvalidMutation
		}
		if !protocol.ValidUUID(payload.BabyID) || !validTimeZone(payload.TimeZoneID) {
			return protocol.ErrInvalidMutation
		}
		if err := validateTimestamp(payload.GivenAt); err != nil {
			return err
		}
		if strings.TrimSpace(payload.Vaccine) == "" {
			return protocol.ErrInvalidText
		}
		if err := validateOptionalText(&payload.Vaccine, 256, 64, false); err != nil {
			return err
		}
		if payload.Dose != nil && (*payload.Dose < 1 || *payload.Dose > 10) {
			return protocol.ErrInvalidMutation
		}
		return validateOptionalText(payload.Note, limits.MaxNoteBytes, 0, true)
```

- `validate.go` delete allowlist (`oneOf(mutation.EntityKind, "baby", …)`): add `"immunization"`.
- `apply.go` `ActionFor`: add `"immunization"` to the `case "feed", "sleep", …` list.
- `records.go`: add `case protocol.ImmunizationPayload: return p.BabyID` to both `mutationBabyID` and `mutationBabyIDFromEntity`. Add nothing to the per-kind apply rules.
- `doc.go:2`: add the kind to the comment.

- [ ] **Step 5: Validation tests** — in `Server/internal/care/validate_test.go`

Add `"immunization.json"` to the fixture list at ~`:117`, and a test modelled on `TestSupplementNameValidation` (`:159-190`), using the same helper that file uses to decode a fixture and validate it:

```go
func TestImmunizationValidation(t *testing.T) {
	// accepted: the fixture; dose null; dose 1; dose 10; a 64-scalar vaccine
	// rejected ErrInvalidMutation: dose 0; dose 11; dose -1; a bad time zone; a non-UUID baby_id
	// rejected ErrInvalidText: vaccine of only spaces; a 65-scalar vaccine; a vaccine with U+0007;
	//   a non-NFC vaccine; a note over limits.MaxNoteBytes
	// rejected (timestamp error): given_at "yesterday"
	// accepted: given_at one year in the future (the server has no future check)
}
```

Every listed case is one table row. Run: `cd Server && go test ./internal/care/ -run 'TestImmunizationValidation|TestKindCodes' -count=1` — `TestKindCodesMatchSchema` fails until Step 6.

- [ ] **Step 6: Tests that pinned "one migration, schema 32"**

- `migrate/compatibility_test.go` `TestOnlyTheBaselineIsEmbedded`: rename to `TestTheBaselineAndItsSuccessorsAreEmbedded`. Expect `names == ["000032_baseline.sql", "000033_immunizations.sql"]`, `c.Min == 33 && c.Max == 33 && len(c.Known) == 2`, `c.Known[0] == baseline` (unchanged digest), `c.Known[1]` equal to version 33, the file name and the sha256 of the embedded 000033 bytes, and `loaded` of length 2 with versions 32, 33. The pre-baseline refusal cases below it stay as they are.
- `migrate/apply_history_test.go`: `overTheBaseline` becomes `overTheEmbedded`: every embedded file plus a synthetic `"000034_next.sql"`. In the three tests replace the literals `33` → `34` and `34` → `35` (`000034_future.sql` → `000035_future.sql`, `version < 33` → `version < 34`, `top != 33` → `top != 34`, `WHERE version = 33` → `= 34`) and the comments.
- `database/baseline_test.go`: both existing tests use `testpg.NewThrough(t, 32)` instead of `testpg.New(t)` — they pin the baseline. Add:

```go
// The current fresh-install catalog, 000033 applied, is pinned beside the baseline's.
func TestCurrentCatalogMatchesItsGolden(t *testing.T) {
	f := testpg.New(t)
	testpg.ProductionShape(t, f)
	got := testpg.SnapshotCatalog(t, f.Owner, testpg.Roles(t, f))
	testkit.AssertGolden(t, "../migrate/testdata/catalog-33.golden", got.Lines)
}
```

  If `ProductionShape` or `Roles` cannot run on a `NewThrough(t, 32)` fixture (they verify roles against the current manifest version), read `Server/internal/testpg/` to find where the version is taken from and pass 32 through; do not weaken either baseline test.
- `care/entity_test.go` `TestKindCodesMatchSchema`: read the two care-kind comments from `../../migrations/000033_immunizations.sql`, expect `len(kinds) == 9`, add `care.KindImmunization: "immunization"` to `constants`, expect `len(constants) == 9`.
- `database/care_schema_test.go`: `:147` `careKinds` gains `, 9 immunization`; the shape matrix loop bound becomes `code <= 10`; the `"dated"` row's kinds become `{2, 3, 4, 8, 9}`; any other row whose expectation for code 9 changes is updated to what the new CHECK says, never loosened beyond it.
- Schema literal sweep. In each file below, change `32` → `33` **only** where it is the current schema version (`v != 32`, `Verify*Role(..., 32)`, `Current != 32`, `Required != 32`): `database/care_schema_test.go`, `request_path_schema_test.go`, `import_schema_test.go`, `identity_schema_test.go`, `family_schema_test.go`, `authority_schema_test.go`, `refresh_recovery_schema_test.go`, `live_activity_schema_test.go`, `roles_test.go`, `tenant/context_test.go:382`.
- `database/roles_test.go:142-205`: the two tests that simulate a future schema with literals shift by one: rows are copied `34 … WHERE schema_version=33`, `VALUES (34, …)`, `current_version=34`, `Verify…Role(…, 34)`.
- Then sweep for anything missed:

```bash
cd Server && grep -rnE "\b32\b" internal cmd scripts --include='*.go' --include='*.sh' | grep -iE "version|schema|compat|current|manifest"
```

  Leave: `testpg.NewThrough(…, 32)`, `Through(…, 32)`, the baseline's own digest entry, `>= 32` thresholds in `check-principals.sh` / `smoke.sh` and the strings `smoke_test.go` pins, and `RunOnce(ctx, 32)` (a batch size).
- `Server/scripts/lib/disposable-sync.sh:324`: `$schema == 32` → `$schema == 33`.
- `Server/README.md:752-755`: the directory now holds the baseline and `000033_immunizations.sql`; the next migration is `000034_<name>.sql`; `catalog-33.golden` pins the current catalog.

- [ ] **Step 7: Record the new golden, then run the server suite**

```bash
BT_GOLDEN_UPDATE=1 BT_TEST_PACKAGES=./internal/database/ BT_TEST_RUN=TestCurrentCatalogMatchesItsGolden make server-test
diff Server/internal/migrate/testdata/catalog-32.golden Server/internal/migrate/testdata/catalog-33.golden | head -80
shasum -a 256 Server/internal/migrate/testdata/catalog-32-chain.golden
```

Read the diff: it must contain only the four CHECKs (parent and partitions), the `compatibility|current_version` line and the manifest lines' version. Anything else is an accident in the migration. The chain golden's digest must still equal `chainGoldenSHA256` in `baseline_test.go`.

Run: `make server-test`
Expected: PASS. The Live Activity composition timing tests in `cmd/baby-tracker-server` are known to fail intermittently under load on `main`; if one fails, re-run that package alone with `BT_TEST_PACKAGES=./cmd/baby-tracker-server/` and report both runs. Any other failure is this task's.

- [ ] **Step 8: Commit**

```bash
cd Server && go vet ./... && go vet -tags identitytest ./... && cd ..
/usr/bin/git add Server
/usr/bin/git commit -m "feat(care): immunization is care kind 9; migration 000033 widens the kind checks

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Server cascade, export, import, sync proofs and API docs

**Files:**
- Create: `Server/internal/sync/immunizations_test.go`
- Create: `Server/api/immunizations.md`
- Modify: `Server/internal/care/cascade.go:18`, `cascade_test.go` (its own order map and kind list ~`:19,51,215`)
- Modify: `Server/internal/care/caretest/caretest.go`, `caretest/batch.go` (a builder for the new kind)
- Modify: `Server/internal/export/archive.go` (`ArchiveV1` ~`:51`, section list ~`:132`), `repository.go` (~`:62`, `:97-117`), `archive_test.go:30`, `service_test.go`, `care_source_test.go` (`TestExportMatchesLegacyCorpus`)
- Modify: `Server/internal/importer/manifest.go` (`parent`), `resolver.go:388`
- Modify: `Server/api/openapi.yaml` (~`:1414,1428,1932,2037,2084,2124,2223`), `docs/protocol/v1.md:583-584`

**Interfaces:**
- Consumes: Tasks 1–2.
- Produces: the archive key `immunizations`; cascade order with `immunization` before `timer`.

- [ ] **Step 1: Failing sync proofs** — `Server/internal/sync/immunizations_test.go`

Read `Server/internal/sync/supplements_test.go` (114 lines) in full; it is the model and uses the same harness. Write three tests with these contracts:

```go
// A shot is created, updated (dose 2 -> 3, a note added) and deleted; each is
// applied, the canonical entity carries given_at/vaccine/dose/note, and a pull
// from cursor 0 serves the tombstone.
func TestImmunizationCreateUpdateDelete(t *testing.T) {}

// Deleting the baby tombstones its shot in the same cascade; a later update of
// the shot is rejected baby_deleted. A shot for a baby of another family is
// rejected baby_not_found and nothing is written.
func TestImmunizationFollowsItsBaby(t *testing.T) {}

// A fresh snapshot serves the live shot after every measurement (kind order 9)
// and omits nothing: CareCount("immunization") == 1.
func TestImmunizationSurvivesSnapshot(t *testing.T) {}
```

Run: `BT_TEST_PACKAGES=./internal/sync/ BT_TEST_RUN=TestImmunization make server-test`
Expected: the first and third pass on Task 2's work or fail only on harness gaps; the cascade assertion order may fail until Step 2. Whatever fails, fix in this task.

- [ ] **Step 2: Cascade order**

`cascade.go:18`:

```go
var cascadeKindOrder = map[string]int{"feed": 0, "sleep": 1, "diaper": 2, "supplement": 3, "growth_profile": 4, "measurement": 5, "immunization": 6, "timer": 7}
```

Mirror it in `cascade_test.go`'s own map and kinds list, and add an immunization row to the test that builds one record of each kind (add a `caretest` builder with digit `9`; digits 2, 3, 4, 6, 7 are taken).

- [ ] **Step 3: Export**

`archive.go` `ArchiveV1`, after `Measurements`:

```go
	Immunizations  []protocol.CanonicalEntity `json:"immunizations"`
```

and `{"immunizations", a.Immunizations}` after `{"measurements", a.Measurements}` in the writer's group list. `repository.go`: initialise `Immunizations: []protocol.CanonicalEntity{}`, add

```go
		case "immunization":
			a.Immunizations = append(a.Immunizations, e)
```

before `case "timer"`, and add `+len(a.Immunizations)` to the 250000 sum.

Tests:
- `archive_test.go:30`: the section list gains `"immunizations"` after `"measurements"`; add `TestEncodeArchiveKeepsImmunizationEntity` beside `TestEncodeArchiveKeepsSupplementEntity`.
- `service_test.go`: apply `immunization.json` beside `supplement.json` and assert `len(archive.Immunizations) == 1`.
- `care_source_test.go` `TestExportMatchesLegacyCorpus` — the golden is not rewritten. Replace the section-order comparison with:

```go
	// The recorded archive predates immunizations: it is the one section the
	// care archive adds, it sits after measurements, and the golden scenario
	// has none, so it is empty.
	var order []string
	for _, name := range got.SectionOrder {
		if name == "immunizations" {
			if string(got.Sections[name]) != "[]" {
				t.Fatalf("the golden scenario has no immunizations: %s", got.Sections[name])
			}
			continue
		}
		order = append(order, name)
	}
	if fmt.Sprint(recorded.SectionOrder) != fmt.Sprint(order) {
		t.Fatalf("section order: recorded %v, care %v", recorded.SectionOrder, order)
	}
	if i := slices.Index(got.SectionOrder, "immunizations"); i < 1 || got.SectionOrder[i-1] != "measurements" {
		t.Fatalf("immunizations is not after measurements: %v", got.SectionOrder)
	}
```

- [ ] **Step 4: Import**

`importer/manifest.go` `parent`: add `case protocol.ImmunizationPayload: return p.BabyID`. `resolver.go:388`: add `&& m.EntityKind != "immunization"` to the allowlist chain. Add `TestImportImmunizationAndReuseCanonicalRecord` modelled line for line on `importer/supplements_test.go:10-40`.

- [ ] **Step 5: API docs**

- `openapi.yaml`: an `ImmunizationPayload` schema beside `SupplementPayload` (`required: [baby_id, given_at, time_zone_id, vaccine, dose, note]`, `dose: {type: [integer, "null"], minimum: 1, maximum: 10}`, `vaccine: {type: string, maxLength: 64}`, `additionalProperties: false`); `immunization` in the three `entity_kind` enums (~`:2037`, `:2124`, `:2223`); a create/update oneOf arm (~`:2084`); `immunizations` in `ArchiveV1` required and properties (~`:1414,1428`). Follow the notation the neighbouring schemas use.
- `Server/api/immunizations.md`: the kind, its payload keys, that `given_at` is stored in `started_at`, that the server checks neither future nor birth date, the exact-33 window, and the fixture's content-hash vector — the shape of `Server/api/supplements.md`.
- `docs/protocol/v1.md:583-584`: add immunization records to the quota sentence.

- [ ] **Step 6: Run and commit**

Run: `make server-test`
Expected: PASS (same known flake note as Task 2).

```bash
cd Server && go vet ./... && go vet -tags identitytest ./... && cd ..
/usr/bin/git add Server docs/protocol/v1.md
/usr/bin/git commit -m "feat(server): immunizations in the cascade, the archive and the import manifest

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Phone model, schema V11 and wire shape

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyStore/Models/ImmunizationEntry.swift`
- Create: `Packages/BabyTracker/Sources/BabyStore/Models/SchemaV11.swift`
- Modify: `Sources/BabyStore/Models/Schema.swift:5,60,75`, `BabyStoreMigrationPlan.swift`, `SchemaV10.swift` (doc comment), `IncompatibleStore.swift:31-32` (doc comment)
- Modify: `Sources/BabyStore/EntryPayload.swift` (after the `SupplementEntry` extension ~`:134`), `MutationFactory.swift:57-68`
- Modify: `Sources/BabyProtocol/WireModels.swift:306-323`, `ContentHash.swift` (`contentPayload` after the supplement case ~`:88`)
- Modify tests: `Tests/BabyStoreTests/BaselineStoreTests.swift`, `GrowthStoreTests.swift:304`, `RefreshRecoveryStoreTests.swift:222,243`, `SingleFormatWriteTests.swift:17-20`, `Tests/BabyProtocolTests/ContentHashTests.swift:27`, `ProtocolFixtureConformanceTests.swift:15,59-77`
- Modify: `Makefile`

(Paths below are relative to `Packages/BabyTracker/` unless they start with `Apps/`, `Server/`, `UITests/`, `scripts/` or `docs/`.)

**Interfaces:**
- Consumes: `Server/api/fixtures/v1/immunization.json` and the two hashes (Task 1).
- Produces: `ImmunizationEntry` (`id, realmKey, babyID, givenAt, timeZoneID, vaccine, dose: Int?, note: String?, createdBy, lastModifiedAt, isTombstoned`; `realmID`), `WireEntityKind.immunization`, `SchemaV11`.

- [ ] **Step 1: Failing tests**

`ContentHashTests.swift`, a vector after the supplement one (the sha256 is left as the literal string `"PENDING"` for the first run):

```swift
        Vector(
            name: "immunization fixture",
            mutationJSON: fixture("immunization.json"),
            canonicalJSON: #"{"entity_kind":"immunization","payload":{"dose":2,"given_at":"2026-09-20T09:30:00.000000Z","note":null,"time_zone_id":"Europe/Zurich","vaccine":"Prevenar 13"},"payload_version":1}"#,
            sha256: "PENDING"
        ),
```

`ProtocolFixtureConformanceTests.swift`: `verifiedFileCount == 22`; add `("immunization.json", nil, "PENDING")` to the mutation-hash argument list.

`SingleFormatWriteTests.swift`: add `(.immunization, 1)` to `currentVersions`.

`BaselineStoreTests.swift`:
- rename `theMigrationPlanHasV9AndV10WithOneStage` → `theMigrationPlanHasV9V10AndV11WithTwoStages`; expect versions `[9.0.0, 10.0.0, 11.0.0]` and `stages.count == 2`;
- `isBaseline` (`:265-269`) builds from `SchemaV11.self`;
- add:

```swift
    @Test func aV10StoreMigratesInPlaceToV11() throws {
        let directory = try Self.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let url = directory.appending(path: "default.store")
        let babyID = UUID()
        let supplementID = UUID()
        try autoreleasepool {
            let schema = Schema(versionedSchema: SchemaV10.self)
            let container = try BabySchema.makeContainer(schema: schema, configurations: [ModelConfiguration(schema: schema, url: url)])
            let context = ModelContext(container)
            context.insert(Baby(id: babyID, name: "Mira", birthDate: .now))
            context.insert(SupplementEntry(id: supplementID, babyID: babyID, name: "Vitamin D", startedAt: .now,
                                           timeZoneID: "UTC", loggedAt: .now))
            try context.save()
        }

        let reopened = ModelContext(try BabySchema.container(storeURL: url, ifIncompatible: .moveAside))

        #expect(try reopened.fetch(FetchDescriptor<Baby>()).map(\.id) == [babyID])
        #expect(try reopened.fetch(FetchDescriptor<SupplementEntry>()).map(\.id) == [supplementID])
        #expect(try reopened.fetch(FetchDescriptor<ImmunizationEntry>()).isEmpty)
        reopened.insert(ImmunizationEntry(babyID: babyID, vaccine: "Prevenar 13", dose: 1, givenAt: .now, timeZoneID: "UTC"))
        try reopened.save()
        #expect(try reopened.fetch(FetchDescriptor<ImmunizationEntry>()).count == 1)
        #expect(try Self.movedAside(in: directory).isEmpty)
        #expect(try Self.isBaseline(url))
    }
```

  Use the `Baby` initialiser the live model has (read `Models/Baby.swift`); the V9 test's `SchemaV9.Baby(id:name:birthDate:)` shows the intent.

`GrowthStoreTests.swift:304`, `RefreshRecoveryStoreTests.swift:222,243`: `SchemaV10.self` → `SchemaV11.self`.

Run: `swift test --filter 'BaselineStoreTests|ContentHashTests|ProtocolFixtureConformanceTests|SingleFormatWriteTests'` (from `Packages/BabyTracker`)
Expected: FAIL to compile (`ImmunizationEntry`, `SchemaV11`, `.immunization`).

- [ ] **Step 2: Model and schema**

`Models/ImmunizationEntry.swift`:

```swift
import Foundation
import SwiftData

/// One vaccination. `vaccine` is free text the family reuses; `dose` is the
/// number on the booklet, when the caregiver gave one.
@Model
public final class ImmunizationEntry {
    public var id: UUID = UUID()
    public var realmKey: String = RealmID.local.encodedKey
    public var babyID: UUID = UUID()
    public var givenAt: Date = Date()
    public var timeZoneID: String = "UTC"
    public var vaccine: String = ""
    public var dose: Int?
    public var note: String?
    public var createdBy: String = ""
    public var lastModifiedAt: Date = Date()
    public var isTombstoned: Bool = false

    public init(id: UUID = UUID(), babyID: UUID, vaccine: String, dose: Int?, givenAt: Date,
                timeZoneID: String, modifiedAt: Date? = nil, createdBy: String = "", realm: RealmID = .local) {
        self.id = id; self.babyID = babyID; self.vaccine = vaccine; self.dose = dose
        self.givenAt = givenAt; self.timeZoneID = timeZoneID
        self.lastModifiedAt = modifiedAt ?? givenAt
        self.createdBy = createdBy; self.realmKey = realm.encodedKey
    }
    public var realmID: RealmID { RealmID(encodedKey: realmKey) ?? .local }
}
```

`Models/SchemaV11.swift`:

```swift
import SwiftData

/// V10 plus `ImmunizationEntry`. Every model here is the live type; a later
/// schema that changes one must freeze it in this file.
public enum SchemaV11: VersionedSchema {
    public static let versionIdentifier = Schema.Version(11, 0, 0)
    public static var models: [any PersistentModel.Type] { SchemaV10.models + [ImmunizationEntry.self] }
}
```

- `SchemaV10.swift` doc comment: add "V11 added a model and changed none, so nothing is frozen here."
- `BabyStoreMigrationPlan.swift`: `schemas` → `[SchemaV9.self, SchemaV10.self, SchemaV11.self]`; add `.lightweight(fromVersion: SchemaV10.self, toVersion: SchemaV11.self)`; update the doc comment.
- `Schema.swift`: the three `SchemaV10` references (`all`, both `Schema(versionedSchema:)`) → `SchemaV11`.
- `IncompatibleStore.swift:31-32`: the comment names V11.

- [ ] **Step 3: Wire shape**

`EntryPayload.swift`, after the supplement extension:

```swift
extension ImmunizationEntry: ChangeLoggable, BabyOwnedRecord, Touchable, RealmScoped {
    public var entityID: UUID { id }
    public var entityTypeName: String { "ImmunizationEntry" }
    public func payloadFields() -> [String: Any] {
        ["baby_id": babyID.uuidString.lowercased(), "given_at": MutationFactory.wireTime(givenAt),
         "time_zone_id": timeZoneID, "vaccine": vaccine,
         "dose": dose as Any? ?? NSNull(), "note": note ?? NSNull()]
    }
}
```

- `MutationFactory.entityKind(forTypeName:)`: `case "ImmunizationEntry": .immunization`.
- `WireModels.swift` `WireEntityKind`: add `immunization` to the first case line and to the `1` arm of `currentPayloadVersion`.
- `ContentHash.swift` `contentPayload`, after `case "supplement":`:

```swift
        case "immunization":
            return .object([
                "dose": p.optionalNumber("dose"),
                "given_at": p.time("given_at"),
                "note": p.optionalText("note"),
                "time_zone_id": p.text("time_zone_id"),
                "vaccine": .string(displayName(p.string("vaccine") ?? "")),
            ])
```

Adding the enum case breaks the two exhaustive `switch kind` in `BabyStore+RemoteApply.swift` (`insertRowIfUnseen`, `writeFields`). For this task add to each:

```swift
        case .immunization:
            throw RemoteEntityError.unsupportedKind(kind.rawValue)
```

Task 6 replaces both.

- [ ] **Step 4: Pin the hashes**

Run the Step 1 filter. The two `PENDING` assertions fail and print the computed hashes. They must equal the two values in Task 1's commit body **exactly**. If they do, paste them in. If either differs, the Swift projection or the server's is wrong: compare the canonical JSON in the vector with the server's `content_hash_test.go` vector and fix the side that deviates. Never paste a Swift-computed hash that differs from the Go one.

- [ ] **Step 5: Green and commit**

Bump `EXPECTED_TESTS` by **3** (migration test, content-hash vector, mutation-hash argument).

Run: `make test`
Expected: `OK: B+3 tests`.

```bash
/usr/bin/git add Packages/BabyTracker Makefile
/usr/bin/git commit -m "feat(store): ImmunizationEntry in schema V11, with its wire shape (+3 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Phone store API and queries

**Files:**
- Create: `Sources/BabyStore/BabyStore+Immunizations.swift`
- Create: `Tests/BabyStoreTests/ImmunizationTests.swift`
- Modify: `Sources/BabyStore/BabyStore.swift` (`recordLocked` ~`:267`), `BabyStore+Queries.swift` (`entry(id:for:)` `:7-18`, `entries(for:on:)` ~`:132`, `entries(for:from:to:)` ~`:163`), `EntryRef.swift`, `HomeSnapshot.swift` (`HistoryItem` `:68-101`), `BabyStore+Growth.swift` (nothing; read `validateMeasurement` `:190-200` as the model)
- Modify (compile-only arms, filled by later tasks): `Sources/LoggingFeature/LogCoordinator.swift` (`EntryRef.kindName`), `Sources/HistoryFeature/HistoryDayBuilder.swift` (item loop `:63-77`), `HistoryViewModel.swift` (`oldestMoment` `:220`, `entryRef(for:)` `:246`, `delete` `:277`), `Sources/InsightsFeature/InsightsViewModel.swift:50-55`, `Sources/AskFeature/AskCareLog.swift` (`line(for:)`, `kind(of:)`, `source(of:)`, `summary`)
- Modify: `Makefile`

**Interfaces:**
- Consumes: `ImmunizationEntry` (Task 4); `SupplementName.normalized`, `BabyStore.supplementNameKey`, `LocalDate(displaying:in:)`, `Baby.localBirthDate`.
- Produces:

```swift
public enum ImmunizationStoreError: Error, Equatable, Sendable {
    case babyUnavailable, entryUnavailable, invalidDose, invalidDate, invalidTimeZone, futureDate, beforeBirth
}
public extension BabyStore {
    static let immunizationDoses: ClosedRange<Int>   // 1...10
    @discardableResult
    func logImmunization(vaccine: String, dose: Int?, for babyID: UUID, at date: Date? = nil,
                         note: String? = nil, expectedWriteContext: RealmWriteContext? = nil) throws -> ImmunizationEntry
    func editImmunization(_ entry: ImmunizationEntry, vaccine: String, dose: Int?, at date: Date, note: String?,
                          expectedWriteContext: RealmWriteContext? = nil) throws
    func immunizations(for babyID: UUID) throws -> [ImmunizationEntry]   // newest givenAt first
    func lastImmunization(for babyID: UUID) throws -> ImmunizationEntry?
    func vaccineNames() throws -> [String]                               // realm-wide, latest use first
    func nextDose(of vaccine: String, for babyID: UUID) throws -> Int
}
public enum HistoryItem { … case immunization(ImmunizationEntry) }   // startedAt == givenAt
public enum EntryRef { … case immunization(UUID) }
```

- [ ] **Step 1: Failing tests** — `Tests/BabyStoreTests/ImmunizationTests.swift`

```swift
import Foundation
import SwiftData
import Testing
import BabyCore
import BabyProtocol
@testable import BabyStore

@MainActor @Suite("Immunizations")
struct ImmunizationTests {
    /// Noon UTC, so a day's shift never crosses midnight in the test zone.
    static let noon = Date(timeIntervalSince1970: 1_800_014_400)

    private func setup(bornDaysAgo: Double = 200) throws -> (BabyStore, Baby, TestDateProvider) {
        let clock = TestDateProvider(Self.noon)
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: clock.now.addingTimeInterval(-bornDaysAgo * 86_400),
                                     referenceSex: .girls, expectedDueDate: nil)
        return (store, baby, clock)
    }

    @Test func aShotIsLoggedEditedAndSoftDeleted() throws {
        let (store, baby, clock) = try setup()
        let shot = try store.logImmunization(vaccine: " Prevenar 13 ", dose: 1, for: baby.id, note: "left thigh")
        #expect(shot.vaccine == "Prevenar 13")
        #expect(shot.dose == 1)
        #expect(shot.givenAt == clock.now)
        #expect(shot.timeZoneID == TimeZone.current.identifier)
        #expect(try store.immunizations(for: baby.id).map(\.id) == [shot.id])
        let earlier = clock.now.addingTimeInterval(-86_400)
        try store.editImmunization(shot, vaccine: "Prevenar 20", dose: nil, at: earlier, note: nil)
        #expect(shot.vaccine == "Prevenar 20")
        #expect(shot.dose == nil)
        #expect(shot.givenAt == earlier)
        #expect(shot.note == nil)
        try store.softDelete(shot)
        #expect(try store.immunizations(for: baby.id).isEmpty)
        #expect(try store.vaccineNames().isEmpty)
    }

    @Test func invalidNamesAndDosesWriteNothing() throws {
        let (store, baby, _) = try setup()
        #expect(throws: SupplementName.ValidationError.self) { try store.logImmunization(vaccine: "   ", dose: nil, for: baby.id) }
        #expect(throws: SupplementName.ValidationError.self) {
            try store.logImmunization(vaccine: String(repeating: "a", count: 65), dose: nil, for: baby.id)
        }
        for dose in [0, 11, -1] {
            #expect(throws: ImmunizationStoreError.invalidDose) { try store.logImmunization(vaccine: "MMR", dose: dose, for: baby.id) }
        }
        #expect(try store.logImmunization(vaccine: String(repeating: "a", count: 64), dose: 10, for: baby.id).dose == 10)
        #expect(try store.context.fetch(FetchDescriptor<ImmunizationEntry>()).count == 1)
    }

    // Review Focus 1.
    @Test func aFutureOrPreBirthTimeIsRefusedAndWritesNothing() throws {
        let (store, baby, clock) = try setup(bornDaysAgo: 10)
        #expect(throws: ImmunizationStoreError.futureDate) {
            try store.logImmunization(vaccine: "MMR", dose: nil, for: baby.id, at: clock.now.addingTimeInterval(1))
        }
        #expect(throws: ImmunizationStoreError.beforeBirth) {
            try store.logImmunization(vaccine: "MMR", dose: nil, for: baby.id, at: clock.now.addingTimeInterval(-12 * 86_400))
        }
        #expect(throws: ImmunizationStoreError.babyUnavailable) {
            try store.logImmunization(vaccine: "MMR", dose: nil, for: UUID())
        }
        #expect(try store.context.fetch(FetchDescriptor<ImmunizationEntry>()).isEmpty)
        // The birth day itself is allowed, and so is exactly now.
        let shot = try store.logImmunization(vaccine: "Hep B", dose: 1, for: baby.id, at: baby.birthDate)
        #expect(throws: ImmunizationStoreError.futureDate) {
            try store.editImmunization(shot, vaccine: "Hep B", dose: 1, at: clock.now.addingTimeInterval(60), note: nil)
        }
        #expect(shot.givenAt == baby.birthDate)
    }

    // Review Focus 3.
    @Test func namesAreRealmWideMergedAndOrderedByLatestShot() throws {
        let (store, mira, clock) = try setup()
        let noah = try store.addBaby(name: "Noah", birthDate: clock.now.addingTimeInterval(-400 * 86_400),
                                     referenceSex: .boys, expectedDueDate: nil)
        let day = 86_400.0
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: noah.id, at: clock.now.addingTimeInterval(-90 * day))
        try store.logImmunization(vaccine: "Infanrix hexa", dose: 1, for: mira.id, at: clock.now.addingTimeInterval(-30 * day))
        let respelled = try store.logImmunization(vaccine: "  prevenar 13 ", dose: 1, for: mira.id, at: clock.now.addingTimeInterval(-5 * day))
        #expect(respelled.vaccine == "Prevenar 13")
        #expect(try store.vaccineNames() == ["Prevenar 13", "Infanrix hexa"])
        let last = try store.immunizations(for: mira.id)
        try store.softDelete(last[0])
        #expect(try store.vaccineNames() == ["Infanrix hexa", "Prevenar 13"])
    }

    @Test func nextDoseCountsThisBabyOnly() throws {
        let (store, mira, clock) = try setup()
        let noah = try store.addBaby(name: "Noah", birthDate: clock.now.addingTimeInterval(-400 * 86_400),
                                     referenceSex: .boys, expectedDueDate: nil)
        #expect(try store.nextDose(of: "Prevenar 13", for: mira.id) == 1)
        try store.logImmunization(vaccine: "Prevenar 13", dose: 3, for: noah.id)
        #expect(try store.nextDose(of: "Prevenar 13", for: mira.id) == 1)
        try store.logImmunization(vaccine: "Prevenar 13", dose: nil, for: mira.id)
        #expect(try store.nextDose(of: "Prevenar 13", for: mira.id) == 1)
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: mira.id)
        let second = try store.logImmunization(vaccine: "prevenar 13", dose: 2, for: mira.id)
        #expect(try store.nextDose(of: " PREVENAR 13 ", for: mira.id) == 3)
        try store.softDelete(second)
        #expect(try store.nextDose(of: "Prevenar 13", for: mira.id) == 2)
        try store.logImmunization(vaccine: "Prevenar 13", dose: 10, for: mira.id)
        #expect(try store.nextDose(of: "Prevenar 13", for: mira.id) == 10)
        #expect(try store.nextDose(of: "   ", for: mira.id) == 1)
    }

    @Test func shotsJoinTheBabysEntriesAndNoOtherBabys() throws {
        let (store, mira, clock) = try setup()
        let noah = try store.addBaby(name: "Noah", birthDate: clock.now.addingTimeInterval(-400 * 86_400),
                                     referenceSex: .boys, expectedDueDate: nil)
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: mira.id)
        let items = try store.entries(for: mira.id, from: .distantPast, to: .distantFuture)
        #expect(items.map(\.id) == [shot.id])
        #expect(items.first?.startedAt == shot.givenAt)
        #expect(try store.entries(for: mira.id, on: clock.now).map(\.id) == [shot.id])
        #expect(try store.entries(for: noah.id, from: .distantPast, to: .distantFuture).isEmpty)
        guard case .immunization(let found)? = try store.entry(id: shot.id, for: mira.id) else {
            Issue.record("entry(id:for:) did not return the shot"); return
        }
        #expect(found.id == shot.id)
        #expect(try store.entry(id: shot.id, for: noah.id) == nil)
        #expect(try store.lastImmunization(for: mira.id)?.id == shot.id)
    }

    @Test func aGenericUpdateNormalisesTheNameAndRollsBackABadOne() throws {
        let (store, baby, _) = try setup()
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: baby.id)
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby.id)
        shot.vaccine = " prevenar 13 "
        try store.update(shot)
        #expect(shot.vaccine == "Prevenar 13")
        shot.vaccine = "   "
        #expect(throws: (any Error).self) { try store.update(shot) }
        #expect(shot.vaccine == "Prevenar 13")
    }

    @Test func aStaleEntryCannotBeEditedInAnotherRealm() throws {
        let (store, baby, clock) = try setup()
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby.id)
        try store.setActiveRealm(Fixture.sharedRealm)
        #expect(throws: RealmWriteError.wrongRealm) {
            try store.editImmunization(shot, vaccine: "MMR", dose: 2, at: clock.now, note: nil)
        }
        #expect(shot.dose == 1)
    }

    @Test func deletingABabyKeepsItsShotsOnThisPhone() throws {
        let (store, baby, _) = try setup()
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby.id)
        try store.deleteBaby(baby)
        #expect(try store.context.fetch(FetchDescriptor<ImmunizationEntry>()).map(\.id) == [shot.id])
        #expect(!shot.isTombstoned)
    }
}
```

`entries(for:on:)`'s parameter label: read its signature in `BabyStore+Queries.swift:110` and match it. If `HistoryItem` has no `Equatable`, the `== nil` line becomes a `switch`. `genericUpdateValidatesAndRollsBackName` in `SupplementTests.swift` shows how the supplement suite asserts the rollback; match its form if `store.update` differs.

Run: `swift test --filter BabyStoreTests.ImmunizationTests`
Expected: FAIL to compile.

- [ ] **Step 2: Implement the store API** — `BabyStore+Immunizations.swift`

```swift
import Foundation
import SwiftData
import BabyCore
import BabyProtocol

public enum ImmunizationStoreError: Error, Equatable, Sendable {
    case babyUnavailable
    case entryUnavailable
    case invalidDose
    case invalidDate
    case invalidTimeZone
    case futureDate
    case beforeBirth
}

public extension BabyStore {
    static let immunizationDoses = 1...10

    @discardableResult
    func logImmunization(vaccine: String, dose: Int?, for babyID: UUID, at date: Date? = nil,
                         note: String? = nil, expectedWriteContext: RealmWriteContext? = nil) throws -> ImmunizationEntry {
        let expected = try expectedWriteContext ?? captureWriteContext()
        return try withRealmWrite(expected: expected) {
            let when = date ?? self.now
            let zone = TimeZone.current.identifier
            try self.validateImmunization(dose: dose, for: babyID, at: when, timeZoneID: zone)
            let name = try self.savedVaccineName(vaccine)
            let entry = ImmunizationEntry(babyID: babyID, vaccine: name, dose: dose, givenAt: when, timeZoneID: zone,
                                          modifiedAt: self.now, createdBy: self.deviceID, realm: self.activeRealm)
            entry.note = note
            self.context.insert(entry)
            try self.recordLocked(.create, on: entry)
            return entry
        }
    }

    func editImmunization(_ entry: ImmunizationEntry, vaccine: String, dose: Int?, at date: Date, note: String?,
                          expectedWriteContext: RealmWriteContext? = nil) throws {
        let expected = try expectedWriteContext ?? captureWriteContext()
        try withRealmWrite(expected: expected) {
            guard entry.realmKey == expected.realm.encodedKey else { throw RealmWriteError.wrongRealm }
            guard entry.modelContext === self.context, !entry.isTombstoned else { throw ImmunizationStoreError.entryUnavailable }
            try self.validateImmunization(dose: dose, for: entry.babyID, at: date, timeZoneID: entry.timeZoneID)
            entry.vaccine = try self.savedVaccineName(vaccine, excluding: entry.id)
            entry.dose = dose
            entry.givenAt = date
            entry.note = note
            entry.lastModifiedAt = self.now
            try self.recordLocked(.update, on: entry)
        }
    }

    /// Newest shot first.
    func immunizations(for babyID: UUID) throws -> [ImmunizationEntry] {
        try immunizationRows().filter { $0.babyID == babyID }
    }

    func lastImmunization(for babyID: UUID) throws -> ImmunizationEntry? {
        try immunizations(for: babyID).first
    }

    /// Every vaccine the family has logged, for any baby: the one given most
    /// recently first, each in the spelling of its earliest shot.
    func vaccineNames() throws -> [String] {
        let rows = try immunizationRows()
        var spelling: [String: String] = [:]
        for row in rows.reversed() where spelling[Self.supplementNameKey(row.vaccine)] == nil {
            spelling[Self.supplementNameKey(row.vaccine)] = row.vaccine
        }
        var seen: Set<String> = []
        return rows.compactMap { row in
            let key = Self.supplementNameKey(row.vaccine)
            return seen.insert(key).inserted ? spelling[key] : nil
        }
    }

    /// One more than this baby's highest dose of `vaccine`, at most 10; 1 when
    /// there is none, or the name is not a name yet.
    func nextDose(of vaccine: String, for babyID: UUID) throws -> Int {
        guard let name = try? SupplementName.normalized(vaccine) else { return 1 }
        let key = Self.supplementNameKey(name)
        let highest = try immunizations(for: babyID)
            .filter { Self.supplementNameKey($0.vaccine) == key }
            .compactMap(\.dose).max() ?? 0
        return min(highest + 1, Self.immunizationDoses.upperBound)
    }
}

extension BabyStore {
    /// The active realm's live shots, newest `givenAt` first. Fetched ascending
    /// and reversed, so of two shots at one instant the later insert is first
    /// (see `lastFeed`).
    func immunizationRows() throws -> [ImmunizationEntry] {
        let key = activeRealm.encodedKey
        var descriptor = FetchDescriptor<ImmunizationEntry>(predicate: #Predicate {
            $0.realmKey == key && !$0.isTombstoned
        })
        descriptor.sortBy = [SortDescriptor(\.givenAt, order: .forward)]
        return Array(try context.fetch(descriptor).reversed())
    }

    /// `raw` normalised, in the spelling the family already uses for it.
    func savedVaccineName(_ raw: String, excluding id: UUID? = nil) throws -> String {
        let name = try SupplementName.normalized(raw)
        let key = Self.supplementNameKey(name)
        return try immunizationRows().reversed().first {
            $0.id != id && Self.supplementNameKey($0.vaccine) == key
        }?.vaccine ?? name
    }

    /// Growth's date rule (`validateMeasurement`): not after now, not before
    /// the birth day in the entry's own zone.
    func validateImmunization(dose: Int?, for babyID: UUID, at date: Date, timeZoneID: String) throws {
        if let dose, !Self.immunizationDoses.contains(dose) { throw ImmunizationStoreError.invalidDose }
        guard let baby = try baby(babyID), !baby.isTombstoned else { throw ImmunizationStoreError.babyUnavailable }
        guard date.timeIntervalSince1970.isFinite, baby.birthDate.timeIntervalSince1970.isFinite else {
            throw ImmunizationStoreError.invalidDate
        }
        guard let zone = TimeZone(identifier: timeZoneID) else { throw ImmunizationStoreError.invalidTimeZone }
        guard date <= now else { throw ImmunizationStoreError.futureDate }
        let givenDay = try LocalDate(displaying: date, in: zone)
        let birthDay = try baby.localBirthDate ?? LocalDate(displaying: baby.birthDate, in: .current)
        guard givenDay >= birthDay else { throw ImmunizationStoreError.beforeBirth }
    }
}
```

`deletingABabyKeepsItsShotsOnThisPhone` logs before the delete, so `validateImmunization`'s live-baby rule does not block it.

`BabyStore.swift` `recordLocked`, after the supplement hook:

```swift
            if let shot = entity as? ImmunizationEntry, operation != .delete {
                shot.vaccine = try savedVaccineName(shot.vaccine, excluding: shot.id)
            }
```

- [ ] **Step 3: Queries, `HistoryItem`, `EntryRef`**

- `EntryRef.swift`: add `immunization(UUID)` to the case line and to the `id` switch.
- `HomeSnapshot.swift` `HistoryItem`: add `case immunization(ImmunizationEntry)`; `id` → `entry.id`; `startedAt` → `entry.givenAt`; `timeZoneID` → `entry.timeZoneID`.
- `BabyStore+Queries.swift`:
  - `entry(id:for:)`: the last line becomes

```swift
        if let med = try supplementRows(for: babyID).first(where: { $0.id == id }) { return .supplement(med) }
        return try immunizations(for: babyID).first(where: { $0.id == id }).map(HistoryItem.immunization)
```

  - `entries(for:on:)` and `entries(for:from:to:)`: beside each `supplements` line add

```swift
        let shots = try immunizations(for: babyID).filter { $0.givenAt >= start && $0.givenAt < end }.map(HistoryItem.immunization)
```

    and include `+ shots` in the sorted concatenation.

- [ ] **Step 4: Compile-only arms in the UI targets**

Each is the smallest arm that compiles and does no harm; the named task replaces it.

- `LogCoordinator.swift` `EntryRef.kindName`: `case .immunization: "immunization"` (final).
- `HistoryDayBuilder.swift` item loop: `case .immunization: continue` (Task 10).
- `HistoryViewModel.swift`: `oldestMoment` → the shot's `givenAt` (final); `entryRef(for:)` → `case .immunization(let e)?: return .immunization(e.id)` (final); `delete` → an arm

```swift
        case .entry(.immunization(let id)):
            if case .immunization(let shot)? = try store.entry(id: id, for: babyID) { try store.softDelete(shot) }
```

  (final).
- `InsightsViewModel.swift`: `case .immunization: break` (final).
- `AskCareLog.swift`: `line(for:)` → `"\(text.time(entry.givenAt)) · vaccine · \(entry.vaccine)"`; `kind(of:)` → `"vaccine"`; `source(of:)` → kind `"immunization"`; `summary` → `case .immunization: break` (Task 12 finishes all four).

Run: `swift build` (from `Packages/BabyTracker`)
Expected: builds. If another exhaustive switch over `HistoryItem` or `EntryRef` surfaces, give it the same minimal arm and list it in the commit body.

- [ ] **Step 5: Green and commit**

Bump `EXPECTED_TESTS` by **9**.

Run: `make test`
Expected: `OK: B+12 tests`. Existing Ask and History tests must pass unchanged; if one fails, an interim arm changed behaviour for existing kinds.

```bash
/usr/bin/git add Packages/BabyTracker Makefile
/usr/bin/git commit -m "feat(store): log, edit and read immunizations; family-wide vaccine names and the next dose (+9 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Phone sync — remote apply, realm moves, import

**Files:**
- Create: `Tests/BabyStoreTests/ImmunizationSyncTests.swift`
- Modify: `Sources/BabyStore/BabyStore+RemoteApply.swift` (`applyCanonical` gate ~`:219` and stamp ~`:250`; `insertRowIfUnseen` ~`:275`; `writeFields` ~`:405`; `RemoteEntityError` ~`:586`)
- Modify: `Sources/BabyStore/BabyStore+Outbox.swift` (`deleteRows` ~`:225-239`), `AppGroupWriteBoundary.swift` (`commitTransition` ~`:112-120`), `BabyStore+Import.swift` (`freezeImportManifest` ~`:87`, `remapBabiesAndDependents` ~`:280`), `Models/ImportCheckpoint.swift:17-19` and `BabyStore+Babies.swift:131-144` (`MigratedEntityKind`, parity)
- Modify tests: `Tests/BabyStoreTests/OwnerReplayTests.swift:40-41,156`
- Modify: `Apps/BabyTrackerApp/UITestSupport.swift:62-74` (`emptyStore`)
- Modify: `Makefile`

**Interfaces:**
- Consumes: Tasks 4–5; test helpers `BabyStore.sharedFixture`, `Fixture`, `SyncFixture`, `SnapshotFixture`, `WireFixture` (`Tests/BabyStoreTests/SyncIntentTestAdapter.swift`, `RemoteApplyTests.swift:293-374`, `SnapshotApplyTests.swift:96-130`, `WireShapeTests.swift:388-456`).
- Produces: a shot written by another phone appears, updates and disappears here; `RemoteEntityError.invalidImmunizationPayload`.

- [ ] **Step 1: Failing tests** — `ImmunizationSyncTests.swift`

Read `SupplementTests.swift:52-69` (remote apply), `:117-135` (snapshot) and the three tests quoted below in that file before writing; the helpers' exact signatures are there. `Fixture.babyID` is a baby the shared fixture already has; give it a birth date early enough for the fixture's `2026-09-20` shot if `sharedFixture` does not.

```swift
@MainActor @Suite("Immunization sync")
struct ImmunizationSyncTests {
    @Test func theOutboxMatchesTheFixtureAndTheDeletePayload() throws {
        let store = try BabyStore.sharedFixture()
        let fixture = try WireFixture.mutation("immunization.json")
        let row = try store.logImmunization(vaccine: "Prevenar 13", dose: 2, for: Fixture.babyID,
                                            at: WireFixture.date("2026-09-20T09:30:00Z"))
        row.timeZoneID = "Europe/Zurich"
        try store.clearOutboxForTesting()
        try store.record(.create, on: row)
        let mutation = try WireFixture.onlyMutation(in: store, for: row.id)
        #expect(mutation.entityKind == .immunization)
        #expect(mutation.payloadVersion == 1)
        try WireFixture.expectSameShape(mutation.payload, fixture.payload)
        #expect(try ProtocolCanonicalizer.contentSHA256(entityKind: "immunization", payload: mutation.payload) == "<the content hash pinned in Task 4>")
        try store.clearOutboxForTesting()
        try store.softDelete(row)
        let deletion = try WireFixture.onlyMutation(in: store, for: row.id)
        #expect(deletion.payload == .object(["baby_id": .string(Fixture.babyID.uuidString.lowercased())]))
    }

    /// A shot without a dose goes out with `"dose": null`, never without the key.
    @Test func aShotWithoutADoseSendsNull() throws {}

    /// create -> the row exists with vaccine, dose, note, givenAt, zone, createdBy and
    /// lastModifiedAt from the canonical meta; update (dose 2 -> null, note set,
    /// given_at moved, vaccine respelled) -> every field follows; tombstone -> gone
    /// from immunizations(for:) and vaccineNames().
    @Test func aRemoteShotIsCreatedUpdatedAndTombstoned() throws {}

    /// A fresh snapshot page holding the shot materialises it; rematerialising
    /// after a pending local edit keeps the local dose over the base.
    @Test func aSnapshotCarriesTheShot() throws {}

    // Review Focus 2. Each of: given_at missing; vaccine missing; vaccine blank;
    // dose 2.5; dose "2"; dose 11. The page throws, the cursor does not advance,
    // no ImmunizationEntry exists, and a valid supplement in the same page is not applied.
    @Test(arguments: ["no given_at", "no vaccine", "blank vaccine", "fractional dose", "string dose", "dose 11"])
    func aMalformedRemoteShotRefusesThePage(_ fault: String) throws {}

    @Test func realmMoveAndPurgeIncludeShotsWithoutTouchingLocalRows() throws {}

    @Test func importCapturesTheShotAndRemapsItsBaby() throws {}
}
```

The four empty bodies and the two "…Includes…" tests are written out in full against the helpers, following the supplement tests named in their comments (`realmMoveAndPurgeIncludeSupplementsWithoutTouchingLocalRows`, `importIncludesSupplementAndRemapsParent`, `snapshotSupportsSupplementAndUnknownKindRollsBackWholePage`); each comment is the test's contract and every clause in it is asserted. The realm-move test needs a real baby in both realms because `logImmunization` validates the baby.

`OwnerReplayTests.swift`: add `"immunization.json"` to the fixture list (`:40-41`) and

```swift
        #expect(try store.context.fetch(FetchDescriptor<ImmunizationEntry>()).map(\.vaccine) == ["Prevenar 13"])
```

beside the supplement assertion (`:156`).

Run: `swift test --filter 'ImmunizationSyncTests|OwnerReplayTests'`
Expected: FAIL (`unsupportedKind`, missing realm-move and import lines).

- [ ] **Step 2: Remote apply**

`RemoteEntityError`: add `case invalidImmunizationPayload`.

`applyCanonical`, beside the supplement payload gate — a live shot must carry what the row needs, checked before anything is written:

```swift
        if kind == .immunization, entity.meta.tombstonedAt == nil {
            guard case let .object(fields) = entity.payload,
                  case let .string(givenAt) = fields["given_at"], (try? Self.wireDate(givenAt)) != nil,
                  case let .string(vaccine) = fields["vaccine"], (try? SupplementName.normalized(vaccine)) != nil,
                  Self.remoteDose(fields["dose"]) != .invalid else {
                throw RemoteEntityError.invalidImmunizationPayload
            }
        }
```

with, in the same extension:

```swift
    enum RemoteDose: Equatable { case none, dose(Int), invalid }

    /// `null` or absent is no dose; a whole number 1...10 is a dose; anything else is malformed.
    static func remoteDose(_ value: JSONValue?) -> RemoteDose {
        switch value {
        case nil, .null?: return .none
        case .number(let number)?:
            let whole = NSDecimalNumber(decimal: number).intValue
            guard Decimal(whole) == number, immunizationDoses.contains(whole) else { return .invalid }
            return .dose(whole)
        default: return .invalid
        }
    }
```

`JSONValue.number`'s associated type is what `ContentHash.swift` uses (`Decimal`, per `ProtocolFixtureLoader.swift:117`); if it is `Double`, compare with `number.rounded() == number` instead.

`applyCanonical`, beside the supplement attribution stamp:

```swift
            if kind == .immunization, let row = try context.fetch(FetchDescriptor<ImmunizationEntry>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).first {
                row.lastModifiedAt = try Self.wireDate(entity.meta.acceptedAt)
                row.createdBy = entity.meta.creatorAccountID.uuidString.lowercased()
            }
```

`insertRowIfUnseen`, replacing Task 4's throwing arm:

```swift
        case .immunization:
            guard try context.fetch(FetchDescriptor<ImmunizationEntry>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).isEmpty else { return }
            guard let babyID = uuid("baby_id"), let givenAt = date("given_at"),
                  let vaccine = string("vaccine"), let zone = string("time_zone_id") else {
                throw RemoteEntityError.invalidImmunizationPayload
            }
            context.insert(ImmunizationEntry(id: entityID, babyID: babyID, vaccine: try SupplementName.normalized(vaccine),
                                             dose: fields["dose"] as? Int, givenAt: givenAt, timeZoneID: zone, realm: realm))
```

`writeFields`, replacing Task 4's throwing arm:

```swift
        case .immunization:
            guard let row = try context.fetch(FetchDescriptor<ImmunizationEntry>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).first else { return }
            if let vaccine = fields["vaccine"] as? String { row.vaccine = try SupplementName.normalized(vaccine) }
            if let value = fields["given_at"] as? String { row.givenAt = try Self.wireDate(value) }
            if let value = fields["time_zone_id"] as? String { row.timeZoneID = value }
            // Present and null clears it, as a measurement's optional numbers do.
            if fields["dose"] != nil { row.dose = fields["dose"] as? Int }
            if let value = optionalString("note") { row.note = value }
            if let value = fields["isTombstoned"] as? Bool { row.isTombstoned = value }
```

Leave `applyOutcome`'s `isGrowth` branch alone: shots follow supplements there.

- [ ] **Step 3: Realm move, purge, import**

- `AppGroupWriteBoundary.swift` `commitTransition`, beside the supplement line:

```swift
            for row in try context.fetch(FetchDescriptor<ImmunizationEntry>(predicate: #Predicate { $0.realmKey == sourceKey })) { row.realmKey = targetKey }
```

- `BabyStore+Outbox.swift` `deleteRows`, beside the supplement line:

```swift
        for row in try context.fetch(FetchDescriptor<ImmunizationEntry>(predicate: #Predicate { $0.realmKey == realmKey })) { context.delete(row) }
```

- `BabyStore+Import.swift` `freezeImportManifest`, after the supplement loop:

```swift
        for shot in try localRows(ImmunizationEntry.self) where !shot.isTombstoned {
            try capture(.immunization, shot.id, shot.payloadFields())
        }
```

  and `remapBabiesAndDependents`, after the supplement loop:

```swift
        for row in try localRows(ImmunizationEntry.self) {
            if let canonical = mapping[row.babyID] { row.babyID = canonical }
        }
```

- `ImportCheckpoint.swift` `MigratedEntityKind` and `BabyStore+Babies.swift` `migratedEntityKinds(in:)`: add the immunization line beside the supplement one in each.
- `Apps/BabyTrackerApp/UITestSupport.swift` `emptyStore`: add `try? context.delete(model: ImmunizationEntry.self)` beside the other model deletes. Vaccine names are realm-wide, so a shot left by one UI test would show in the next test's picker.

- [ ] **Step 4: Green and commit**

Bump `EXPECTED_TESTS` by **12** (six plain tests + six arguments).

Run: `make test`
Expected: `OK: B+24 tests`.

```bash
/usr/bin/git add Packages/BabyTracker Apps/BabyTrackerApp/UITestSupport.swift Makefile
/usr/bin/git commit -m "feat(sync): immunizations apply from the server, move with the realm and join the import (+12 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: Glyph, tracker and colour

**Files:**
- Create: `Sources/BabyUI/Resources/Icons.xcassets/icon-vaccine.imageset/Contents.json`, `icon.svg`
- Modify: `Sources/BabyUI/BabyIcon.swift:6-7`, `Palette.swift` (colour list ~`:6-20`, `Tracker` `:33-77`), `ErrorBanner.swift` (`ErrorSubject` ~`:24-33`), `Sources/BabyUI/Localizable.xcstrings`
- Modify: `Sources/HistoryFeature/HistoryView.swift` (`HistoryCopy.filterName` `:499-507`), `Sources/HistoryFeature/Localizable.xcstrings`
- Modify tests: `Tests/BabyUITests/PaletteTests.swift:14-21`
- Modify: `Makefile` (no count change)

**Interfaces:**
- Produces: `BabyIcon.vaccine`, `Palette.vaccine`, `Tracker.vaccine` (title "Vaccine"), `ErrorSubject.vaccination`.

- [ ] **Step 1: Failing test**

`PaletteTests.swift` `trackerTitlesAreSentenceCase`: add `#expect(Tracker.vaccine.title == "Vaccine")`.

Run: `swift test --filter BabyUITests`
Expected: FAIL to compile.

- [ ] **Step 2: The glyph**

`icon-vaccine.imageset/Contents.json` — identical to `icon-meds.imageset/Contents.json`:

```json
{ "images": [{ "filename": "icon.svg", "idiom": "universal" }],
  "info": { "author": "xcode", "version": 1 },
  "properties": { "preserves-vector-representation": true, "template-rendering-intent": "template" } }
```

`icon.svg` — a syringe on the diagonal, house style (24×24, stroke only, 1.8, round caps), one line:

```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="24" height="24" fill="none" stroke="#000" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="m17.5 3 3.5 3.5"/><path d="m19.2 4.8-3 3"/><path d="m13.6 6 4.4 4.4"/><path d="m14.8 7.2-8 8v2h2l8-8"/><path d="m11 11 1.6 1.6"/><path d="m8.6 13.4 1.6 1.6"/><path d="m6.8 17.2-3.6 3.6"/></svg>
```

It must not read as the meds glyph (a capsule and a drop). Step 5 checks it at 14 pt and 24 pt; adjust the paths there if it is illegible, keeping the style attributes.

- [ ] **Step 3: Code**

- `BabyIcon`: append `vaccine` to the case list.
- `Palette`: `public static let vaccine = Color(red: 0.96, green: 0.60, blue: 0.76)   // #F49AC1`. (`#F49AC1` was pencilled in for pump in an unbuilt plan; pumping picks another when it is built.) `PaletteTests` requires uniqueness among `Tracker` colours and contrast ≥ 4.5:1 on both test grounds; if either fails, darken or lighten in steps of 0.02 per channel until both pass and record the final hex in the comment.
- `Tracker`: add `vaccine` to the case list; `color` → `Palette.vaccine`; `icon` → `.vaccine`; `title` → `String(localized: "Vaccine", bundle: .module)`.
- `ErrorSubject`: `public static let vaccination = String(localized: "vaccination", bundle: .module)`.
- `HistoryCopy.filterName`: `case .vaccine: L10n.tr("Vaccines")`.

- [ ] **Step 4: Strings**

`/tmp/imm-babyui.json`:

```json
{
  "Vaccine": {"de": "Impfung", "fr": "Vaccin", "es": "Vacuna", "it": "Vaccino", "uk": "Щеплення"},
  "vaccination": {"de": "Impfung", "fr": "vaccination", "es": "vacunación", "it": "vaccinazione", "uk": "щеплення"}
}
```

`/tmp/imm-history-filter.json`:

```json
{
  "Vaccines": {"de": "Impfungen", "fr": "Vaccins", "es": "Vacunas", "it": "Vaccini", "uk": "Щеплення"}
}
```

```bash
grep -c '"Vaccine"\|"vaccination"' Packages/BabyTracker/Sources/BabyUI/Localizable.xcstrings      # expected 0
python3 scripts/add-catalog-strings.py Packages/BabyTracker/Sources/BabyUI/Localizable.xcstrings /tmp/imm-babyui.json
python3 scripts/add-catalog-strings.py Packages/BabyTracker/Sources/HistoryFeature/Localizable.xcstrings /tmp/imm-history-filter.json
```

`HomeActionDisplayTests` requires every Home action title to be unique; "Vaccine" is.

- [ ] **Step 5: Green and commit**

Run: `make test`
Expected: `OK: B+24 tests` (an assertion was added, no test). `everyIconHasAnAssetInTheBundle` passes whenever a compiled catalog exists, so it does not prove the imageset; Task 14's screenshots do.

```bash
/usr/bin/git add Packages/BabyTracker
/usr/bin/git commit -m "feat(ui): the vaccine glyph, tracker and colour

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: The vaccine panel

**Files:**
- Create: `Sources/LoggingFeature/ImmunizationSheet.swift`
- Create: `Tests/LoggingFeatureTests/ImmunizationSheetTests.swift`
- Modify: `Sources/LoggingFeature/LogCoordinator.swift` (`LogSheet` `:8-53`, `EditDestination` `:85-131`, `LogSheetView.body` `:165-193`, `EditSheetRouter.body` `:231-254`), `Sources/LoggingFeature/Localizable.xcstrings`
- Modify tests: `Tests/LoggingFeatureTests/LogSheetRoutingTests.swift`
- Modify: `Makefile`

**Interfaces:**
- Consumes: Task 5's store API; `EntryPanel`, `PanelSection`, `DetailsSection`, `DetailFieldRow`, `EditTimeRow`, `EditChromeCopy`, `EditModel.merge` / `.trimmedToNil`, `EditModeError`, `performEdit`, `.editLifecycle`, `ErrorBannerCopy.deleteFailed(what:)`, `LoggingL10n.text` / `.format`.
- Produces: `LogSheet.immunization`; `EditDestination.immunization(UUID)`; `ImmunizationSheet(store:babyID:editing:onDone:)`; `ImmunizationSheetModel`.

Read `Sources/LoggingFeature/SupplementSheet.swift` (275 lines) and `MeasurementSheet.swift` (the date rule and its message row) in full first. The panel is the supplement panel with four differences: realm-wide names, nothing selected on open, a dose row, and Growth's date rule.

- [ ] **Step 1: Failing model tests** — `ImmunizationSheetTests.swift`

```swift
import Foundation
import Testing
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

@MainActor @Suite("Vaccine panel")
struct ImmunizationSheetTests {
    static let noon = Date(timeIntervalSince1970: 1_800_014_400)

    private func setup(bornDaysAgo: Double = 200) throws -> (BabyStore, UUID, TestDateProvider) {
        let clock = TestDateProvider(Self.noon)
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: clock.now.addingTimeInterval(-bornDaysAgo * 86_400),
                                     referenceSex: .girls, expectedDueDate: nil)
        return (store, baby.id, clock)
    }

    @Test func withNoNamesItAsksForTheFirstAndCannotSave() throws {
        let (store, baby, _) = try setup()
        let model = ImmunizationSheetModel(store: store, babyID: baby)
        #expect(model.names.isEmpty)
        #expect(model.isAddingName)
        #expect(model.selectedName == nil)
        #expect(model.dose == nil)
        #expect(!model.canSave)
    }

    @Test func nothingIsSelectedOnOpenEvenWithOneName() throws {
        let (store, baby, _) = try setup()
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: baby)
        let model = ImmunizationSheetModel(store: store, babyID: baby)
        #expect(model.names == ["Prevenar 13"])
        #expect(!model.isAddingName)
        #expect(model.selectedName == nil)
        #expect(model.dose == nil)
        #expect(!model.canSave)
    }

    @Test func theDoseFollowsTheNameUntilChangedByHand() throws {
        let (store, baby, _) = try setup()
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: baby)
        try store.logImmunization(vaccine: "MMR", dose: 2, for: baby)
        let model = ImmunizationSheetModel(store: store, babyID: baby)
        model.choose("Prevenar 13")
        #expect(model.dose == 2)
        #expect(model.canSave)
        model.choose("MMR")
        #expect(model.dose == 3)
        model.stepDose(-1)
        #expect(model.dose == 2)
        model.choose("Prevenar 13")
        #expect(model.dose == 2)          // by hand now: it no longer follows
        model.choose(nil)
        #expect(!model.canSave)
    }

    @Test func aTypedNameSetsTheDoseAndTheStepperClearsIt() throws {
        let (store, baby, _) = try setup()
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: baby)
        let model = ImmunizationSheetModel(store: store, babyID: baby)
        model.addName()
        model.type("rota")
        #expect(model.dose == 1)
        model.type("prevenar 13")
        #expect(model.dose == 2)
        model.type("")
        #expect(model.dose == nil)
        #expect(!model.canSave)
        model.type("Rotarix")
        model.stepDose(-1)
        #expect(model.dose == nil)        // below 1 is no dose
        model.stepDose(-1)
        #expect(model.dose == nil)
        model.stepDose(1)
        #expect(model.dose == 1)
        for _ in 0..<20 { model.stepDose(1) }
        #expect(model.dose == 10)
        model.useSavedNames()
        #expect(model.selectedName == nil)
    }

    @Test func savingLogsOneShotInTheFamilysSpelling() throws {
        let (store, baby, clock) = try setup()
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: baby, at: clock.now.addingTimeInterval(-60 * 86_400))
        let model = ImmunizationSheetModel(store: store, babyID: baby)
        model.addName()
        model.type("  prevenar 13 ")
        model.note = "  left thigh  "
        #expect(model.log())
        let shots = try store.immunizations(for: baby)
        #expect(shots.count == 2)
        #expect(shots[0].vaccine == "Prevenar 13")
        #expect(shots[0].dose == 2)
        #expect(shots[0].note == "left thigh")
        #expect(shots[0].givenAt == clock.now)
    }

    // Review Focus 1.
    @Test func dateRulesDisableSaveWithAMessage() throws {
        let (store, baby, clock) = try setup(bornDaysAgo: 10)
        let model = ImmunizationSheetModel(store: store, babyID: baby)
        model.addName(); model.type("MMR")
        #expect(model.dateMessage == nil)
        #expect(model.canSave)
        model.when = clock.now.addingTimeInterval(-12 * 86_400)
        #expect(model.dateMessage == ImmunizationSheetCopy.beforeBirth)
        #expect(!model.canSave)
        #expect(!model.log())
        model.when = clock.now.addingTimeInterval(3600)
        #expect(model.dateMessage == ImmunizationSheetCopy.inTheFuture)
        #expect(!model.canSave)
        #expect(try store.immunizations(for: baby).isEmpty)
    }

    @Test func aFailedSaveKeepsTheInputAndShowsAMessage() throws {
        let (store, baby, _) = try setup()
        let model = ImmunizationSheetModel(store: store, babyID: baby)
        model.addName(); model.type("MMR")
        try store.setActiveRealm(.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID()))
        #expect(!model.log())
        #expect(model.errorMessage != nil)
        #expect(model.newName == "MMR")
    }

    @Test func editingShowsTheStoredDoseAndNeverFollowsTheName() throws {
        let (store, baby, clock) = try setup()
        try store.logImmunization(vaccine: "MMR", dose: 4, for: baby)
        let shot = try store.logImmunization(vaccine: "Prevenar 13", dose: nil, for: baby, note: "fussy after")
        let model = ImmunizationSheetModel(store: store, babyID: baby, editing: shot.id)
        #expect(model.isEditing)
        #expect(model.selectedName == "Prevenar 13")
        #expect(model.dose == nil)
        #expect(model.note == "fussy after")
        model.choose("MMR")
        #expect(model.dose == nil)
        model.stepDose(1)
        model.when = clock.now.addingTimeInterval(-86_400)
        try model.save()
        #expect(shot.vaccine == "MMR")
        #expect(shot.dose == 1)
        #expect(shot.givenAt == clock.now.addingTimeInterval(-86_400))
    }

    @Test func reloadTakesTheOtherPhonesChangeOnlyIntoUntouchedFields() throws {
        let (store, baby, clock) = try setup()
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby)
        let model = ImmunizationSheetModel(store: store, babyID: baby, editing: shot.id)
        model.note = "mine"
        try store.editImmunization(shot, vaccine: "MMR", dose: 2, at: clock.now.addingTimeInterval(-3600), note: "theirs")
        model.reload()
        #expect(model.dose == 2)
        #expect(model.when == clock.now.addingTimeInterval(-3600))
        #expect(model.note == "mine")
    }

    // Review Focus 5.
    @Test func aShotDeletedElsewhereClosesThePanel() throws {
        let (store, baby, _) = try setup()
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby)
        let model = ImmunizationSheetModel(store: store, babyID: baby, editing: shot.id)
        try store.softDelete(shot)
        model.reload()
        #expect(model.isGone)
        #expect(throws: EditModeError.gone) { try model.save() }
        #expect(throws: EditModeError.gone) { try model.delete() }
        let never = ImmunizationSheetModel(store: store, babyID: baby, editing: UUID())
        #expect(never.isGone)
    }

    @Test func deleteTombstonesTheShot() throws {
        let (store, baby, _) = try setup()
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby)
        let model = ImmunizationSheetModel(store: store, babyID: baby, editing: shot.id)
        try model.delete()
        #expect(try store.immunizations(for: baby).isEmpty)
    }
}
```

`LogSheetRoutingTests.swift`, beside the measurement and supplement assertions:

```swift
        #expect(LogSheet.immunization.id == "immunization")
        #expect(LogSheet.editEntry(.immunization(id)).id == "edit-immunization-\(id)")
```

and a test:

```swift
    @Test func aShotResolvesToItsPanelAndAWrongKindToGone() throws {
        let (store, baby, _) = try fixture()
        let mira = try #require(try store.baby(baby))
        mira.birthDate = store.now.addingTimeInterval(-100 * 86_400)
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby)
        let med = try store.logSupplement(name: "Iron", for: baby)
        #expect(EditDestination.resolve(.immunization(shot.id), store: store, babyID: baby) == .immunization(shot.id))
        #expect(EditDestination.resolve(.immunization(med.id), store: store, babyID: baby) == .gone)
        #expect(EditDestination.resolve(.supplement(shot.id), store: store, babyID: baby) == .gone)
        try store.softDelete(shot)
        #expect(EditDestination.resolve(.immunization(shot.id), store: store, babyID: baby) == .gone)
    }
```

The shared `fixture()` creates the baby with `birthDate: .now` (wall clock) under a fixed clock; the two lines after it move the birth date so the shot is not pre-birth. If `Baby.birthDate` is not directly settable in tests, create the baby with `store.addBaby` and an explicit date instead.

Run: `swift test --filter 'ImmunizationSheetTests|LogSheetRoutingTests'`
Expected: FAIL to compile.

- [ ] **Step 2: The model** — top of `ImmunizationSheet.swift`

```swift
import SwiftUI
import Observation
import BabyCore
import BabyProtocol
import BabyStore
import BabyUI

public enum ImmunizationSheetCopy {
    public static let addName = LoggingL10n.text("Add vaccine")
    public static let name = LoggingL10n.text("Vaccine name")
    public static let invitation = LoggingL10n.text("Add your first vaccine")
    public static let vaccine = LoggingL10n.text("Vaccine")
    public static let choose = LoggingL10n.text("Choose vaccine")
    public static let useSaved = LoggingL10n.text("Use saved vaccines")
    public static let validation = LoggingL10n.text("Enter a name of up to 64 characters, without control characters.")
    public static let dose = LoggingL10n.text("Dose")
    public static let noDose = LoggingL10n.text("No dose")
    public static let decreaseDose = LoggingL10n.text("Decrease dose")
    public static let increaseDose = LoggingL10n.text("Increase dose")
    public static let beforeBirth = LoggingL10n.text("Choose a date on or after the baby’s birth date.")
    public static let inTheFuture = LoggingL10n.text("Choose a date and time that is not in the future.")
    public static let loadFailed = LoggingL10n.text("Couldn’t load vaccines. Close this sheet and try again.")
    public static let saveFailed = LoggingL10n.text("Couldn’t save vaccination. Your entry is still here; try again.")
}

/// One vaccination, logged or edited. The dose follows the chosen vaccine —
/// one more than this baby's highest — until the caregiver sets it by hand;
/// when editing it is the stored dose and never follows.
@MainActor @Observable
public final class ImmunizationSheetModel {
    private let store: BabyStore
    private let babyID: UUID
    private var writeContext: RealmWriteContext?
    public let editingID: UUID?
    public private(set) var names: [String] = []
    public private(set) var isAddingName = false
    public private(set) var errorMessage: String?
    /// The picker's choice; nil until the caregiver chooses.
    public private(set) var selectedName: String?
    public private(set) var newName = ""
    public private(set) var dose: Int?
    public var when: Date
    public var note = ""
    public let timeZoneID: String
    public let latestAllowedDate: Date
    public private(set) var isGone = false
    private var doseFollowsName: Bool
    private var loaded: Loaded?
    private let birthDay: LocalDate?

    private struct Loaded: Equatable {
        var name: String
        var dose: Int?
        var when: Date
        var note: String
    }

    public init(store: BabyStore, babyID: UUID, editing entryID: UUID? = nil) {
        self.store = store
        self.babyID = babyID
        editingID = entryID
        let entry = entryID.flatMap { Self.fetch($0, babyID: babyID, in: store) }
        when = entry?.givenAt ?? store.now
        timeZoneID = entry?.timeZoneID ?? TimeZone.current.identifier
        latestAllowedDate = max(store.now, entry?.givenAt ?? store.now)
        doseFollowsName = entryID == nil
        let baby = try? store.baby(babyID)
        birthDay = baby?.localBirthDate ?? baby.flatMap { try? LocalDate(displaying: $0.birthDate, in: .current) }
        do {
            writeContext = try store.captureWriteContext()
            names = try store.vaccineNames()
            if let entry {
                let loaded = Loaded(name: Self.listed(entry.vaccine, in: names), dose: entry.dose,
                                    when: entry.givenAt, note: entry.note ?? "")
                self.loaded = loaded
                selectedName = loaded.name
                dose = loaded.dose
                note = loaded.note
            } else {
                isAddingName = names.isEmpty
            }
        } catch {
            errorMessage = ImmunizationSheetCopy.loadFailed
        }
        isGone = entryID != nil && entry == nil
    }

    public var isEditing: Bool { editingID != nil }
    /// The name Save would write, before normalising.
    public var vaccine: String? { isAddingName ? newName : selectedName }
    public var hasValidName: Bool { vaccine.flatMap { try? SupplementName.normalized($0) } != nil }

    /// Growth's rule, in the entry's zone; nil when the time is fine.
    public var dateMessage: String? {
        if when > latestAllowedDate { return ImmunizationSheetCopy.inTheFuture }
        guard let birthDay, let zone = TimeZone(identifier: timeZoneID),
              let day = try? LocalDate(displaying: when, in: zone) else { return nil }
        return day < birthDay ? ImmunizationSheetCopy.beforeBirth : nil
    }

    public var canSave: Bool { writeContext != nil && !isGone && hasValidName && dateMessage == nil }

    public func addName() { isAddingName = true; follow() }
    public func useSavedNames() { isAddingName = false; follow() }
    public func choose(_ name: String?) { selectedName = name; follow() }
    public func type(_ text: String) { newName = text; follow() }

    /// − below 1 is no dose; + from no dose is 1; never above 10.
    public func stepDose(_ direction: Int) {
        doseFollowsName = false
        let next = (dose ?? 0) + direction
        dose = next < BabyStore.immunizationDoses.lowerBound ? nil : min(next, BabyStore.immunizationDoses.upperBound)
    }

    private func follow() {
        guard doseFollowsName else { return }
        dose = hasValidName ? vaccine.flatMap { try? store.nextDose(of: $0, for: babyID) } : nil
    }

    /// The view dismisses only after this single atomic write succeeds.
    public func log() -> Bool {
        guard editingID == nil, canSave, let writeContext, let vaccine else { return false }
        do {
            try store.logImmunization(vaccine: vaccine, dose: dose, for: babyID, at: when,
                                      note: EditModel.trimmedToNil(note), expectedWriteContext: writeContext)
            errorMessage = nil
            return true
        } catch {
            errorMessage = ImmunizationSheetCopy.saveFailed
            return false
        }
    }

    public func save() throws {
        guard let editingID else { throw EditModeError.invalid }
        guard let entry = Self.fetch(editingID, babyID: babyID, in: store) else {
            isGone = true
            throw EditModeError.gone
        }
        guard canSave, let writeContext, let vaccine else { throw EditModeError.invalid }
        do {
            try store.editImmunization(entry, vaccine: vaccine, dose: dose, at: when,
                                       note: EditModel.trimmedToNil(note), expectedWriteContext: writeContext)
            errorMessage = nil
        } catch {
            errorMessage = ImmunizationSheetCopy.saveFailed
            throw error
        }
        reload()
    }

    /// A tombstone. A refused delete puts the flag back.
    public func delete() throws {
        guard let editingID else { throw EditModeError.invalid }
        guard let entry = Self.fetch(editingID, babyID: babyID, in: store) else {
            isGone = true
            throw EditModeError.gone
        }
        let modifiedAt = entry.lastModifiedAt
        do {
            try store.softDelete(entry)
        } catch {
            entry.isTombstoned = false
            entry.lastModifiedAt = modifiedAt
            throw error
        }
    }

    /// Re-reads the entry after any write, this phone's or the other's.
    public func reload() {
        guard let editingID else { return }
        guard let entry = Self.fetch(editingID, babyID: babyID, in: store) else {
            isGone = true
            return
        }
        names = (try? store.vaccineNames()) ?? names
        let fresh = Loaded(name: Self.listed(entry.vaccine, in: names), dose: entry.dose,
                           when: entry.givenAt, note: entry.note ?? "")
        if let loaded {
            if !isAddingName, var name = selectedName {
                EditModel.merge(&name, loaded: loaded.name, fresh: fresh.name)
                selectedName = name
            }
            EditModel.merge(&dose, loaded: loaded.dose, fresh: fresh.dose)
            EditModel.merge(&when, loaded: loaded.when, fresh: fresh.when)
            EditModel.merge(&note, loaded: loaded.note, fresh: fresh.note)
        }
        loaded = fresh
    }

    private static func listed(_ name: String, in names: [String]) -> String {
        names.first { $0.caseInsensitiveCompare(name) == .orderedSame } ?? name
    }

    private static func fetch(_ id: UUID, babyID: UUID, in store: BabyStore) -> ImmunizationEntry? {
        guard case .immunization(let entry)? = try? store.entry(id: id, for: babyID) else { return nil }
        return entry
    }
}
```

`EditModel.merge` is generic over `Equatable` in `EditChrome.swift:286-300`; if it does not accept `Int?`, add an overload there rather than special-casing here.

- [ ] **Step 3: The view** — rest of `ImmunizationSheet.swift`

```swift
public struct ImmunizationSheet: View {
    @State private var model: ImmunizationSheetModel
    private let store: BabyStore
    private let onDone: () -> Void
    @FocusState private var nameFocused: Bool
    @Environment(\.errorReporter) private var errorReporter
    @Environment(\.dynamicTypeSize) private var typeSize
    @State private var isFinished = false
    @State private var isShowingDetails: Bool

    public init(store: BabyStore, babyID: UUID, editing entryID: UUID? = nil, onDone: @escaping () -> Void) {
        let model = ImmunizationSheetModel(store: store, babyID: babyID, editing: entryID)
        _model = State(initialValue: model)
        _isShowingDetails = State(initialValue: !model.note.isEmpty)
        self.store = store
        self.onDone = onDone
    }

    public var body: some View {
        EntryPanel(title: Tracker.vaccine.title, icon: .vaccine, tint: Palette.vaccine, canSave: model.canSave,
                   identifierPrefix: "immunization", onCancel: onDone,
                   onSave: saveOrLog, onDelete: deleteAction,
                   accessory: model.names.isEmpty ? nil : AnyView(addNameButton)) {
            PanelSection {
                if model.isAddingName {
                    if model.names.isEmpty { Text(ImmunizationSheetCopy.invitation) }
                    TextField(ImmunizationSheetCopy.name, text: Binding(get: { model.newName }, set: { model.type($0) }))
                        .accessibilityLabel(ImmunizationSheetCopy.name)
                        .accessibilityIdentifier("immunization.name")
                        .focused($nameFocused)
                    if !model.names.isEmpty {
                        Button(ImmunizationSheetCopy.useSaved) { model.useSavedNames() }
                    }
                } else {
                    // Outside a Form a menu picker shows no label: the row names it.
                    LabeledContent(ImmunizationSheetCopy.vaccine) {
                        Picker(ImmunizationSheetCopy.vaccine,
                               selection: Binding(get: { model.selectedName }, set: { model.choose($0) })) {
                            Text(ImmunizationSheetCopy.choose).tag(String?.none)
                            ForEach(model.names, id: \.self) { Text($0).tag(Optional($0)) }
                        }
                        .labelsHidden()
                        .accessibilityIdentifier("immunization.picker")
                    }
                }
                doseRow
            } footer: {
                if model.isAddingName && !model.newName.isEmpty && !model.hasValidName {
                    Text(ImmunizationSheetCopy.validation)
                }
            }
            PanelSection {
                EditTimeRow(date: $model.when, latest: model.latestAllowedDate, identifier: "immunization.time")
                    .environment(\.timeZone, TimeZone(identifier: model.timeZoneID) ?? .current)
                if let message = model.dateMessage {
                    Text(message)
                        .font(.footnote)
                        .foregroundStyle(Palette.destructive)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityIdentifier("immunization.validation.date")
                }
            }
            DetailsSection(isExpanded: $isShowingDetails, identifier: "immunization.details") {
                DetailFieldRow(EditChromeCopy.note, text: $model.note, identifier: "immunization.note")
            }
            if let error = model.errorMessage {
                PanelSection { Text(error).foregroundStyle(Palette.destructive).accessibilityIdentifier("immunization.error") }
            }
        }
        .editLifecycle(isPresented: model.isEditing, revision: store.revision, isGone: model.isGone,
                       reload: model.reload, onGone: finish)
    }

    /// "Dose  −  2  +". At the accessibility sizes the value sits above the two buttons.
    @ViewBuilder private var doseRow: some View {
        let value = Text(model.dose.map { "\($0)" } ?? "—")
            .font(.body.monospacedDigit())
            .frame(minWidth: 32)
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(ImmunizationSheetCopy.dose)
            .accessibilityValue(model.dose.map { "\($0)" } ?? ImmunizationSheetCopy.noDose)
            .accessibilityIdentifier("immunization.dose")
        if typeSize.isAccessibilitySize {
            VStack(alignment: .leading, spacing: 8) {
                HStack { Text(ImmunizationSheetCopy.dose); Spacer(minLength: 8); value }
                HStack(spacing: 8) { minus; plus }
            }
        } else {
            HStack(spacing: 8) {
                Text(ImmunizationSheetCopy.dose)
                Spacer(minLength: 8)
                minus.frame(width: 56)
                value
                plus.frame(width: 56)
            }
        }
    }

    private var minus: some View {
        stepButton(-1, systemImage: "minus", label: ImmunizationSheetCopy.decreaseDose, identifier: "immunization.dose.minus")
            .disabled(model.dose == nil)
    }

    private var plus: some View {
        stepButton(1, systemImage: "plus", label: ImmunizationSheetCopy.increaseDose, identifier: "immunization.dose.plus")
            .disabled(model.dose == BabyStore.immunizationDoses.upperBound)
    }

    private func stepButton(_ direction: Int, systemImage: String, label: String, identifier: String) -> some View {
        Button { model.stepDose(direction) } label: {
            Image(systemName: systemImage).frame(maxWidth: .infinity, minHeight: 44)
        }
        .buttonStyle(.tracker(Palette.secondaryText))
        .accessibilityLabel(label)
        .accessibilityIdentifier(identifier)
    }

    private var addNameButton: some View {
        Button { model.addName(); nameFocused = true } label: {
            Image(systemName: "plus").frame(width: 20, height: 20)
        }
        .buttonStyle(.glass)
        .controlSize(.large)
        .accessibilityLabel(ImmunizationSheetCopy.addName)
        .accessibilityIdentifier("immunization.add")
    }

    private func saveOrLog() {
        if model.isEditing { save() } else if model.log() { onDone() }
    }

    private var deleteAction: (() -> Void)? {
        guard model.isEditing else { return nil }
        return delete
    }

    private func save() {
        if performEdit(model.save, failure: nil, reporter: errorReporter) { finish() }
    }

    private func delete() {
        if performEdit(model.delete, failure: ErrorBannerCopy.deleteFailed(what: ErrorSubject.vaccination),
                       reporter: errorReporter) { finish() }
    }

    private func finish() {
        guard !isFinished else { return }
        isFinished = true
        onDone()
    }
}
```

`Palette.destructive` is what `PartFields` uses for the same kind of message; the supplement and measurement panels use `.red`. This panel uses `Palette.destructive` for both of its messages.

- [ ] **Step 4: Routing** — `LogCoordinator.swift`

- `LogSheet`: add `/// Log a vaccination.` `case immunization`; `id` → `case .immunization: "immunization"`.
- `EditDestination`: add `case immunization(UUID)`; in `resolve`'s tuple switch, before `default`:

```swift
        case (.immunization, .immunization(let shot)):
            return .immunization(shot.id)
```

- `LogSheetView.body`: `case .immunization: ImmunizationSheet(store: store, babyID: babyID, onDone: onDone)`.
- `EditSheetRouter.body`: `case .immunization(let id): ImmunizationSheet(store: store, babyID: babyID, editing: id, onDone: onDone)`.

- [ ] **Step 5: Strings** — `/tmp/imm-logging.json`

"Enter a name of up to 64 characters, without control characters.", "Choose a date on or after the baby’s birth date." and "Choose a date and time that is not in the future." already exist in the LoggingFeature catalog; confirm with `grep` and leave them out.

```json
{
  "Add vaccine": {"de": "Impfung hinzufügen", "fr": "Ajouter un vaccin", "es": "Añadir vacuna", "it": "Aggiungi vaccino", "uk": "Додати щеплення"},
  "Vaccine name": {"de": "Name des Impfstoffs", "fr": "Nom du vaccin", "es": "Nombre de la vacuna", "it": "Nome del vaccino", "uk": "Назва вакцини"},
  "Add your first vaccine": {"de": "Erste Impfung hinzufügen", "fr": "Ajoutez votre premier vaccin", "es": "Añade tu primera vacuna", "it": "Aggiungi il primo vaccino", "uk": "Додайте перше щеплення"},
  "Vaccine": {"de": "Impfstoff", "fr": "Vaccin", "es": "Vacuna", "it": "Vaccino", "uk": "Вакцина"},
  "Choose vaccine": {"de": "Impfstoff wählen", "fr": "Choisir un vaccin", "es": "Elegir vacuna", "it": "Scegli vaccino", "uk": "Оберіть вакцину"},
  "Use saved vaccines": {"de": "Gespeicherte Impfstoffe verwenden", "fr": "Utiliser les vaccins enregistrés", "es": "Usar vacunas guardadas", "it": "Usa vaccini salvati", "uk": "Обрати зі збережених"},
  "Dose": {"de": "Dosis", "fr": "Dose", "es": "Dosis", "it": "Dose", "uk": "Доза"},
  "No dose": {"de": "Keine Dosis", "fr": "Pas de dose", "es": "Sin dosis", "it": "Nessuna dose", "uk": "Без дози"},
  "Decrease dose": {"de": "Dosis verringern", "fr": "Diminuer la dose", "es": "Reducir dosis", "it": "Riduci dose", "uk": "Зменшити дозу"},
  "Increase dose": {"de": "Dosis erhöhen", "fr": "Augmenter la dose", "es": "Aumentar dosis", "it": "Aumenta dose", "uk": "Збільшити дозу"},
  "Couldn’t load vaccines. Close this sheet and try again.": {"de": "Impfungen konnten nicht geladen werden. Schließe dieses Blatt und versuche es erneut.", "fr": "Impossible de charger les vaccins. Fermez cette feuille et réessayez.", "es": "No se pudieron cargar las vacunas. Cierra esta hoja e inténtalo de nuevo.", "it": "Impossibile caricare i vaccini. Chiudi questo foglio e riprova.", "uk": "Не вдалося завантажити щеплення. Закрийте це вікно і спробуйте ще раз."},
  "Couldn’t save vaccination. Your entry is still here; try again.": {"de": "Impfung konnte nicht gespeichert werden. Dein Eintrag ist noch da; versuche es erneut.", "fr": "Impossible d’enregistrer la vaccination. Votre saisie est toujours là ; réessayez.", "es": "No se pudo guardar la vacunación. Tu entrada sigue aquí; inténtalo de nuevo.", "it": "Impossibile salvare la vaccinazione. La voce è ancora qui; riprova.", "uk": "Не вдалося зберегти щеплення. Ваш запис тут; спробуйте ще раз."}
}
```

Match the wording of the existing "Couldn’t load meds…" / "Couldn’t save med…" translations in the catalog where they differ from the above (the same sentence frame in each language).

```bash
python3 scripts/add-catalog-strings.py Packages/BabyTracker/Sources/LoggingFeature/Localizable.xcstrings /tmp/imm-logging.json
```

- [ ] **Step 6: Green and commit**

Bump `EXPECTED_TESTS` by **12** (eleven model tests + one routing test).

Run: `make test`
Expected: `OK: B+36 tests`.

```bash
/usr/bin/git add Packages/BabyTracker Makefile
/usr/bin/git commit -m "feat(logging): the vaccine panel; the dose follows the vaccine until set by hand (+12 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: Vaccine as a Home action

**Files:**
- Modify: `Sources/BabyCore/HomeLayout.swift` (`HomeActionKind` `:5-6`, comment `:43`)
- Modify: `Sources/BabyUI/HomeActionKind+Display.swift:7-40`
- Modify: `Sources/HomeFeature/HomeDock.swift` (`run` `:116-126`), `HomeView.swift` (`HomeAction` `:17-32`)
- Modify: `Sources/LoggingFeature/MoreSheet.swift` (`run` `:141-152`)
- Modify: `Apps/BabyTrackerApp/RootView.swift` (`handleHomeAction` ~`:446`, the `LogLink` switch ~`:566`)
- Modify tests: `Tests/BabyCoreTests/HomeLayoutTests.swift`, `Tests/BabyStoreTests/TrackingPreferencesTests.swift:54`, `Tests/LoggingFeatureTests/MoreSheetModelTests.swift:28,36,46,54`, `Tests/SettingsFeatureTests/SettingsViewModelTests.swift:73`
- Modify: `Makefile`

**Interfaces:**
- Consumes: `LogSheet.immunization` (Task 8), `Tracker.vaccine` (Task 7).
- Produces: `HomeActionKind.vaccine` (raw `"vaccine"`), `HomeAction.vaccine`.

- [ ] **Step 1: Update the pinned expectations and add one test**

`.vaccine` is last in every completed order. Change exactly these:

| File:line | Becomes |
|---|---|
| `HomeLayoutTests.swift:10,13,14` | `[.solids, .meds, .growth, .vaccine]` |
| `HomeLayoutTests.swift:21` | `[.bottle, .solids, .breast, .sleep, .diaper, .meds, .growth, .vaccine]` |
| `HomeLayoutTests.swift:25` | `[.solids, .breast, .bottle, .sleep, .diaper, .meds, .growth, .vaccine]` |
| `HomeLayoutTests.swift:32` | `[.sleep, .meds, .growth, .vaccine]` |
| `HomeLayoutTests.swift:40` | `overflow(feedRunning: true, …) == [.vaccine]` |
| `HomeLayoutTests.swift:41` | `[.breast, .bottle, .vaccine]` |
| `HomeLayoutTests.swift:54` | keep, and add `#expect(layout.role(at: 7) == .overflow)` |
| `TrackingPreferencesTests.swift:54` | `[.solids, .breast, .bottle, .sleep, .diaper, .meds, .growth, .vaccine]` |
| `MoreSheetModelTests.swift:28` | `[.solids, .growth, .vaccine]` |
| `MoreSheetModelTests.swift:36` | `[.breast, .bottle, .sleep, .vaccine]` |
| `MoreSheetModelTests.swift:46` | `[.breast, .bottle, .vaccine]` |
| `MoreSheetModelTests.swift:54` | `model.tiles == [.vaccine]` |
| `SettingsViewModelTests.swift:73` | `moveHomeAction(at: 7, by: 1, …)` (index 6 is no longer the end) |

Add to `HomeLayoutTests`:

```swift
    @Test func vaccineIsTheLastDefaultActionAndNeverConflictsWithATimer() {
        #expect(HomeActionKind.defaultOrder.last == .vaccine)
        #expect(HomeActionKind.allCases.count == 8)
        #expect(HomeActionKind.vaccine.rawValue == "vaccine")
        // An order stored by the version before this one reads with it appended.
        let before = ["breast", "bottle", "sleep", "diaper", "solids", "meds", "growth"]
        #expect(HomeActionKind.order(fromStored: before) == HomeActionKind.defaultOrder)
        // On the dock it stays put while a feed or a sleep runs.
        let docked = HomeLayout(order: [.vaccine, .breast, .bottle, .sleep, .diaper, .solids, .meds, .growth])
        #expect(docked.big == .vaccine)
    }
```

Use `HomeLayout`'s real initialiser label (read `HomeLayout.swift:25-54`), and add one assertion on the running-state tiles with the layout's own accessor showing `.vaccine` is kept while a feed runs — the function `HomeLayoutTests` already calls for `runningTilesDropWhatConflicts`.

Run: `swift test --filter 'HomeLayoutTests|TrackingPreferencesTests|MoreSheetModelTests|SettingsViewModelTests'`
Expected: FAIL to compile (`.vaccine`).

- [ ] **Step 2: Implement**

- `HomeLayout.swift`: `case breast, bottle, sleep, diaper, solids, meds, growth, vaccine`; comment `:43` → "Always every kind, once each."
- `HomeActionKind+Display.swift`: `icon` → `.vaccine`; `color` → `Palette.vaccine`; `title` → `Tracker.vaccine.title`.
- `HomeView.swift` `HomeAction`: add `case vaccine`.
- `HomeDock.swift` `run`: `case .vaccine: onAction(.vaccine)`.
- `MoreSheet.swift` `run`: `case .vaccine: onSwitchTo(.immunization)`.
- `RootView.swift` `handleHomeAction`: `case .vaccine: sheet = .immunization`; the `LogLink` switch: `case .vaccine: sheet = .immunization`.

No change in `HomeLayout`'s logic, `TrackingPreferences`, `HomeButtonsView`, the Quick Log widget or `LogLink`: all are generic over the enum. Confirm with `swift build` that no other exhaustive switch over `HomeActionKind` or `HomeAction` remains; give any that surfaces the arm that opens the vaccine panel and list it in the commit body.

- [ ] **Step 3: Build the app**

```bash
pgrep xcodebuild   # must print nothing
xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' build
```

Expected: `BUILD SUCCEEDED` (the app and widget targets compile the new case).

- [ ] **Step 4: Green and commit**

Bump `EXPECTED_TESTS` by **1**.

Run: `make test`
Expected: `OK: B+37 tests`.

```bash
/usr/bin/git add Packages/BabyTracker Apps Makefile
/usr/bin/git commit -m "feat(home): Vaccine is a Home action, last in the default order (+1 test)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 10: History and the PDF

**Files:**
- Create: `Tests/HistoryFeatureTests/ImmunizationHistoryTests.swift`
- Modify: `Sources/HistoryFeature/HistoryDayBuilder.swift` (item loop `:63-77`, row builders ~`:205`), `HistoryViewModel.swift` (`trackers` `:98`, `hasAnyData` `:233-239`), `HistoryReport.swift:13` (doc comment), `Sources/HistoryFeature/Localizable.xcstrings`
- Modify tests: `Tests/HistoryFeatureTests/HistoryViewModelTests.swift:583`
- Modify: `Makefile`

**Interfaces:**
- Consumes: `HistoryItem.immunization`, `EntryRef.immunization`, `Tracker.vaccine`, `store.lastImmunization(for:)`.
- Produces: History rows with `tracker == .vaccine`, `target == .entry(.immunization(id))`.

- [ ] **Step 1: Failing tests** — `ImmunizationHistoryTests.swift`

Use the store/clock helper `HistoryViewModelTests.makeStore` uses (local noon, baby born 90 days back); read `HistoryViewModelTests.swift:8-38` and `HistoryReportTests.swift:62` for the exact helpers and the report builder's call.

```swift
@MainActor @Suite("Immunizations in History")
struct ImmunizationHistoryTests {
    @Test func aShotIsARowWithItsDoseAndOpensItsPanel() throws {
        // log "Prevenar 13" dose 2 and "MMR" with no dose, an hour apart
        // load(dayCount: 2)
        // rows (newest first): text "MMR"; text "Prevenar 13 · dose 2"
        // both tracker == .vaccine, chips empty, end == nil, deleteConfirmation == false
        // targets == .entry(.immunization(id))
        // the day's totals equal the totals of the same day without the shots
    }

    @Test func theVaccineFilterShowsOnlyShots() throws {
        // a feed, a diaper and a shot today; filter = [.vaccine] -> one row, the shot
        // filter = all trackers minus .vaccine -> no vaccine row
        // HistoryViewModel.trackers contains .vaccine
    }

    @Test func aSwipeDeleteTombstonesTheShot() throws {
        // model.delete(row) -> store.immunizations(for:) is empty; reload -> day is gone
    }

    @Test func aBabyWithOnlyAnOldShotHasData() throws {
        // one shot 80 days ago, nothing else; load(dayCount: 2) -> hasAnyData is true
    }

    @Test func thePDFListsTheShotWithItsNote() throws {
        // a shot "Prevenar 13" dose 2, note "left thigh"; build the report for today
        // the entry: type == "Vaccine", details == "Prevenar 13 · dose 2", note == "left thigh"
    }
}
```

Each comment line is an assertion; write them all. `HistoryViewModelTests.swift:583`: the default filter expectation gains `.vaccine`.

Run: `swift test --filter HistoryFeatureTests`
Expected: FAIL (rows missing; `:583`).

- [ ] **Step 2: Implement**

`HistoryDayBuilder.swift`, replacing Task 5's `continue` arm:

```swift
            case .immunization(let shot):
                guard filter.contains(.vaccine), let key = day(of: shot.givenAt) else { continue }
                dated[key, default: []].append(BuiltEntry(at: shot.givenAt, row: immunizationRow(shot), note: Self.note([shot.note])))
```

and beside `medRow`:

```swift
    private func immunizationRow(_ shot: ImmunizationEntry) -> HistoryListRow {
        let text = shot.dose.map { L10n.fmt("%1$@ · dose %2$lld", shot.vaccine, $0) } ?? shot.vaccine
        return singleRow(id: shot.id, tracker: .vaccine, at: shot.givenAt, zone: shot.timeZoneID,
                         text: text, target: .immunization(shot.id))
    }
```

`HistoryViewModel.swift`: `trackers` → `[.feed, .sleep, .diaper, .supplement, .growth, .vaccine]`; `hasAnyData` also ORs `(try store.lastImmunization(for: babyID)) != nil`, in the form the existing clauses use.

`HistoryReport.swift:13`: add "Vaccine" to the doc comment's list of types. No other PDF change: the report takes its rows from the day builder.

- [ ] **Step 3: String** — `/tmp/imm-history.json`

```json
{
  "%1$@ · dose %2$lld": {"de": "%1$@ · Dosis %2$lld", "fr": "%1$@ · dose %2$lld", "es": "%1$@ · dosis %2$lld", "it": "%1$@ · dose %2$lld", "uk": "%1$@ · доза %2$lld"}
}
```

```bash
python3 scripts/add-catalog-strings.py Packages/BabyTracker/Sources/HistoryFeature/Localizable.xcstrings /tmp/imm-history.json
```

- [ ] **Step 4: Green and commit**

Bump `EXPECTED_TESTS` by **5**.

Run: `make test`
Expected: `OK: B+42 tests`.

```bash
/usr/bin/git add Packages/BabyTracker Makefile
/usr/bin/git commit -m "feat(history): vaccinations as rows, a filter and PDF lines (+5 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 11: The Insights card and the Immunizations screen

**Files:**
- Create: `Sources/InsightsFeature/ImmunizationsPresentation.swift`, `ImmunizationsViewModel.swift`, `ImmunizationsCard.swift`, `ImmunizationsScreen.swift`
- Create: `Tests/InsightsFeatureTests/ImmunizationsTests.swift`
- Modify: `Sources/InsightsFeature/InsightsViewModel.swift` (property beside `growth` `:14`, `init` `:27`, `load()` `:36`, the error path `:80-85`), `InsightsView.swift` (`init` `:27-35`, the card stack `:49-70`), `Sources/InsightsFeature/Localizable.xcstrings`
- Modify: `Apps/BabyTrackerApp/RootView.swift` (the Insights tab ~`:329-336`)
- Modify: `Makefile`

**Interfaces:**
- Consumes: `store.immunizations(for:)`, `store.baby(_:)`, `Baby.localBirthDate`, `LocalDate`, `EntryRef.immunization`, `TrackerIcon`, `Palette`, InsightsFeature's `L10n.tr` / `L10n.fmt`.
- Produces:

```swift
struct ImmunizationShot: Equatable, Sendable { let id: UUID; let vaccine: String; let dose: Int?; let givenAt: Date; let timeZoneID: String }
struct ImmunizationsPresentation: Equatable {
    init(shots: [ImmunizationShot], birthDay: LocalDate?, locale: Locale = .current)
    struct Row: Equatable, Identifiable { let id: UUID; let text: String }
    struct Section: Equatable, Identifiable { var id: String { vaccine }; let vaccine: String; let rows: [Row] }
    var count: Int
    var isEmpty: Bool
    var latestText: String?        // "Prevenar 13 · dose 2 · Sep 3"
    var sections: [Section]
}
@MainActor @Observable public final class ImmunizationsViewModel {
    init(store: BabyStore, babyID: UUID)
    private(set) var presentation: ImmunizationsPresentation
    func load() throws
}
InsightsView(…, onEditEntry: @escaping (EntryRef) -> Void = { _ in })
```

- [ ] **Step 1: Failing tests** — `ImmunizationsTests.swift`

```swift
import Foundation
import Testing
import BabyCore
@testable import BabyStore
@testable import InsightsFeature

@MainActor @Suite("Immunizations card and screen")
struct ImmunizationsTests {
    static let locale = Locale(identifier: "en_US")
    static let zone = "UTC"
    /// 2026-09-03 12:00 UTC.
    static let september3 = Date(timeIntervalSince1970: 1_788_436_800)
    static func daysBefore(_ days: Double, _ date: Date = september3) -> Date { date.addingTimeInterval(-days * 86_400) }
    static func birth(daysBefore days: Double) throws -> LocalDate {
        try LocalDate(displaying: daysBefore(days), in: TimeZone(identifier: zone)!)
    }
    static func shot(_ vaccine: String, _ dose: Int?, _ givenAt: Date) -> ImmunizationShot {
        ImmunizationShot(id: UUID(), vaccine: vaccine, dose: dose, givenAt: givenAt, timeZoneID: zone)
    }

    @Test func noShotsIsEmpty() throws {
        let p = ImmunizationsPresentation(shots: [], birthDay: try Self.birth(daysBefore: 120), locale: Self.locale)
        #expect(p.isEmpty)
        #expect(p.count == 0)
        #expect(p.latestText == nil)
        #expect(p.sections.isEmpty)
    }

    @Test func theCardNamesTheLatestShot() throws {
        let p = ImmunizationsPresentation(shots: [
            Self.shot("Prevenar 13", 2, Self.september3),
            Self.shot("Infanrix hexa", 1, Self.daysBefore(60)),
        ], birthDay: try Self.birth(daysBefore: 120), locale: Self.locale)
        #expect(p.count == 2)
        #expect(p.latestText == "Prevenar 13 · dose 2 · Sep 3")
        let noDose = ImmunizationsPresentation(shots: [Self.shot("MMR", nil, Self.september3)], birthDay: nil, locale: Self.locale)
        #expect(noDose.latestText == "MMR · Sep 3")
    }

    @Test func sectionsAreByVaccineNewestFirstRowsOldestFirst() throws {
        let p = ImmunizationsPresentation(shots: [
            Self.shot("Prevenar 13", 2, Self.september3),
            Self.shot("Infanrix hexa", 2, Self.daysBefore(1)),
            Self.shot("Infanrix hexa", 1, Self.daysBefore(61)),
            Self.shot("prevenar 13", 1, Self.daysBefore(62)),
        ], birthDay: try Self.birth(daysBefore: 122), locale: Self.locale)
        #expect(p.sections.map(\.vaccine) == ["Prevenar 13", "Infanrix hexa"])
        #expect(p.sections[0].rows.map(\.text) == ["Dose 1 · Jul 3, 2026 · 2mo", "Dose 2 · Sep 3, 2026 · 4mo"])
        #expect(p.sections[1].rows.count == 2)
    }

    @Test(arguments: [(10.0, "10d"), (20.0, "2w"), (122.0, "4mo"), (800.0, "2y")])
    func ageUsesGrowthsShortForm(_ days: Double, _ age: String) throws {
        let p = ImmunizationsPresentation(shots: [Self.shot("MMR", nil, Self.september3)],
                                          birthDay: try Self.birth(daysBefore: days), locale: Self.locale)
        #expect(p.sections[0].rows[0].text == "Sep 3, 2026 · \(age)")
    }

    @Test func aRowWithoutABirthDayOrBeforeItShowsNoAge() throws {
        let unknown = ImmunizationsPresentation(shots: [Self.shot("MMR", 1, Self.september3)], birthDay: nil, locale: Self.locale)
        #expect(unknown.sections[0].rows[0].text == "Dose 1 · Sep 3, 2026")
        let before = ImmunizationsPresentation(shots: [Self.shot("MMR", 1, Self.september3)],
                                               birthDay: try Self.birth(daysBefore: -5), locale: Self.locale)
        #expect(before.sections[0].rows[0].text == "Dose 1 · Sep 3, 2026")
        let birthday = ImmunizationsPresentation(shots: [Self.shot("Hep B", 1, Self.september3)],
                                                 birthDay: try Self.birth(daysBefore: 0), locale: Self.locale)
        #expect(birthday.sections[0].rows[0].text == "Dose 1 · Sep 3, 2026 · 0d")
    }

    @Test func theModelReadsTheBabysShotsAndFollowsTheStore() throws {
        let clock = TestDateProvider(Self.september3)
        let store = try BabyStore.inMemory(dateProvider: clock)
        let mira = try store.addBaby(name: "Mira", birthDate: Self.daysBefore(122), referenceSex: .girls, expectedDueDate: nil)
        let noah = try store.addBaby(name: "Noah", birthDate: Self.daysBefore(400), referenceSex: .boys, expectedDueDate: nil)
        try store.logImmunization(vaccine: "MMR", dose: 1, for: noah.id)
        let model = ImmunizationsViewModel(store: store, babyID: mira.id)
        try model.load()
        #expect(model.presentation.isEmpty)
        let shot = try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: mira.id)
        try model.load()
        #expect(model.presentation.count == 1)
        #expect(model.presentation.sections[0].rows[0].id == shot.id)
    }

    // Review Focus 5.
    @Test func aDeletedShotLeavesTheScreen() throws {
        let clock = TestDateProvider(Self.september3)
        let store = try BabyStore.inMemory(dateProvider: clock)
        let mira = try store.addBaby(name: "Mira", birthDate: Self.daysBefore(122), referenceSex: .girls, expectedDueDate: nil)
        let shot = try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: mira.id)
        let insights = InsightsViewModel(store: store, babyID: mira.id)
        try insights.load()
        #expect(insights.immunizations.presentation.count == 1)
        try store.softDelete(shot)
        try insights.load()
        #expect(insights.immunizations.presentation.isEmpty)
    }
}
```

The four age strings are what `DateComponentsFormatter` with `.abbreviated` emits in `en_US`; Growth's tests already pin `10d` and `2w`. If the month or year form comes out different on the build machine's OS (for example `4 mo`), pin what the formatter emits and use that same string in the spec's example; do not hand-build the unit.

Run: `swift test --filter InsightsFeatureTests.ImmunizationsTests`
Expected: FAIL to compile.

- [ ] **Step 2: Presentation** — `ImmunizationsPresentation.swift`

```swift
import Foundation
import BabyCore

/// One vaccination as the card and the screen read it.
struct ImmunizationShot: Equatable, Sendable {
    let id: UUID
    let vaccine: String
    let dose: Int?
    let givenAt: Date
    let timeZoneID: String
}

/// The Immunizations card and screen, as text. Pure: the locale is an input,
/// so tests pin it.
struct ImmunizationsPresentation: Equatable {
    struct Row: Equatable, Identifiable {
        let id: UUID
        let text: String
    }
    struct Section: Equatable, Identifiable {
        var id: String { vaccine }
        let vaccine: String
        let rows: [Row]
    }

    let count: Int
    let latestText: String?
    let sections: [Section]
    var isEmpty: Bool { count == 0 }

    static let empty = ImmunizationsPresentation(shots: [], birthDay: nil)

    init(shots: [ImmunizationShot], birthDay: LocalDate?, locale: Locale = .current) {
        let newestFirst = shots.sorted { $0.givenAt > $1.givenAt }
        count = newestFirst.count
        latestText = newestFirst.first.map { shot in
            let date = shot.givenAt.formatted(Self.style(shot, locale).day().month(.abbreviated))
            let name = shot.dose.map { L10n.fmt("%1$@ · dose %2$lld", shot.vaccine, $0) } ?? shot.vaccine
            return "\(name) · \(date)"
        }
        // One section per vaccine, in the order of its newest shot; the heading
        // is the spelling of its earliest shot, as the picker shows it.
        var order: [String] = []
        var grouped: [String: [ImmunizationShot]] = [:]
        for shot in newestFirst {
            let key = Self.key(shot.vaccine)
            if grouped[key] == nil { order.append(key) }
            grouped[key, default: []].append(shot)
        }
        sections = order.map { key in
            let oldestFirst = Array((grouped[key] ?? []).reversed())
            return Section(vaccine: oldestFirst.first?.vaccine ?? "", rows: oldestFirst.map { shot in
                Row(id: shot.id, text: Self.rowText(shot, birthDay: birthDay, locale: locale))
            })
        }
    }

    private static func key(_ name: String) -> String {
        name.folding(options: [.caseInsensitive], locale: Locale(identifier: "en_US_POSIX"))
    }

    private static func style(_ shot: ImmunizationShot, _ locale: Locale) -> Date.FormatStyle {
        Date.FormatStyle(locale: locale, timeZone: TimeZone(identifier: shot.timeZoneID) ?? .current)
    }

    private static func rowText(_ shot: ImmunizationShot, birthDay: LocalDate?, locale: Locale) -> String {
        var parts: [String] = []
        if let dose = shot.dose { parts.append(L10n.fmt("Dose %lld", dose)) }
        parts.append(shot.givenAt.formatted(style(shot, locale).day().month(.abbreviated).year()))
        if let age = age(of: shot, birthDay: birthDay, locale: locale) { parts.append(age) }
        return parts.joined(separator: " · ")
    }

    /// Growth's short age: days under two weeks, weeks under thirteen, months
    /// under two years, then years. Nil before the birth day.
    private static func age(of shot: ImmunizationShot, birthDay: LocalDate?, locale: Locale) -> String? {
        guard let birthDay, let zone = TimeZone(identifier: shot.timeZoneID),
              let givenDay = try? LocalDate(displaying: shot.givenAt, in: zone), givenDay >= birthDay else { return nil }
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: "UTC")!
        calendar.locale = locale
        guard let from = calendar.date(from: birthDay.components), let to = calendar.date(from: givenDay.components) else { return nil }
        let days = calendar.dateComponents([.day], from: from, to: to).day ?? 0
        let months = calendar.dateComponents([.month], from: from, to: to).month ?? 0
        let formatter = DateComponentsFormatter()
        formatter.unitsStyle = .abbreviated
        formatter.maximumUnitCount = 1
        formatter.calendar = calendar
        formatter.allowedUnits = days < 14 ? [.day] : days < 91 ? [.weekOfMonth] : months < 24 ? [.month] : [.year]
        return formatter.string(from: from, to: to)
    }
}
```

`birthDay.components` stands for `LocalDate`'s year/month/day as `DateComponents`; read `Sources/BabyCore/LocalDate.swift` and use the accessor it has (or build `DateComponents(year:month:day:)` from its fields). The card and screen reuse the supplement name key's folding; it is three lines and InsightsFeature cannot reach BabyStore's internal helper.

- [ ] **Step 3: View model and Insights wiring**

`ImmunizationsViewModel.swift`:

```swift
import Foundation
import Observation
import BabyCore
import BabyStore

/// The baby's vaccinations for the Insights card and the screen it opens.
/// All time: it ignores the range picker.
@MainActor @Observable
public final class ImmunizationsViewModel {
    private let store: BabyStore
    private let babyID: UUID
    private(set) var presentation = ImmunizationsPresentation.empty

    public init(store: BabyStore, babyID: UUID) {
        self.store = store
        self.babyID = babyID
    }

    public func load() throws {
        do {
            let baby = try store.baby(babyID)
            let birthDay = baby?.localBirthDate ?? baby.flatMap { try? LocalDate(displaying: $0.birthDate, in: .current) }
            let shots = try store.immunizations(for: babyID).map {
                ImmunizationShot(id: $0.id, vaccine: $0.vaccine, dose: $0.dose, givenAt: $0.givenAt, timeZoneID: $0.timeZoneID)
            }
            presentation = ImmunizationsPresentation(shots: shots, birthDay: birthDay)
        } catch {
            // A realm switch or failed read must not leave another family's shots on screen.
            presentation = .empty
            throw error
        }
    }
}
```

`InsightsViewModel.swift`: `public let immunizations: ImmunizationsViewModel` beside `growth`; create it in `init`; in `load()` call `try immunizations.load()` right after `try growth.load()`. The existing error path (`:80-85`) resets and rethrows; `ImmunizationsViewModel.load` clears itself on its own failure, and if a later step of `load()` throws, clear it there too with the reset the path applies to its other outputs.

`InsightsView.swift`: add the parameter `onEditEntry: @escaping (EntryRef) -> Void = { _ in }` after `onOpenAllCheckIns`, store it, and after `GrowthSectionView(…)` in the stack add:

```swift
                    ImmunizationsCard(model: model.immunizations, onEdit: onEditEntry)
```

`RootView.swift`, the Insights tab: add `onEditEntry: { ref in sheet = .editEntry(ref) }`. The fitted log sheet is attached to the `TabView`, so it presents over the pushed screen; after Save or Delete the store revision bumps, `InsightsView` reloads its model, and the screen, which reads the same `@Observable` model, follows.

- [ ] **Step 4: Card and screen**

`ImmunizationsCard.swift` — the Growth card's shell (`GrowthSectionView.swift:39-109`) and its "All check-ins" row styling:

```swift
import SwiftUI
import BabyStore
import BabyUI

enum ImmunizationsCopy {
    static let title = L10n.tr("Immunizations")
    static let empty = L10n.tr("No vaccinations logged yet")
    static let open = L10n.tr("Open immunizations")
}

struct ImmunizationsCard: View {
    let model: ImmunizationsViewModel
    let onEdit: (EntryRef) -> Void

    var body: some View {
        let presentation = model.presentation
        VStack(alignment: .leading, spacing: 10) {
            HStack(alignment: .firstTextBaseline) {
                TrackerIcon(.vaccine).foregroundStyle(Palette.vaccine).accessibilityHidden(true)
                Text(ImmunizationsCopy.title).font(.headline)
                Spacer(minLength: 8)
            }
            if let latest = presentation.latestText {
                Text(latest).font(.footnote).foregroundStyle(Palette.secondaryText)
                    .accessibilityIdentifier("insights.immunizations.latest")
                NavigationLink {
                    ImmunizationsScreen(model: model, onEdit: onEdit)
                } label: {
                    HStack {
                        Text(ImmunizationsCopy.open).foregroundStyle(Palette.secondaryText)
                        Spacer(minLength: 8)
                        Text("\(presentation.count)").foregroundStyle(Palette.secondaryText)
                        Image(systemName: "chevron.right").font(.footnote.weight(.semibold)).foregroundStyle(Palette.secondaryText)
                    }
                    .font(.footnote).frame(minHeight: 44).contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityIdentifier("insights.immunizations.open")
            } else {
                Text(ImmunizationsCopy.empty).font(.footnote).foregroundStyle(Palette.secondaryText)
                    .accessibilityIdentifier("insights.immunizations.empty")
            }
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.white.opacity(0.055), in: .rect(cornerRadius: 18))
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("insights.immunizations")
    }
}
```

Match how the other Insights cards title themselves: `InsightSectionCard` shows a title; the Growth card shows only its icon. Use whichever the two neighbouring cards on screen share; if they differ, show the icon and the title as above.

`ImmunizationsScreen.swift` — pushed inside `InsightsView`'s `NavigationStack`, the way `GrowthReferenceInfoView` is (`GrowthReferenceInfoView.swift:9-46`):

```swift
import SwiftUI
import BabyStore
import BabyUI

struct ImmunizationsScreen: View {
    let model: ImmunizationsViewModel
    let onEdit: (EntryRef) -> Void

    var body: some View {
        let presentation = model.presentation
        Group {
            if presentation.isEmpty {
                Text(ImmunizationsCopy.empty)
                    .font(.footnote).foregroundStyle(Palette.secondaryText)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                    .accessibilityIdentifier("immunizations.empty")
            } else {
                List {
                    ForEach(presentation.sections) { section in
                        Section {
                            ForEach(section.rows) { row in
                                Button { onEdit(.immunization(row.id)) } label: {
                                    Text(row.text)
                                        .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
                                        .contentShape(Rectangle())
                                }
                                .buttonStyle(.plain)
                                .accessibilityIdentifier("immunizations.row.\(row.id.uuidString)")
                            }
                        } header: {
                            Text(section.vaccine)
                        }
                    }
                }
                .listStyle(.plain)
                .scrollContentBackground(.hidden)
            }
        }
        .background(Palette.ground.ignoresSafeArea())
        .foregroundStyle(Palette.primaryText)
        .navigationTitle(ImmunizationsCopy.title)
        #if os(iOS)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar(.visible, for: .navigationBar)
        #endif
        .accessibilityIdentifier("immunizations.screen")
    }
}
```

The root of Insights hides its navigation bar; the pushed screen sets a title and makes the bar visible, as the Growth reference screen's bar and back button show today. Task 14's screenshot confirms the back button.

- [ ] **Step 5: Strings** — `/tmp/imm-insights.json`

```json
{
  "Immunizations": {"de": "Impfungen", "fr": "Vaccinations", "es": "Vacunaciones", "it": "Vaccinazioni", "uk": "Щеплення"},
  "No vaccinations logged yet": {"de": "Noch keine Impfungen erfasst", "fr": "Aucune vaccination enregistrée", "es": "Aún no hay vacunaciones registradas", "it": "Nessuna vaccinazione registrata", "uk": "Щеплень ще не записано"},
  "Open immunizations": {"de": "Impfungen öffnen", "fr": "Ouvrir les vaccinations", "es": "Abrir vacunaciones", "it": "Apri vaccinazioni", "uk": "Відкрити щеплення"},
  "%1$@ · dose %2$lld": {"de": "%1$@ · Dosis %2$lld", "fr": "%1$@ · dose %2$lld", "es": "%1$@ · dosis %2$lld", "it": "%1$@ · dose %2$lld", "uk": "%1$@ · доза %2$lld"},
  "Dose %lld": {"de": "Dosis %lld", "fr": "Dose %lld", "es": "Dosis %lld", "it": "Dose %lld", "uk": "Доза %lld"}
}
```

```bash
python3 scripts/add-catalog-strings.py Packages/BabyTracker/Sources/InsightsFeature/Localizable.xcstrings /tmp/imm-insights.json
```

- [ ] **Step 6: Build, green, commit**

```bash
pgrep xcodebuild   # must print nothing
xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' build
```

Bump `EXPECTED_TESTS` by **10** (six plain tests + four arguments).

Run: `make test`
Expected: `OK: B+52 tests`.

```bash
/usr/bin/git add Packages/BabyTracker Apps Makefile
/usr/bin/git commit -m "feat(insights): an Immunizations card and the screen it opens, by vaccine with the age at each shot (+10 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 12: Ask

**Files:**
- Modify: `Sources/AskFeature/AskPlan.swift:33-41`, `AskSessionBuilder.swift:103`, `AskCareLog.swift` (`log` `:70-103`, `lastEntries` ~`:134-145`, `entrySummary` ~`:199-202`, `line(for:)` `:207-219`, `kind(of:)` `:241-248`, `source(of:)` `:250-258`, `CareData`/`summary` `:355-399`), `AskViewModel.swift:80-87`, `Sources/AskFeature/Localizable.xcstrings`
- Modify tests: `Tests/AskFeatureTests/AskCareLogTests.swift`
- Modify: `Makefile`

**Interfaces:**
- Consumes: `HistoryItem.immunization`, `store.immunizations(for:)`, `store.baby(_:)`.
- Produces: model-facing kind `vaccine`; source kind `immunization`.

Read `AskCareLog.swift` in full first. Ask has no tool definitions: a planning call picks up to three reads by guided generation, and `AskCareLog` answers them as text.

- [ ] **Step 1: Failing tests** — in `AskCareLogTests.swift`, using the suite's existing store/log helpers

```swift
    @Test func aVaccineReadListsEveryShotWhateverTheRange() throws {
        // baby born 300 days ago; shots: "Prevenar 13" dose 1 at -200d, dose 2 at -140d (note "fever that evening"),
        // "MMR" no dose at -3d; plus a feed today
        // log(range: "today", date: "", kind: "vaccine"):
        //   contains "Prevenar 13 · dose 1", "Prevenar 13 · dose 2 · note: \"fever that evening\"", and "MMR"
        //   contains " · vaccine · " on each of the three lines; no feed line
        //   each line carries the shot's date, not only a clock time
        //   record's shown sources are the three shots, kind "immunization"
    }

    @Test func theKindListNamesVaccine() throws {
        // log(range: "today", date: "", kind: "pump") == "Use kind feed, sleep, diaper, med, vaccine or all."
    }

    @Test func latestIncludesTheLastVaccine() throws {
        // lastEntries(): five lines; the last is "vaccine: none logged in the past year" with no shot,
        // and names "MMR" once one is logged
    }

    @Test func totalsCountVaccinesOnlyWhenThereAreSome() throws {
        // a day with feeds only: the summary string is exactly what it is today (ends ", 0 meds")
        // the same day with one "MMR" shot: it ends ", 0 meds, 1 vaccine (MMR)"
    }

    @Test func anAllReadIncludesAShotInItsWindow() throws {
        // a shot today; log(range: "today", date: "", kind: "all") lists it and the entry summary counts it
    }
```

Update the two existing assertions that pin the lists: `:74` (the correction string) and `:127-131` (`lines.count == 4` → `5`, and the new last line). The existing exact totals strings (`:93-96`, `:112`, `:164`, `:205-206`) must **not** change.

Run: `swift test --filter AskFeatureTests`
Expected: FAIL.

- [ ] **Step 2: Implement**

`AskPlan.swift`:
- the `tool` guide: "…latest for when the last feed, sleep, diaper, med or vaccine was";
- the `kind` guide: description `"Which entries, for entries reads; vaccine lists every vaccination the baby has had"`, `.anyOf(["all", "feed", "sleep", "diaper", "med", "vaccine"])`.

`AskSessionBuilder.swift:103`: "…and latest for when the last feed, sleep, diaper, med or vaccine was. For how many doses of a vaccine, read entries with kind vaccine."

`AskCareLog.swift`:
- `log`: the whitelist and correction become `["feed", "sleep", "diaper", "med", "vaccine", "all"]` and `"Use kind feed, sleep, diaper, med, vaccine or all."`. After the window is resolved:

```swift
        // Shots are months apart and the ranges stop at 30 days, so a vaccine
        // read covers the baby's whole life whatever range the model named.
        if kind == "vaccine" {
            let birth = (try? store.baby(babyID))??.birthDate ?? now.addingTimeInterval(-5 * 365 * 86_400)
            window = AskWindow(start: min(birth, now), end: now)
        }
```

  Declare `window` as `var`, use `AskWindow`'s real initialiser, and flatten the optional the way the file's other `store.baby` reads do. The rest of `log` is unchanged: it fetches `store.entries(for:from:to:)` over the window, filters by kind, shows up to `logLines` entries and records sources.
- `line(for:)`:

```swift
        case .immunization(let entry):
            let dose = entry.dose.map { " · dose \($0)" } ?? ""
            return "\(text.day(entry.givenAt)) \(text.time(entry.givenAt)) · vaccine · \(entry.vaccine)\(dose)\(Self.note(entry.note))"
```

  If `text.time` already includes the date, drop the `text.day` prefix; the test requires the date to be present exactly once.
- `kind(of:)` → `"vaccine"`; `source(of:)` → kind `"immunization"`.
- `lastEntries`: the kind list becomes `["feed", "sleep", "diaper", "med", "vaccine"]`. Its 365-day look-back stays.
- `entrySummary`: after the meds part, `let shots = items.filter { Self.kind(of: $0) == "vaccine" }` and, when not empty, `parts.append("vaccines \(shots.count), newest \(text.time(newest.startedAt))")` in the meds part's form.
- `CareData` / `summary`: collect `vaccines: [String]` beside `meds` (`case .immunization(let entry): if inside(entry.givenAt) { vaccines.append(entry.vaccine) }`), and after the meds part append **only when the count is above zero**: `Self.count(vaccines.count, "vaccine")` plus the distinct names in parentheses, as meds do.

`AskViewModel.swift` `sourceLabel`: `case "immunization": String(localized: "Vaccine", bundle: .module)`.

- [ ] **Step 3: String** — `/tmp/imm-ask.json`

```json
{
  "Vaccine": {"de": "Impfung", "fr": "Vaccin", "es": "Vacuna", "it": "Vaccino", "uk": "Щеплення"}
}
```

```bash
python3 scripts/add-catalog-strings.py Packages/BabyTracker/Sources/AskFeature/Localizable.xcstrings /tmp/imm-ask.json
```

- [ ] **Step 4: Green and commit**

Bump `EXPECTED_TESTS` by **5**.

Run: `make test`
Expected: `OK: B+57 tests`.

```bash
/usr/bin/git add Packages/BabyTracker Makefile
/usr/bin/git commit -m "feat(ask): vaccine joins Ask's reads; a vaccine read covers the baby's whole life (+5 tests)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 13: Two phones against the real server

**Files:**
- Create: `Tests/FamilySyncClientE2ETests/ImmunizationSyncE2ETests.swift`
- Modify: `Tests/FamilySyncClientE2ETests/OwnerReinstallE2ETests.swift` (`FamilyLedger` `:312-370`, `kinds` `:362`)
- Modify: `Server/scripts/protocol-e2e.sh` (proof grep ~`:110`; ledger SQL `:121-134`), `Server/scripts/protocol_e2e_test.go` (~`:90,151,372`)
- Modify: `Makefile`

**Interfaces:**
- Consumes: everything above; the E2E harness (`PinnedSession.swift`, `Phone` helpers in `TwoCaregiverSyncE2ETests.swift:1003-1100`).

- [ ] **Step 1: The scenario**

Read `TwoCaregiverSyncE2ETests.swift` for how a two-phone family is set up (owner bootstrap, invite, join), and how a test returns early without `E2EEnvironment.current()`. Reuse its `Phone` helpers; if they are `private` to that file, lift them to `internal` rather than copying.

```swift
@Suite("Immunization sync E2E")
struct ImmunizationSyncE2ETests {
    @Test @MainActor func aShotSyncsBothWaysAndSurvivesASnapshot() async throws {
        guard let environment = E2EEnvironment.current() else { return }
        // 1. owner + joiner share a family with one baby born 150 days ago
        // 2. owner logs "Prevenar 13" dose 1, 60 days ago, note "left thigh"; owner.sync(.localWrite); joiner.sync()
        //    joiner has the shot: vaccine, dose 1, givenAt within 1 ms, zone, note
        //    joiner.store.vaccineNames() == ["Prevenar 13"]; joiner.store.nextDose(of: "Prevenar 13", for: baby) == 2
        // 3. joiner logs " prevenar 13 " dose 2 today -> stored as "Prevenar 13"; sync both
        //    owner has 2 shots; owner.nextDose == 3
        // 4. joiner edits the owner's shot: dose 1 -> nil, note -> nil, given 61 days ago; sync both
        //    the edit's outbox row is acknowledged; owner sees dose nil, note nil, the new time
        // 5. owner deletes the joiner's shot; sync both; joiner has 1 shot; vaccineNames still ["Prevenar 13"]
        // 6. a fresh snapshot on the joiner holds exactly the 1 live shot with step 4's fields
        // 7. print("immunization proof: step 6: create, reuse, edit to no dose, delete and fresh snapshot agree on both phones")
    }
}
```

Write every numbered step with its assertions.

`OwnerReinstallE2ETests.swift` `FamilyLedger`: add `"immunizations"` to `kinds` after `"supplements"` and the matching model enumeration to the digest builder and `liveIDs`, in the supplements' form.

- [ ] **Step 2: The script**

`protocol-e2e.sh`:
- after the two existing proof greps add `grep -q '^immunization proof: step 6:' "$swift_log"` with the same failure handling;
- the owner-reinstall ledger SQL (`:121-134`) gains a `kind=9` count in the position that matches `immunizations` in the Swift `kinds` list. The two sides are compared as strings, so the order must match exactly.

`protocol_e2e_test.go`: extend the pinned strings to require `"kind=9 AND"` and the new proof line, in the form the existing pins use.

Run: `cd Server && go test ./scripts/ -count=1`
Expected: PASS.

- [ ] **Step 3: Run the end-to-end gate**

Bump `EXPECTED_TESTS` by **1** (the E2E test counts in `make test`, where it returns early).

Run: `make test` → `OK: B+58 tests`.
Run: `make server-protocol-e2e`
Expected: exits 0; the log contains `two-caregiver proof: step 24:`, `owner-reinstall proof: step 6:` and `immunization proof: step 6:`. This builds the server image from the working tree, migrates a fresh database to 33 and runs the Swift suite against it; it passes the `$schema == 33` gate in `disposable-sync.sh` only if Task 2 changed the literal. A silent non-zero exit with no failing test is that gate.

- [ ] **Step 4: Commit**

```bash
/usr/bin/git add Packages/BabyTracker Server/scripts Makefile
/usr/bin/git commit -m "test(e2e): a vaccination syncs both ways and survives a fresh snapshot (+1 test)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 14: UI tests, screenshots, verification notes

**Files:**
- Modify: `Apps/BabyTrackerApp/UITestSupport.swift` (a `-uiTestSeedVaccines` fixture beside `-uiTestSeedMeds` ~`:249`), `Apps/BabyTrackerApp/RootView.swift` (`openRequestedLogSheetIfNeeded` ~`:598-628`: `-uiTestOpenVaccine`), `UITests/BabyTrackerUITests/UITestHarness.swift` (`Launch` fields `seedVaccines`, `openVaccine`)
- Modify: `UITests/BabyTrackerUITests/LoggingFlowTests.swift`
- Create: `docs/verification/2026-10-11-immunizations/README.md` and PNGs
- Modify: `TASKS.md`

- [ ] **Step 1: Seed and launch hooks** (all `#if DEBUG`, in the existing files)

`-uiTestSeedVaccines`: move the seeded baby's birth date 150 days back (as `seedHistorySessions` moves it), then log `Infanrix hexa` dose 1 at −90 d, `Prevenar 13` dose 1 at −90 d, `Infanrix hexa` dose 2 at −30 d, `Prevenar 13` dose 2 at −30 d with note "Fussy that evening". `-uiTestOpenVaccine` sets `sheet = .immunization` beside the other `-uiTestOpen…` flags. Add the two `Launch` fields to the harness; key/value arguments go before bare flags, as the comment at `UITestHarness.swift:51-62` requires.

- [ ] **Step 2: UI tests** — in `LoggingFlowTests.swift`

```swift
    /// More → Vaccine → a new name saves; the History row reads it.
    func testAVaccineIsLoggedFromMore() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true))
        defer { app.terminate() }
        XCTAssertTrue(app.tap(app.revealOnHome(app.buttons["home.more"]), until: app.buttons["more.vaccine"]))
        XCTAssertTrue(app.tap(app.buttons["more.vaccine"], until: app.textFields["immunization.name"]))
        let save = app.buttons["immunization.save"]
        XCTAssertFalse(save.isEnabled)
        app.textFields["immunization.name"].tap()
        app.textFields["immunization.name"].typeText("Rotarix")
        XCTAssertEqual(app.staticTexts["immunization.dose"].value as? String, "1")
        XCTAssertTrue(save.isEnabled)
        save.tap()
        XCTAssertTrue(app.buttons["home.more"].waitForExistence(timeout: 10))
        app.goToTab("History")
        XCTAssertFalse(app.historyRows(startingWith: "Vaccine, Rotarix · dose 1").isEmpty)
    }

    /// Picking a saved vaccine fills in the next dose; nothing is selected on open.
    func testPickingASavedVaccineOffersTheNextDose() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, seedVaccines: true, openVaccine: true))
        defer { app.terminate() }
        let picker = app.buttons["immunization.picker"]
        XCTAssertTrue(picker.waitForExistence(timeout: 10))
        XCTAssertFalse(app.buttons["immunization.save"].isEnabled)
        picker.tap()
        app.buttons["Prevenar 13"].tap()
        XCTAssertEqual(app.staticTexts["immunization.dose"].value as? String, "3")
        XCTAssertTrue(app.buttons["immunization.save"].isEnabled)
        app.buttons["immunization.save"].tap()
        app.goToTab("History")
        XCTAssertFalse(app.historyRows(startingWith: "Vaccine, Prevenar 13 · dose 3").isEmpty)
    }

    /// The Insights card opens the screen; a row opens its panel; deleting it drops the row.
    func testTheImmunizationsCardOpensTheScreen() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, seedVaccines: true))
        defer { app.terminate() }
        app.goToTab("Insights")
        let open = app.buttons["insights.immunizations.open"]
        for _ in 0..<8 where !open.isHittable { app.swipeUp() }
        XCTAssertTrue(app.tap(open, until: app.otherElements["immunizations.screen"]))
        let rows = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH 'immunizations.row.'"))
        XCTAssertEqual(rows.count, 4)
        XCTAssertTrue(app.tap(rows.element(boundBy: 0), until: app.buttons["immunization.delete"]))
        app.buttons["immunization.delete"].tap()
        app.buttons["immunization.confirmDelete"].tap()
        XCTAssertTrue(app.otherElements["immunizations.screen"].waitForExistence(timeout: 10))
        XCTAssertEqual(rows.count, 3)
    }

    /// The widget's link opens the vaccine panel.
    func testAVaccineLinkOpensItsPanel() {
        let app = Harness.launch(Harness.Launch(reset: true, seedBaby: true, logLink: "vaccine"))
        defer { app.terminate() }
        XCTAssertTrue(app.textFields["immunization.name"].waitForExistence(timeout: 10))
    }
```

Element types follow how the harness's existing tests address the same controls (a menu `Picker` is a button; the dose value's element type is whatever `accessibilityElement` yields — use `app.descendants(matching: .any)["immunization.dose"]` if `staticTexts` does not find it). The seeded baby in the first test is born today, which the birth rule allows for a shot logged now.

- [ ] **Step 3: Run the subset and every class the More sheet touches**

```bash
pgrep xcodebuild   # must print nothing
xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' \
  -parallel-testing-enabled NO -maximum-concurrent-test-simulator-destinations 1 \
  -only-testing:BabyTrackerUITests/LoggingFlowTests \
  -only-testing:BabyTrackerUITests/GrowthFlowUITests \
  -only-testing:BabyTrackerUITests/SettingsFlowTests
```

Expected: all pass. `AccessibilityAuditTests` and `UkrainianLayoutSmokeTests` also open More and History's filter; run those two classes in a second invocation and fix what the new tile or the sixth filter toggle breaks (a clipped label is fixed in the layout, never by loosening the audit).

- [ ] **Step 4: Screenshots**

Into `docs/verification/2026-10-11-immunizations/`, with `xcrun simctl launch <simulator-udid> <bundle id> <arguments>` then `xcrun simctl io <simulator-udid> screenshot <path>`; each in English at the default size and in Ukrainian at the largest size (`-UIPreferredContentSizeCategoryName UICTContentSizeCategoryAccessibilityXXXL -AppleLanguages "(uk)" -AppleLocale uk_UA`):

| File | State |
| --- | --- |
| `more.png` / `more-uk-a11y.png` | More sheet, default order: Solids, Growth, Vaccine, Past entry |
| `panel-first.png` | The panel with no saved names |
| `panel-picked.png` / `panel-picked-uk-a11y.png` | Seeded names, Prevenar 13 picked, dose 3 |
| `panel-date-refused.png` | A time before the birth date, with the message |
| `history-row.png` / `history-filter-uk-a11y.png` | The row; the filter sheet with six toggles |
| `insights-card.png` / `insights-card-empty.png` | The card with shots and with none |
| `screen.png` / `screen-uk-a11y.png` | The Immunizations screen, with its back button |
| `dock-vaccine.png` | `-uiTestHomeOrder vaccine,breast,bottle,sleep`: Vaccine as the big button |

Look at every image: the glyph reads as a syringe at History's 12–14 pt and the dock's 24 pt and is distinct from the meds glyph; no text is clipped or split mid-word; the pushed screen has a back button. Fix what is wrong and retake.

- [ ] **Step 5: Whole-suite gates, notes, tracker**

Run: `make test` → `OK: B+58 tests`. Run: `make server-test`. Run: `make server-protocol-e2e`.

`docs/verification/2026-10-11-immunizations/README.md`, in the shape of `docs/verification/2026-10-11-home-buttons/README.md`: branch and spec; **Automated** (each command with its real outcome and counts, including any known-flaky server test and its re-run); **Screenshots** (the table above); **Needs a phone** (the widget slot with Vaccine moved up; a shot logged on one phone appearing on the other; the Lock Screen is unaffected); **Not done** (the server MCP does not list immunizations).

`TASKS.md`: a new first section `# Immunizations (2026-10-11)` with the spec, plan and verification paths, the branch, "Not merged. Server deploy (schema 33) must precede the merge and both phones must be updated before the first shot is logged.", and one checked line per task plus unchecked lines for Task 15's steps and the user's phone check. `TASKS.md` in the main checkout may hold another session's uncommitted edits; if the branch cannot fast-forward for that reason, restore the base's `TASKS.md` on the branch and add the section to the working copy after the merge.

```bash
/usr/bin/git add Apps UITests docs/verification/2026-10-11-immunizations TASKS.md
/usr/bin/git commit -m "test(immunizations): UI tests, screenshots and verification notes

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 15: Review, then the rollout gate

**Files:** `docs/verification/2026-10-11-immunizations-rollout.md` (created during the rollout).

- [ ] **Step 1: Whole-branch review**

Read `git diff main...immunizations` end to end against the spec and this plan's Global Constraints and Review Focus. Check in particular: every key in `payloadFields()` equals the fixture's; no string in a view; no `Form`/`NavigationStack` at the panel's root; the three legacy goldens and the chain golden are byte-identical to `main` (`git diff --stat main...immunizations -- '*legacy*' '*chain*'` prints nothing); no MCP file changed. Fix what is found, re-run the affected gates, commit.

- [ ] **Step 2: Stop and report to the user**

Report: what was built, every gate's outcome, the screenshots' location, and what remains. **Do not deploy, merge or install without the user's explicit go-ahead for each of the three.** The order, once approved, and the reason for it:

1. **Server first.** Follow the sequence recorded in `docs/verification/2026-10-09-baby-icons-rollout.md`, adjusted for the authority server (no ledger tars; compose files `compose.yaml`, `deploy/unraid/compose.yaml`, `deploy/unraid/push.yaml`): preflight (running revision, schema 32, containers healthy); `git archive <sha> Server` uploaded and checksummed; a pre-deploy dump and a cutover dump with the app stopped, each verified with `pg_restore --list`; the rollback set; `provision-unraid-secrets.sh` with `BT_ALLOW_SOURCE_UPDATE=1`; build the image; stop only the app → dump → `compose run --rm --no-deps migrate` → confirm `schema_compatibility.current_version = 33` → `compose up -d --wait app`; `verify-unraid-deployment.sh verify`; five minutes of logs as counts by level and status. The window is exactly 33, so the old binary cannot run against the migrated database and the new one cannot run against the old: migrate and restart are one step. This is the first migration applied to the homelab since the baseline; if any step behaves differently from the record, stop and report rather than improvise.
2. **Merge** `immunizations` into `main` by fast-forward, leaving the main checkout's uncommitted files alone.
3. **Both phones**, from a clean tree of `main`: a signed Debug build (`xcodebuild -scheme BabyTracker -configuration Debug -destination 'generic/platform=iOS'`), `codesign --verify --deep --strict`, `devicectl device install app` on each, launch each, and confirm sync 200s on the server. The local store migrates V10 → V11 on first launch. **No shot is logged until both phones run this build**: a phone on the old build stops syncing at the first immunization. Never install a pre-V11 build on these phones afterwards.
4. Only then ask the user to log a vaccination on one phone and see it on the other.

- [ ] **Step 3: Record the rollout**

Write `docs/verification/2026-10-11-immunizations-rollout.md` with what was actually run and seen at each step, update `TASKS.md`, and tell the user that pumping's plan must be renumbered to migration `000034` and `SchemaV12` before it is built.

---

## Self-review notes

- **Spec coverage:** data and validation → Tasks 1, 2, 5; server migration, cascade, export, import → Tasks 2–3; phone schema and sync → Tasks 4, 6; names and next dose → Task 5; Home action and widget link → Task 9; panel → Task 8; History, filter, PDF → Tasks 7, 10; card and screen → Task 11; Ask → Task 12; two-phone proof → Task 13; UI tests, screenshots, copy in six languages → Tasks 7–12, 14; deploy order → Task 15. The MCP is out of scope by the spec.
- **Hashes:** the two fixture hashes cannot be known before the code runs. Task 1 computes them on the server; Task 4 requires Swift to arrive at the same values independently before pinning them.
- **Assumptions to re-check at Task 0:** `home-buttons` merged; no other `000033`; `EXPECTED_TESTS` baseline. Every later count is relative to that baseline.
