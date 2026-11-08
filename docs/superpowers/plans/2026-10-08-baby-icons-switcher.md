# Baby icons and shared switcher Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every baby gets an automatically assigned, synced boy/girl icon, and Home, History and Insights share one switcher header that shows it.

**Architecture:**
- **Icon id.** The icon is a stored, synced string id (`"boy.1"`…`"girl.4"`) on the baby, sent in the existing `baby` wire payload as an optional `icon` key.
- **Assignment.** A pure assignment rule in `BabyCore` picks the id. The store calls it on add, on a Boys/Girls change, and in a fill-in pass for existing babies.
- **Server.** The server stores the id, validates it, and keeps the stored value when an update leaves it out. It does this in Go before journaling, and with `COALESCE` in SQL as a backstop.
- **Display.** `BabyUI` gets the art, `BabyAvatarView` and `BabySwitcherHeader`. The header replaces the three hand-written headers.

**Tech Stack:**
- Swift 6.2, SwiftUI and SwiftData (iOS 26), Swift Testing.
- Go server with PostgreSQL migrations.
- XCUITest.

**Spec:** `docs/superpowers/specs/2026-10-08-baby-icons-switcher-design.md`

## Global Constraints

- **Valid icon ids:** exactly `boy.1 boy.2 boy.3 boy.4 girl.1 girl.2 girl.3 girl.4`. `neutral` is drawn but never stored or sent.
- **Wire key:** `icon`. It is optional. Send it only when it is set: never `"icon": null`, and never an empty string.
- **Wire version:** the baby payload version stays **1**. Do not bump `CurrentPayloadVersion` or the Swift mirror.
- **Hashes:** the canonical mutation hash includes `icon` **only when it is set**. Every existing receipt hash must stay byte-identical.
- **Frozen fixture:** do not edit `Server/api/fixtures/v1/baby.json` or its manifest SHA.
- **Server migration:** `000029_baby_icon.sql`. The schema compatibility window moves to min 29, max 29.
- **SwiftData:** add `SchemaV10`. `SchemaV9` keeps a frozen copy of the old `Baby` shape. Add one lightweight stage from V9 to V10. A V9 store must open **in place**, not be moved aside.
- **Accessibility ids that must survive:** `home.babySwitcher`, `history.babySwitcher`, `history.babyName`, `insights.babySwitcher`, `insights.babyName`.
- **Header type:** name in `.title2.weight(.bold)`, `lineLimit(2)`, `minimumScaleFactor(0.8)`. Icon 32 pt in the header, 28 pt in list rows. Tap area at least 44 pt.
- **Simulator:** only the shared device `platform=iOS Simulator,id=<simulator-udid>`. Run `pgrep xcodebuild` first, and never run two xcodebuild jobs at once.
- **Test count:** `make test` asserts a literal total (`Makefile` `EXPECTED_TESTS`, currently 1618). Every task that adds Swift tests bumps it in the same commit by exactly the number of new test cases the run reports.
- **History import:** the server builds imported babies from `ImportBaby`, which has no icon, and that stays as it is. After an import, the fill-in pass in Task 5 gives the family's babies their icons again, in the same fixed order. Do not change `ImportBaby`, `import_baby_candidates` or the importer.
- **Branch:** work in a worktree on branch `feat/baby-icons`, never directly on `main`. Other sessions commit to `main` in parallel.

## Review Focus

These are the five ways this is most likely to break for a real family. The test that pins each one is in the task named.

1. **An app build without `icon` renames a baby.** The server must keep the icon in the database, in the journal entry other phones pull, and in the snapshot. *Task 3, `TestBabyUpdateWithoutIconKeepsStoredIcon`.*
2. **An existing install upgrades.** The V9 store opens in place, the baby survives, and `iconID` is nil. Then the fill-in pass gives the baby an icon. *Task 4, `aBaselineStoreMigratesInPlaceToV10`; Task 5, `fillInAssignsInOrderAndIsIdempotent`.*
3. **Both phones run the fill-in pass on the same family.** They must reach the same ids, and a second pass writes nothing, so there is no revision loop. *Task 5, `fillInAssignsInOrderAndIsIdempotent`.*
4. **A read-only caregiver opens the app.** The fill-in pass must not throw into the UI or show a banner on every reload. It logs and moves on. *Task 5, `fillInInAReadOnlyRealmWritesNothing`.*
5. **A baby with no Boys/Girls set.** It shows the neutral face, the fill-in pass skips it, and it gets an icon only when a parent sets Boys/Girls. *Task 2, `unknownIDsDrawNeutral`; Task 5, `fillInSkipsBabiesWithoutSex`, `settingSexForTheFirstTimeAssigns`.*

---

## Setup (once, before Task 1)

- [ ] Create the worktree.

```bash
cd <repo>
git worktree add .worktrees/baby-icons -b feat/baby-icons main
cd .worktrees/baby-icons
```

All paths below are relative to the worktree root.

---

### Task 1: Icon art mockup (user sign-off gate)

**Files:**
- Create: `mockups/2026-10-08-baby-icons/index.html`
- Create: `mockups/2026-10-08-baby-icons/README.md`

**Interfaces:**
- Produces: 9 approved 24×24 SVGs (`boy.1`…`girl.4`, `neutral`) and 8 approved tint hex values, which Task 6 copies verbatim.

The style rules come from `Packages/BabyTracker/Sources/BabyUI/Resources/Icons.xcassets/icon-bottle.imageset/icon.svg`:
- `viewBox="0 0 24 24"` and `fill="none"`;
- `stroke="#000"` and `stroke-width="1.8"`;
- round caps and joins;
- template-rendered, so colour comes from the tint.

- [ ] **Step 1: Write the mockup page.**
  - The page shows each icon at 32 pt and 28 pt, tinted, on the app ground `#0D1224`.
  - It also shows a mock header, `[icon] Sophia ⌄`, in two variants: boy.1 with boy.2 (twins), and girl.1 with neutral.
  - Start from these drafts:

```html
<!doctype html><meta charset="utf-8"><title>Baby icons</title>
<style>
body{background:#0D1224;color:#F3F5FD;font:16px -apple-system,system-ui;padding:24px}
.grid{display:grid;grid-template-columns:repeat(5,120px);gap:16px}
.cell{text-align:center} .cell svg{width:32px;height:32px} .cell svg.s{width:28px;height:28px}
.hdr{display:flex;align-items:center;gap:8px;font:700 22px -apple-system;margin:12px 0}
.hdr svg{width:32px;height:32px} .chev{color:#9AA4C9;font-size:14px}
</style>
<svg style="display:none" xmlns="http://www.w3.org/2000/svg">
 <defs>
  <g id="face"><circle cx="12" cy="13" r="8"/></g>
  <g id="eyes-dot"><path d="M9.2 12.4h.01M14.8 12.4h.01"/></g>
  <g id="eyes-lash"><path d="M8.4 12.6q.8-.9 1.6 0M14 12.6q.8-.9 1.6 0"/></g>
  <g id="smile"><path d="M9.8 15.8q2.2 1.6 4.4 0"/></g>
 </defs>
 <symbol id="boy.1" viewBox="0 0 24 24"><use href="#face"/><use href="#eyes-dot"/><use href="#smile"/></symbol>
 <symbol id="boy.2" viewBox="0 0 24 24"><use href="#face"/><use href="#eyes-dot"/><use href="#smile"/><path d="M12 5c0-1.6 1-2.8 2.6-2.6"/></symbol>
 <symbol id="boy.3" viewBox="0 0 24 24"><use href="#face"/><use href="#eyes-dot"/><use href="#smile"/><path d="M4.6 10.4a7.6 7.6 0 0 1 14.8 0zM19.4 10.4h2.6"/></symbol>
 <symbol id="boy.4" viewBox="0 0 24 24"><use href="#face"/><use href="#eyes-dot"/><circle cx="12" cy="16.8" r="1.5"/><path d="M9.6 16.8h4.8"/></symbol>
 <symbol id="girl.1" viewBox="0 0 24 24"><use href="#face"/><use href="#eyes-lash"/><use href="#smile"/></symbol>
 <symbol id="girl.2" viewBox="0 0 24 24"><use href="#face"/><use href="#eyes-lash"/><use href="#smile"/><path d="M16.6 5.4 14.2 3.8v3.2zM16.6 5.4 19 3.8v3.2z"/></symbol>
 <symbol id="girl.3" viewBox="0 0 24 24"><use href="#face"/><use href="#eyes-lash"/><use href="#smile"/><circle cx="3.4" cy="9.4" r="1.8"/><circle cx="20.6" cy="9.4" r="1.8"/></symbol>
 <symbol id="girl.4" viewBox="0 0 24 24"><use href="#face"/><use href="#eyes-lash"/><use href="#smile"/><path d="M5.2 9.6a7.6 7.6 0 0 1 13.6 0"/><circle cx="16.8" cy="6" r="1.2"/></symbol>
 <symbol id="neutral" viewBox="0 0 24 24"><use href="#face"/><use href="#eyes-dot"/><path d="M10.2 16h3.6"/></symbol>
</svg>
<h1>Baby icons — draft</h1>
<div class="grid" id="grid"></div>
<h2>Headers</h2><div id="hdrs"></div>
<script>
const tint={"boy.1":"#7FB8FF","boy.2":"#5FD3C4","boy.3":"#9FD27A","boy.4":"#8A9BF0",
 "girl.1":"#F59AC0","girl.2":"#C4A6F5","girl.3":"#F2B061","girl.4":"#E89A7B","neutral":"#9AA4C9"};
const ico=(id,cls="")=>`<svg class="${cls}" fill="none" stroke="${tint[id]}" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><use href="#${id}"/></svg>`;
grid.innerHTML=Object.keys(tint).map(id=>`<div class="cell">${ico(id)} ${ico(id,"s")}<div>${id}</div></div>`).join("");
hdrs.innerHTML=[["boy.1","Max"],["boy.2","Leo"],["girl.1","Sophia"],["neutral","Mira"]]
 .map(([id,n])=>`<div class="hdr">${ico(id)}${n}<span class="chev">⌄</span></div>`).join("");
</script>
```

- [ ] **Step 2: Write `README.md`.**
  - It lists the 9 ids, the tint table, and the rule: the approved SVG paths and hex values get copied into Task 6 unchanged.
  - Leave a line `Status: awaiting user sign-off`.

- [ ] **Step 3: Render and check.**
  - Open `mockups/2026-10-08-baby-icons/index.html` in a browser and take a screenshot.
  - Check that all 9 icons render and that each pair of same-sex icons differs by both shape and colour.

- [ ] **Step 4: Commit.**

```bash
git add mockups/2026-10-08-baby-icons
git commit -m "mockup(babies): baby icon drafts for sign-off"
```

- [ ] **Step 5: GATE.** Show the user the screenshot and ask for sign-off or changes.
  - Tasks 2–5 do not depend on the art, so continue with them while waiting.
  - Task 6 must not start until the user approves.
  - Once approved, set `Status: approved <date>` in the README and record any changed paths or hex values there. Then commit.

---

### Task 2: `BabyAvatar` and the assignment rule (BabyCore)

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyCore/BabyAvatar.swift`
- Test: `Packages/BabyTracker/Tests/BabyCoreTests/BabyAvatarTests.swift`
- Modify: `Makefile` (`EXPECTED_TESTS`)

**Interfaces:**
- Produces:
  - `public enum BabyAvatar: String, CaseIterable, Sendable`, with cases `boy1…boy4`, `girl1…girl4` and `neutral`.
  - `public init(iconID: String?)`
  - `public static let storedIDs: [String]`
  - `public static func ids(for sex: GrowthReferenceSex) -> [String]`
  - `public static func assign(sex: GrowthReferenceSex, siblings: [String?]) -> String`

- [ ] **Step 1: Write the failing tests.**

```swift
import Testing
@testable import BabyCore

@Test func storedIDsAreExactlyTheEight() {
    #expect(BabyAvatar.storedIDs == ["boy.1", "boy.2", "boy.3", "boy.4", "girl.1", "girl.2", "girl.3", "girl.4"])
}

@Test func unknownIDsDrawNeutral() {
    #expect(BabyAvatar(iconID: nil) == .neutral)
    #expect(BabyAvatar(iconID: "") == .neutral)
    #expect(BabyAvatar(iconID: "neutral") == .neutral)
    #expect(BabyAvatar(iconID: "boy.9") == .neutral)
    #expect(BabyAvatar(iconID: "girl.3") == .girl3)
}

@Test func firstBabyGetsTheFirstIconOfTheirSet() {
    #expect(BabyAvatar.assign(sex: .boys, siblings: []) == "boy.1")
    #expect(BabyAvatar.assign(sex: .girls, siblings: ["boy.1"]) == "girl.1")
}

@Test func twinsGetDifferentIcons() {
    #expect(BabyAvatar.assign(sex: .boys, siblings: ["boy.1"]) == "boy.2")
}

@Test func aGapIsFilledFirst() {
    #expect(BabyAvatar.assign(sex: .boys, siblings: ["boy.1", "boy.3", nil, "girl.2"]) == "boy.2")
}

@Test func aFifthSameSexBabyReusesTheLeastUsedLowestNumber() {
    #expect(BabyAvatar.assign(sex: .girls, siblings: ["girl.1", "girl.2", "girl.3", "girl.4"]) == "girl.1")
    #expect(BabyAvatar.assign(sex: .girls, siblings: ["girl.1", "girl.2", "girl.3", "girl.4", "girl.1"]) == "girl.2")
}

@Test func otherSetAndUnknownIDsAreIgnored() {
    #expect(BabyAvatar.assign(sex: .boys, siblings: ["girl.1", "girl.2", "bogus", nil]) == "boy.1")
}
```

- [ ] **Step 2: Run the tests and confirm they fail.**

Run: `cd Packages/BabyTracker && swift test --filter BabyAvatarTests`
Expected: compile failure, `cannot find 'BabyAvatar' in scope`.

- [ ] **Step 3: Implement.**

```swift
/// A baby's face in the switcher. The stored id is the raw value of one of
/// the eight set cases and travels in the baby's wire payload as `icon`.
/// `neutral` is drawn for a baby whose Boys/Girls is not known yet; it is
/// never stored.
public enum BabyAvatar: String, CaseIterable, Sendable {
    case boy1 = "boy.1", boy2 = "boy.2", boy3 = "boy.3", boy4 = "boy.4"
    case girl1 = "girl.1", girl2 = "girl.2", girl3 = "girl.3", girl4 = "girl.4"
    case neutral

    public init(iconID: String?) {
        guard let iconID, Self.storedIDs.contains(iconID), let avatar = BabyAvatar(rawValue: iconID) else {
            self = .neutral
            return
        }
        self = avatar
    }

    public static let storedIDs: [String] = allCases.filter { $0 != .neutral }.map(\.rawValue)

    public static func ids(for sex: GrowthReferenceSex) -> [String] {
        let prefix = sex == .boys ? "boy." : "girl."
        return storedIDs.filter { $0.hasPrefix(prefix) }
    }

    /// The lowest-numbered icon of the set no sibling uses; once all four are
    /// taken, the one the fewest siblings use, lowest number on a tie.
    /// `siblings` is the other living babies in the same realm.
    public static func assign(sex: GrowthReferenceSex, siblings: [String?]) -> String {
        let candidates = ids(for: sex)
        let used = siblings.compactMap { $0 }
        return candidates.min { a, b in
            let countA = used.filter { $0 == a }.count, countB = used.filter { $0 == b }.count
            return countA != countB ? countA < countB : candidates.firstIndex(of: a)! < candidates.firstIndex(of: b)!
        }!
    }
}
```

- [ ] **Step 4: Run the tests and confirm they pass.**

Run: `cd Packages/BabyTracker && swift test --filter BabyAvatarTests`
Expected: 7 tests pass.

- [ ] **Step 5: Run the full suite and bump the total.**
  - Run `make test`.
  - The total is 7 above the expected 1618, so set `EXPECTED_TESTS := 1625` in `Makefile`.
  - Run it again; expect PASS.

- [ ] **Step 6: Commit.**

```bash
git add Packages/BabyTracker/Sources/BabyCore/BabyAvatar.swift Packages/BabyTracker/Tests/BabyCoreTests/BabyAvatarTests.swift Makefile
git commit -m "feat(core): baby avatar ids and the assignment rule"
```

---

### Task 3: Server stores, validates and keeps `icon`

**Files:**
- Create: `Server/migrations/000029_baby_icon.sql`
- Modify:
  - `Server/internal/protocol/entities.go:12-16` (`BabyPayload`)
  - `Server/internal/protocol/canonical.go:113-118` (baby case)
  - `Server/internal/records/validation.go:167-176` (`validateBaby`)
  - `Server/internal/records/babies.go` (`GetBaby`, `putBaby`)
  - `Server/internal/records/repository.go:~237` (merge before the entity is built)
  - `Server/internal/records/snapshot.go:69` (baby `jsonb_build_object`)
  - `Server/api/openapi.yaml:1815-1822` (`BabyPayload`)
  - `Server/internal/migrate/compatibility.json`
- Modify the version-28 tests and scripts listed in Step 7.
- Test:
  - `Server/internal/protocol/canonical_test.go`
  - `Server/internal/records/validation_test.go`
  - `Server/internal/sync/baby_icon_test.go` (new)
  - `Server/internal/database/records_schema_test.go`

**Interfaces:**
- Produces:
  - Wire field `icon` on baby create and update.
  - Server behaviour: an omitted `icon` keeps the stored one; a present `icon` replaces it; an invalid `icon` is rejected with `ErrInvalidMutation`.

- [ ] **Step 1: Write the failing unit tests (no database).**

In `Server/internal/records/validation_test.go`:

```go
func TestBabyIconValidation(t *testing.T) {
	limits := protocol.DefaultLimits()
	for _, icon := range []string{"boy.1", "boy.4", "girl.1", "girl.4"} {
		m := mustMutation(t, "baby.json")
		p := m.Payload.(protocol.BabyPayload)
		value := icon
		p.Icon = &value
		m.Payload = p
		if err := ValidateMutation(m, limits); err != nil {
			t.Fatalf("icon %q rejected: %v", icon, err)
		}
	}
	for _, icon := range []string{"", "neutral", "boy.0", "boy.5", "girl", "BOY.1"} {
		m := mustMutation(t, "baby.json")
		p := m.Payload.(protocol.BabyPayload)
		value := icon
		p.Icon = &value
		m.Payload = p
		if err := ValidateMutation(m, limits); !errors.Is(err, protocol.ErrInvalidMutation) {
			t.Fatalf("icon %q error = %v", icon, err)
		}
	}
}
```

In `Server/internal/protocol/canonical_test.go`:

```go
func TestBabyMutationHashOmitsAbsentIcon(t *testing.T) {
	without := BabyPayload{Name: "Élie", BirthDate: "2026-01-12"}
	bytes, err := canonicalPayloadValue(without, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bytes.(map[string]any)["icon"]; ok {
		t.Fatal("absent icon must not appear in the canonical payload")
	}
	icon := "boy.2"
	with := BabyPayload{Name: "Élie", BirthDate: "2026-01-12", Icon: &icon}
	value, err := canonicalPayloadValue(with, false)
	if err != nil {
		t.Fatal(err)
	}
	if value.(map[string]any)["icon"] != "boy.2" {
		t.Fatalf("icon missing from canonical payload: %v", value)
	}
}
```

Check the real signature of `canonicalPayloadValue` (canonical.go, just above line 105) and match its arguments. The second argument is the "include baby id" flag.

Also add a decode test next to `TestDecodeMutationNormalizesValidPayloadBeforeAdmission`. Take the bytes of `api/fixtures/v1/baby.json`, insert `"icon":"girl.3"` into the payload object, and check that `DecodeMutation` succeeds with `*payload.Icon == "girl.3"`. Insert `"icon":null` instead and check that it decodes with `Icon == nil`.

- [ ] **Step 2: Run the tests and confirm they fail.**

Run: `cd Server && go test ./internal/records/ ./internal/protocol/ -run 'BabyIcon|BabyMutationHash|Icon' -count=1`
Expected: compile failure, `p.Icon undefined`.

- [ ] **Step 3: Implement the protocol, validation and hash.**

`entities.go`:

```go
type BabyPayload struct {
	Name      string  `json:"name"`
	BirthDate string  `json:"birth_date"`
	SortOrder uint32  `json:"sort_order"`
	Icon      *string `json:"icon,omitempty"`
}
```

`validation.go`, in `validateBaby` before the final return:

```go
	if payload.Icon != nil && !oneOf(*payload.Icon, "boy.1", "boy.2", "boy.3", "boy.4", "girl.1", "girl.2", "girl.3", "girl.4") {
		return protocol.ErrInvalidMutation
	}
```

`canonical.go`, baby case:

```go
	case BabyPayload:
		name, err := NormalizeDisplayName(value.Name)
		if err != nil {
			name = normalizeString(value.Name)
		}
		result := map[string]any{"birth_date": normalizeString(value.BirthDate), "name": name, "sort_order": value.SortOrder}
		// Only when set: a receipt stored before icons existed must hash
		// the same on retry.
		if value.Icon != nil {
			result["icon"] = *value.Icon
		}
		return result, nil
```

`openapi.yaml`: under `BabyPayload.properties`, add the following. Do not add `icon` to `required`.

```yaml
        icon:
          type: string
          enum: [boy.1, boy.2, boy.3, boy.4, girl.1, girl.2, girl.3, girl.4]
```

- [ ] **Step 4: Run the unit tests and confirm they pass.**

Run: `cd Server && go test ./internal/records/ ./internal/protocol/ -count=1`
Expected: PASS. This includes the existing `compatibility_test.go` fixture checks: `baby.json` is unchanged and `icon` is `omitempty`.

- [ ] **Step 5: Write the migration.**

`Server/migrations/000029_baby_icon.sql`:

```sql
-- A baby's switcher icon. Assigned by the app, never derived here.
ALTER TABLE public.babies ADD COLUMN icon text
  CHECK (icon IS NULL OR icon IN ('boy.1','boy.2','boy.3','boy.4','girl.1','girl.2','girl.3','girl.4'));

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 29,principal,object_kind,object_identity,privileges FROM public.schema_privilege_manifest WHERE schema_version=28;
UPDATE public.schema_compatibility SET current_version=29, introduced_by='000029_baby_icon.sql',recorded_at=now() WHERE singleton;
```

Before writing it, open the tail of `000026_sleep_sessions.sql` and `000028_live_activity_push.sql`, and copy their bookkeeping lines exactly, changing only the numbers. If 000028 has more bookkeeping statements than the two above, copy those too.

`Server/internal/migrate/compatibility.json`:
- set `"min": 29, "max": 29`;
- append the entry for version 29 in the same shape as 28;
- the `sha256` value is the output of `shasum -a 256 Server/migrations/000029_baby_icon.sql`.

- [ ] **Step 6: Persist, merge and snapshot.**

`babies.go`, `GetBaby`:
- the SELECT ends `...,sort_order,icon FROM babies ...`;
- `Scan(..., &payload.SortOrder, &payload.Icon)`.

`babies.go`, `putBaby`:
- add `icon` to the column list and `$14` to VALUES;
- in `DO UPDATE SET` add `icon=COALESCE(EXCLUDED.icon,babies.icon)`;
- pass `payload.Icon` as the 14th argument.

`repository.go`, immediately before `var entity protocol.CanonicalEntity`:

```go
	payload := m.Payload
	// An update from a build that predates icons leaves `icon` out. The
	// stored icon is kept here, not only in SQL, so the journal entry other
	// phones pull and the response carry it too.
	if next, ok := payload.(protocol.BabyPayload); ok && next.Icon == nil && existing != nil {
		if old, ok := existing.Payload.(protocol.BabyPayload); ok && old.Icon != nil {
			next.Icon = old.Icon
			payload = next
		}
	}
```

In the non-delete branch, change `Payload: m.Payload` to `Payload: payload`. Do not change `m` itself; the receipt hash was taken from it.

`snapshot.go:69`: change it to `'name',b.name,'birth_date',b.birth_date::text,'sort_order',b.sort_order,'icon',b.icon`.

- [ ] **Step 7: Move every hard-coded schema version from 28 to 29.**

Update each of these:
- `internal/migrate/compatibility_test.go`: lines ~50, 131, 210, 259 and 282 (`!= 28` → `!= 29`). Lines ~308-312 should check `Known[28]` and the `000029` SHA, while keeping the existing check on `Known[27]`.
- `internal/database/{records,snapshot,import,identity,timer,delivery}_schema_test.go`: `current_version != 28` → `29`.
- `internal/database/roles_test.go:137,182`: these use a fake "next" version `SELECT 29 … WHERE schema_version=28`. Change them to `SELECT 30 … WHERE schema_version=29`.
- `internal/tenant/context_test.go:355`.
- `scripts/lib/disposable-sync.sh:313-318`, `scripts/protocol_e2e_test.go:218` and `scripts/smoke_test.go:178`.
- `scripts/rehearse-live-activity-upgrade.sh` lines 232, 547, 550, 612, 645 and 672. Read each one first. A line that means "the version before the upgrade under rehearsal" stays 28 or 27; only "current" moves.

Then check for anything missed:

```bash
cd Server && rtk proxy grep -rnE "\b28\b" internal scripts --include='*.go' --include='*.sh' | grep -iE "version|schema|compat|current" 
```

Fix each hit that means "current schema", and leave the others.

`internal/database/records_schema_test.go:18`: append `"icon"` to the `babies` column list. Also look at the second column check around lines 80-101 and add it there if it lists columns too.

- [ ] **Step 8: Write the database integration test.**

`Server/internal/sync/baby_icon_test.go`. Model it on `TestSnapshotPagesStayAtOneHighWater` in `internal/sync/snapshot_test.go:200` and the testkit helpers `SeedBabies` and `RenameBaby` in `internal/testkit/fixtures.go:598-630`.

```go
func TestBabyUpdateWithoutIconKeepsStoredIcon(t *testing.T) {
	h := testkit.New(t)
	ids := h.SeedBabies(t, 1)
	// Set an icon with an update that carries one.
	h.SetBabyIcon(t, ids[0], "girl.2")
	// An older build renames without the key.
	outcome := h.RenameBaby(t, ids[0], "Renamed")
	if got := outcome.CanonicalEntity.Payload.(protocol.BabyPayload).Icon; got == nil || *got != "girl.2" {
		t.Fatalf("outcome icon = %v", got)
	}
	// What the journal hands other phones.
	page := h.PullAll(t)
	assertBabyIcon(t, page, ids[0], "girl.2")
	// What a fresh snapshot shows.
	snapshot := h.FullSnapshot(t)
	assertBabyIcon(t, snapshot, ids[0], "girl.2")
	// A present icon replaces the stored one.
	h.SetBabyIcon(t, ids[0], "girl.4")
	assertBabyIcon(t, h.FullSnapshot(t), ids[0], "girl.4")
}
```

Add `SetBabyIcon` to `internal/testkit/fixtures.go`, right after `RenameBaby` and built the same way. Load `MustMutation(t, "baby.json")`, set the entity id, operation `update`, and `Payload.Icon`, then apply it.

`RenameBaby`, `PullAll` and `FullSnapshot` may have different names or return types. Read `fixtures.go` and the snapshot test, and use the real helpers, keeping the same three assertions: the outcome, the pull or journal page, and the snapshot.

`assertBabyIcon` is a local helper in the test file. It finds the baby entity by id in the page's entities and compares `*Payload.(protocol.BabyPayload).Icon`.

Add a second test, `TestBabyIconOutOfSetIsRejected`. Apply a baby update with `icon` `"boy.7"` and check that the outcome is a rejection (status `"rejected"`). Copy the rejection assertion that `validation_test.go` or the sync tests already use.

- [ ] **Step 9: Run the full server suite with a database.**

Run: `make server-test`
This starts a throwaway Postgres in Docker.
Expected: PASS. If a version-28 assertion fails, it was missed in Step 7; fix it and run again.

- [ ] **Step 10: Commit.**

```bash
git add Server
git commit -m "feat(server): optional synced baby icon, kept when an update omits it"
```

---

### Task 4: Client model, schema V10 and wire mapping

**Files:**
- Modify:
  - `Packages/BabyTracker/Sources/BabyStore/Models/Baby.swift`
  - `Packages/BabyTracker/Sources/BabyStore/Models/SchemaV9.swift`
  - `Packages/BabyTracker/Sources/BabyStore/Models/BabyStoreMigrationPlan.swift`
  - `Packages/BabyTracker/Sources/BabyStore/Models/Schema.swift:5,60,75`
  - `Packages/BabyTracker/Sources/BabyStore/EntryPayload.swift:41-47`
  - `Packages/BabyTracker/Sources/BabyProtocol/ContentHash.swift:41-46`
  - `Packages/BabyTracker/Sources/BabyStore/BabyStore+RemoteApply.swift:276-280, 470-479`
  - `Packages/BabyTracker/Sources/BabyStore/BabyStore+Growth.swift:144-149` (rollback restore)
  - `Packages/BabyTracker/Tests/BabyStoreTests/BaselineStoreTests.swift`
  - `GrowthStoreTests.swift:304` and `RefreshRecoveryStoreTests.swift:222,243`: replace `SchemaV9.self` with `SchemaV10.self` where they mean "the current schema".
- Create: `Packages/BabyTracker/Sources/BabyStore/Models/SchemaV10.swift`
- Test:
  - `Packages/BabyTracker/Tests/BabyStoreTests/BabyIconSyncTests.swift` (new)
  - `BaselineStoreTests.swift`

**Interfaces:**
- Consumes: the wire key `icon` (Task 3).
- Produces:
  - `Baby.iconID: String?`
  - `SchemaV10`
  - `Baby.payloadFields()` includes `"icon"` only when it is set
  - remote apply writes `iconID` from `"icon"`

- [ ] **Step 1: Write the failing tests.**

`BabyIconSyncTests.swift`. Look at an existing BabyStore test file (for example `WireShapeTests.swift`) for how a test opens an in-memory store, and reuse that helper.

```swift
import Testing
import Foundation
@testable import BabyStore

@MainActor
struct BabyIconSyncTests {
    @Test func payloadOmitsIconWhenUnset() {
        let baby = Baby(name: "Mira", birthDate: .now)
        #expect(baby.payloadFields()["icon"] == nil)
    }

    @Test func payloadCarriesIconWhenSet() {
        let baby = Baby(name: "Mira", birthDate: .now)
        baby.iconID = "girl.2"
        #expect(baby.payloadFields()["icon"] as? String == "girl.2")
    }

    @Test func remoteApplySetsIconAndAnAbsentKeyKeepsIt() throws {
        // Use the same remote-apply entry point and canonical-entity builder
        // as the existing baby remote-apply tests (grep RemoteApply tests
        // for `"sort_order"`). First apply a baby payload with "icon":"boy.3"
        // and expect iconID == "boy.3"; then apply an update without the key
        // and expect iconID still "boy.3".
    }
}
```

Write `remoteApplySetsIconAndAnAbsentKeyKeepsIt` in full. Before writing it, run `rtk proxy grep -rln "\"sort_order\"" Packages/BabyTracker/Tests` to find the existing remote-apply baby test, then copy its setup exactly. The two assertions are the ones described in the comment.

In `BaselineStoreTests.swift`:
- Change `theMigrationPlanHasOneSchemaAndNoStages` and rename it `theMigrationPlanHasV9AndV10WithOneStage`:

```swift
    @Test func theMigrationPlanHasV9AndV10WithOneStage() {
        #expect(BabyStoreMigrationPlan.schemas.map { $0.versionIdentifier } == [Schema.Version(9, 0, 0), Schema.Version(10, 0, 0)])
        #expect(BabyStoreMigrationPlan.stages.count == 1)
    }
```

- Add a migration test:

```swift
    /// An install from before icons opens in place: nothing moved aside,
    /// the baby kept, its icon nil until the fill-in pass gives it one.
    @Test func aBaselineStoreMigratesInPlaceToV10() throws {
        let directory = try Self.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let url = directory.appending(path: "default.store")
        let babyID = UUID()
        try autoreleasepool {
            let schema = Schema(versionedSchema: SchemaV9.self)
            let container = try BabySchema.makeContainer(schema: schema, configurations: [ModelConfiguration(schema: schema, url: url)])
            let context = ModelContext(container)
            context.insert(SchemaV9.Baby(id: babyID, name: "Mira", birthDate: .now))
            try context.save()
        }

        let reopened = ModelContext(try BabySchema.container(storeURL: url, ifIncompatible: .moveAside))

        let babies = try reopened.fetch(FetchDescriptor<Baby>())
        #expect(babies.map(\.id) == [babyID])
        #expect(babies.first?.iconID == nil)
        #expect(try Self.movedAside(in: directory).isEmpty)
        #expect(try Self.isBaseline(url))
    }
```

Then update the rest of the file:
- `isBaseline` should compare against `SchemaV10`.
- `aStoreAPlannedSchemaWroteIsNotIncompatible` should pass `[SchemaV10.self]` where it means the current schema.
- `OlderSchema.models` should be built from `SchemaV10.models`. It stands in for an unplanned version, so its checksum must differ from both V9 and V10. It replaces `FeedEntry`, so it still does.

- [ ] **Step 2: Run the tests and confirm they fail.**

Run: `cd Packages/BabyTracker && swift test --filter 'BabyIconSyncTests|BaselineStoreTests'`
Expected: compile failure, `value of type 'Baby' has no member 'iconID'`.

- [ ] **Step 3: Implement the model and schema.**

`Baby.swift`: add `public var iconID: String?` after `photoData`.

`SchemaV9.swift`:
- Replace `Baby.self` in `models` with `SchemaV9.Baby.self`.
- Add the frozen shape inside the enum. It is the current `Baby` without `iconID`:

```swift
    /// `Baby` as V9 stored it, before `iconID`. Frozen: never edit.
    @Model public final class Baby {
        public var id: UUID = UUID()
        public var realmKey: String = RealmID.local.encodedKey
        public var name: String = ""
        public var birthDate: Date = Date()
        public var birthDateISO: String?
        public var photoData: Data?
        public var sortOrder: Int = 0
        public var lastModifiedAt: Date = Date()
        public var isTombstoned: Bool = false

        public init(id: UUID, name: String, birthDate: Date) {
            self.id = id
            self.name = name
            self.birthDate = birthDate
        }
    }
```

- Update the doc comment: V9 is no longer the only version, and its `Baby` is frozen.
- `SchemaV9.swift` needs `import Foundation` for `UUID`, `Date` and `Data`.

`SchemaV10.swift`:

```swift
import SwiftData

/// V9 plus `Baby.iconID`, the synced switcher icon. Every model here is the
/// live type; a later schema that changes one must freeze it in this file.
public enum SchemaV10: VersionedSchema {
    public static let versionIdentifier = Schema.Version(10, 0, 0)
    public static var models: [any PersistentModel.Type] {
        [
            Baby.self, FeedEntry.self, SleepEntry.self, DiaperEntry.self, SupplementEntry.self,
            ActiveTimer.self, ChangeLogEntry.self, SyncOutboxEntry.self, CanonicalEntityBase.self,
            SyncCursorState.self, ImportCheckpoint.self, RealmTransitionCheckpoint.self,
            CaregiverAttribution.self, FencedLease.self, AccountAccessState.self, RealmWritePermission.self,
            GrowthProfile.self, MeasurementCheckIn.self,
        ]
    }
}
```

`BabyStoreMigrationPlan.swift`:

```swift
/// V9 is the baseline every install started from; V10 adds the baby icon.
public enum BabyStoreMigrationPlan: SchemaMigrationPlan {
    public static var schemas: [any VersionedSchema.Type] { [SchemaV9.self, SchemaV10.self] }
    public static var stages: [MigrationStage] {
        [.lightweight(fromVersion: SchemaV9.self, toVersion: SchemaV10.self)]
    }
}
```

`Schema.swift`: change `BabySchema.all = SchemaV10.models`, and both `Schema(versionedSchema: SchemaV9.self)` to `SchemaV10.self`.

Read `IncompatibleStore.swift:56-64` and confirm it reads `BabyStoreMigrationPlan.schemas`. If it does, a V9 store is now "planned" and opens in place. If it hard-codes V9, change it to the plan's list.

Inside `BabyStore` source files, the unqualified name `Baby` still means the live type, because the frozen one is `SchemaV9.Baby`. If the compiler reports ambiguity anywhere, qualify it as `BabyStore.Baby`.

- [ ] **Step 4: Implement the wire mapping.**

`EntryPayload.swift`, `Baby.payloadFields()`:

```swift
    public func payloadFields() -> [String: Any] {
        var fields: [String: Any] = [
            "name": name,
            "birth_date": (try? LocalDate(displaying: birthDate, in: .current))?.iso8601 ?? "",
            "sort_order": sortOrder,
        ]
        // Only when set: `baby.json`'s key set is the contract for a baby
        // without one, and the server keeps a stored icon an update leaves out.
        if let iconID { fields["icon"] = iconID }
        return fields
    }
```

`ContentHash.swift`, baby case. Read the `ContentFields` type first; it wraps `[String: JSONValue]`. If it exposes the raw dictionary, use it as below. If not, add a `func has(_ key: String) -> Bool` to it.

```swift
        case "baby":
            var object: [String: JSONValue] = [
                "birth_date": p.text("birth_date"),
                "name": .string(displayName(p.string("name") ?? "")),
                "sort_order": p.integer("sort_order"),
            ]
            // Mirrors the server: present only when set, so existing hashes hold.
            if p.string("icon") != nil { object["icon"] = p.text("icon") }
            return .object(object)
```

`BabyStore+RemoteApply.swift`:
- In the field merge's `.baby` case, after `sort_order`, add:

```swift
            if let value = fields["icon"] as? String { row.iconID = value }
```

- `insertRowIfUnseen`'s `.baby` case needs no change; the merge that follows fills the icon.

`BabyStore+Growth.swift` rollback restore, after `baby.birthDateISO = canonical.birthDateISO`, add:

```swift
                    baby.iconID = canonical.iconID
```

- [ ] **Step 5: Run the tests and confirm they pass.**

Run: `cd Packages/BabyTracker && swift test --filter 'BabyIconSyncTests|BaselineStoreTests|WireShapeTests|GrowthStoreTests|RefreshRecoveryStoreTests|ContentHashTests|ProtocolFixtureConformanceTests'`
Expected: PASS. `WireShapeTests.babyOutboxPayloadMatchesTheFixture` still passes because the icon is unset there.

- [ ] **Step 6: Run the full suite and bump the total by the new test count.** Run `make test`, set `EXPECTED_TESTS` to the reported total, then run it again.

- [ ] **Step 7: Commit.**

```bash
git add Packages/BabyTracker Makefile
git commit -m "feat(store): Baby.iconID in schema V10, synced as the optional icon key"
```

---

### Task 5: Assigning icons in the store, and the fill-in pass

**Files:**
- Modify:
  - `Packages/BabyTracker/Sources/BabyStore/BabyStore+Babies.swift` (`addBaby`, new `assignMissingBabyIcons`)
  - `Packages/BabyTracker/Sources/BabyStore/BabyStore+Growth.swift` (`updateBabyAndGrowthProfile`)
  - `Apps/BabyTrackerApp/RootView.swift` (`reload()`)
- Test: `Packages/BabyTracker/Tests/BabyStoreTests/BabyIconAssignmentTests.swift` (new)

**Interfaces:**
- Consumes: `BabyAvatar.assign(sex:siblings:)` (Task 2) and `Baby.iconID` (Task 4).
- Produces: `public func assignMissingBabyIcons() throws -> Int` on `BabyStore`. It returns the number of babies assigned and writes nothing when that number is 0.

- [ ] **Step 1: Write the failing tests.**

Reuse the in-memory store helper the other BabyStore tests use, for example `GrowthStoreTests`. Read its setup and copy it.

For `fillInInAReadOnlyRealmWritesNothing`, find how existing tests put a store into a read-only realm: run `rtk proxy grep -rn "allowsReadOnly\|readOnly" Packages/BabyTracker/Tests/BabyStoreTests | head`, then copy that setup.

```swift
import Testing
import Foundation
import BabyCore
@testable import BabyStore

@MainActor
struct BabyIconAssignmentTests {
    @Test func addBabyAssignsFromTheSet() throws {
        let store = try makeStore()
        let first = try store.addBaby(name: "Max", birthDate: .now, referenceSex: .boys, expectedDueDate: nil)
        let twin = try store.addBaby(name: "Leo", birthDate: .now, referenceSex: .boys, expectedDueDate: nil)
        let sister = try store.addBaby(name: "Sophia", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        #expect(first.iconID == "boy.1")
        #expect(twin.iconID == "boy.2")
        #expect(sister.iconID == "girl.1")
        // Set before the create was recorded, so the create's payload carries it.
        #expect(sister.payloadFields()["icon"] as? String == "girl.1")
    }

    @Test func aDeletedSiblingFreesItsIcon() throws {
        let store = try makeStore()
        let a = try store.addBaby(name: "A", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        _ = try store.addBaby(name: "B", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        try store.deleteBaby(a)
        let c = try store.addBaby(name: "C", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        #expect(c.iconID == "girl.1")
    }

    @Test func changingSexReassignsFromTheOtherSet() throws {
        let store = try makeStore()
        let baby = try store.addBaby(name: "Kim", birthDate: .now, referenceSex: .boys, expectedDueDate: nil)
        try store.updateBabyAndGrowthProfile(baby, name: "Kim", birthDate: baby.birthDate, referenceSex: .girls, expectedDueDate: nil)
        #expect(baby.iconID == "girl.1")
    }

    @Test func savingWithTheSameSexKeepsTheIcon() throws {
        let store = try makeStore()
        _ = try store.addBaby(name: "A", birthDate: .now, referenceSex: .boys, expectedDueDate: nil)
        let b = try store.addBaby(name: "B", birthDate: .now, referenceSex: .boys, expectedDueDate: nil)
        try store.updateBabyAndGrowthProfile(b, name: "Bee", birthDate: b.birthDate, referenceSex: .boys, expectedDueDate: nil)
        #expect(b.iconID == "boy.2")
    }

    @Test func settingSexForTheFirstTimeAssigns() throws {
        let store = try makeStore()
        let legacy = try insertLegacyBaby(in: store, name: "Old", sortOrder: 0)   // no profile, no icon
        try store.updateBabyAndGrowthProfile(legacy, name: "Old", birthDate: legacy.birthDate, referenceSex: .girls, expectedDueDate: nil)
        #expect(legacy.iconID == "girl.1")
    }

    @Test func fillInSkipsBabiesWithoutSex() throws {
        let store = try makeStore()
        let legacy = try insertLegacyBaby(in: store, name: "Old", sortOrder: 0)
        #expect(try store.assignMissingBabyIcons() == 0)
        #expect(legacy.iconID == nil)
    }

    @Test func fillInAssignsInOrderAndIsIdempotent() throws {
        let store = try makeStore()
        let second = try insertLegacyBaby(in: store, name: "Second", sortOrder: 1)
        let first = try insertLegacyBaby(in: store, name: "First", sortOrder: 0)
        let keeper = try insertLegacyBaby(in: store, name: "Keeper", sortOrder: 2)
        keeper.iconID = "boy.1"
        try store.setGrowthProfile(for: first.id, referenceSex: .boys, expectedDueDate: nil)
        try store.setGrowthProfile(for: second.id, referenceSex: .boys, expectedDueDate: nil)
        try store.setGrowthProfile(for: keeper.id, referenceSex: .boys, expectedDueDate: nil)

        #expect(try store.assignMissingBabyIcons() == 2)
        #expect(first.iconID == "boy.2")
        #expect(second.iconID == "boy.3")
        let revision = store.revision
        #expect(try store.assignMissingBabyIcons() == 0)
        #expect(store.revision == revision)
    }

    @Test func fillInInAReadOnlyRealmWritesNothing() throws {
        // Build a read-only shared realm as the existing read-only tests do,
        // with one baby that has a profile and no icon, then:
        //   #expect(throws: (any Error).self) { try store.assignMissingBabyIcons() }
        //   #expect(baby.iconID == nil)
    }
}
```

`insertLegacyBaby(in:name:sortOrder:)` is a private helper in the test file. It inserts `Baby(name:birthDate:sortOrder:realm:)` directly into the store's context with `store.activeRealm`, records it with `store.record(.create, on:)` (or the existing test helper that does the same), and returns it. It creates no growth profile. Write `fillInInAReadOnlyRealmWritesNothing` in full, using the setup found above.

- [ ] **Step 2: Run the tests and confirm they fail.**

Run: `cd Packages/BabyTracker && swift test --filter BabyIconAssignmentTests`
Expected: compile failure, `assignMissingBabyIcons` not found.

- [ ] **Step 3: Implement it in `BabyStore+Babies.swift`.**

In `addBaby`, between creating `baby` and `context.insert`:

```swift
            baby.iconID = BabyAvatar.assign(sex: referenceSex, siblings: existing.map(\.iconID))
```

Add to the extension:

```swift
    /// Gives every living baby that has Boys/Girls but no icon one, in list
    /// order then id, each pick counting the ones before it — so two phones
    /// doing this at once write the same ids. Writes nothing when nothing is
    /// missing; a read-only realm throws from the write boundary.
    @discardableResult
    func assignMissingBabyIcons() throws -> Int {
        let missing = try babies()
            .filter { $0.iconID == nil }
            .compactMap { baby in try growthProfile(for: baby.id)?.referenceSex.map { (baby, $0) } }
        guard !missing.isEmpty else { return 0 }
        let expected = try captureWriteContext()
        return try withRealmWrite(expected: expected) {
            let all = try self.babies().sorted { ($0.sortOrder, $0.id.uuidString) < ($1.sortOrder, $1.id.uuidString) }
            var assigned = 0
            for baby in all where baby.iconID == nil {
                guard let sex = try self.growthProfile(for: baby.id)?.referenceSex else { continue }
                baby.iconID = BabyAvatar.assign(sex: sex, siblings: all.filter { $0.id != baby.id }.map(\.iconID))
                baby.lastModifiedAt = self.now
                try self.recordLocked(.update, on: baby)
                assigned += 1
            }
            return assigned
        }
    }
```

In `BabyStore+Growth.swift`, `updateBabyAndGrowthProfile`, after `baby.birthDateISO = birthDay.iso8601` and before `recordLocked(.update, on: baby)`:

```swift
            if let referenceSex, baby.iconID == nil || self.growthProfile(for: baby.id)?.referenceSex != referenceSex {
                let siblings = try self.babies().filter { $0.id != baby.id }.map(\.iconID)
                baby.iconID = BabyAvatar.assign(sex: referenceSex, siblings: siblings)
            }
```

`growthProfile(for:)` throws, so write `try self.growthProfile(for: baby.id)` inside the condition. Swift needs the `try` to cover the whole expression. Reading the profile before `setGrowthProfileLocked` runs is intentional: it compares against the old sex.

- [ ] **Step 4: Run the tests and confirm they pass.**

Run: `cd Packages/BabyTracker && swift test --filter 'BabyIconAssignmentTests|GrowthStoreTests|BabyIconSyncTests'`
Expected: PASS.

- [ ] **Step 5: Call the fill-in pass from the app.**

In `Apps/BabyTrackerApp/RootView.swift`, `reload()`, just before `babies = try store.babies()`:

```swift
            // Older babies, and babies a sync brought in, get their icon here.
            // Logged, not reported: a read-only family would otherwise show
            // a banner on every reload.
            do { try store.assignMissingBabyIcons() }
            catch { AppLog.store.error("Baby icon fill-in skipped: \(String(describing: type(of: error)), privacy: .public)") }
```

`reload()` runs on launch and on every `store.revision` change, which includes a remote apply. A pass that assigns something bumps the revision once. The next pass finds nothing and writes nothing, so there is no loop.

- [ ] **Step 6: Run the full suite, bump the total, and build the app.**

Run: `make test`, then update `EXPECTED_TESTS`.

Then build:

```bash
pgrep xcodebuild || xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' build
```

Expected: BUILD SUCCEEDED.

- [ ] **Step 7: Commit.**

```bash
git add Packages/BabyTracker Apps/BabyTrackerApp/RootView.swift Makefile
git commit -m "feat(store): assign baby icons on add, on Boys/Girls change, and fill in older babies"
```

---

### Task 6: Icon assets, tints and `BabyAvatarView` (needs the Task 1 sign-off)

**Files:**
- Create 9 imagesets under `Packages/BabyTracker/Sources/BabyUI/Resources/Icons.xcassets/`: `avatar-boy-1` … `avatar-boy-4`, `avatar-girl-1` … `avatar-girl-4`, and `avatar-neutral`. Each holds `Contents.json` and `icon.svg`.
- Create: `Packages/BabyTracker/Sources/BabyUI/BabyAvatarView.swift`
- Modify: `Packages/BabyTracker/Sources/BabyUI/Palette.swift`
- Test: `Packages/BabyTracker/Tests/BabyUITests/BabyAvatarViewTests.swift`

**Interfaces:**
- Consumes: `BabyAvatar` (Task 2) and the approved SVGs and tints (Task 1 README).
- Produces:
  - `public struct BabyAvatarView: View` with `init(_ avatar: BabyAvatar, size: CGFloat)`
  - `BabyAvatar.assetName: String` and `BabyAvatar.tint: Color`, as extensions in BabyUI

- [ ] **Step 1: Write the failing test.**

```swift
import Testing
import BabyCore
@testable import BabyUI
#if canImport(UIKit)
import UIKit
#endif

@Test func everyAvatarHasAnAssetAndADistinctTint() {
    #expect(Set(BabyAvatar.allCases.map(\.assetName)).count == BabyAvatar.allCases.count)
    #if canImport(UIKit)
    for avatar in BabyAvatar.allCases {
        #expect(UIImage(named: avatar.assetName, in: .module, with: nil) != nil, "\(avatar) asset missing")
    }
    #endif
    let tints = BabyAvatar.allCases.filter { $0 != .neutral }.map { "\($0.tint)" }
    #expect(Set(tints).count == 8)
}
```

- [ ] **Step 2: Run the test and confirm it fails.**

Run: `cd Packages/BabyTracker && swift test --filter everyAvatarHasAnAssetAndADistinctTint`
Expected: compile failure, `assetName` not found.

- [ ] **Step 3: Add the assets.**
  - Each `icon.svg` is the approved symbol from Task 1, written out as a standalone file. Expand `<use href>` to the real paths; Xcode does not resolve `<use>`.
  - The root element is `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="24" height="24" fill="none" stroke="#000" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">`.
  - Each `Contents.json` is a copy of `icon-bottle.imageset/Contents.json` (template rendering, preserves vector).

- [ ] **Step 4: Implement.**

`Palette.swift`: add the 8 approved tints from the Task 1 README, as `Palette.avatarBoy1` … `Palette.avatarGirl4`, in the file's existing `Color(red:green:blue:)` style with a hex comment.

`BabyAvatarView.swift`:

```swift
import SwiftUI
import BabyCore

public extension BabyAvatar {
    var assetName: String { "avatar-" + rawValue.replacingOccurrences(of: ".", with: "-") }

    var tint: Color {
        switch self {
        case .boy1: Palette.avatarBoy1
        case .boy2: Palette.avatarBoy2
        case .boy3: Palette.avatarBoy3
        case .boy4: Palette.avatarBoy4
        case .girl1: Palette.avatarGirl1
        case .girl2: Palette.avatarGirl2
        case .girl3: Palette.avatarGirl3
        case .girl4: Palette.avatarGirl4
        case .neutral: Palette.secondaryText
        }
    }
}

/// A baby's face, tinted with its own colour. Hidden from VoiceOver: the
/// name beside it is what identifies the baby.
public struct BabyAvatarView: View {
    private let avatar: BabyAvatar
    private let size: CGFloat

    public init(_ avatar: BabyAvatar, size: CGFloat) {
        self.avatar = avatar
        self.size = size
    }

    public var body: some View {
        Image(avatar.assetName, bundle: .module)
            .renderingMode(.template)
            .resizable()
            .scaledToFit()
            .frame(width: size, height: size)
            .foregroundStyle(avatar.tint)
            .accessibilityHidden(true)
    }
}
```

`neutral`'s raw value is `"neutral"`, so its asset is `avatar-neutral`.

- [ ] **Step 5: Run the test and confirm it passes.** Run `cd Packages/BabyTracker && swift test --filter BabyUITests`; expect PASS. Then run `make test` and bump `EXPECTED_TESTS`.

- [ ] **Step 6: Commit.**

```bash
git add Packages/BabyTracker Makefile
git commit -m "feat(ui): baby avatar icons, tints and BabyAvatarView"
```

---

### Task 7: One `BabySwitcherHeader` on Home, History and Insights

**Files:**
- Create: `Packages/BabyTracker/Sources/BabyUI/BabySwitcherHeader.swift`
- Modify:
  - `Packages/BabyTracker/Sources/BabyUI/Localizable.xcstrings`: add "Switch baby" with the same de, es, fr, it and uk values as in `InsightsFeature/Localizable.xcstrings`.
  - `Packages/BabyTracker/Sources/HomeFeature/HomeView.swift:184-213`
  - `Packages/BabyTracker/Sources/HistoryFeature/HistoryView.swift:~262-290` (`name`)
  - `Packages/BabyTracker/Sources/InsightsFeature/InsightsView.swift:108-134` (`name`)
  - `Apps/BabyTrackerApp/RootView.swift:223-300`: pass `babyAvatar: BabyAvatar(iconID: baby.iconID)` to the three views.

**Interfaces:**
- Consumes: `BabyAvatarView` and `BabyAvatar` (Task 6).
- Produces:
  - `public struct BabySwitcherHeader: View`
  - `init(babyName: String, avatar: BabyAvatar, switcherID: String, headingID: String, onSwitchBaby: (() -> Void)?)`
  - New parameter `babyAvatar: BabyAvatar = .neutral` on `HomeView`, `HistoryView` and `InsightsView`.

- [ ] **Step 1: Implement the header.**

```swift
import SwiftUI
import BabyCore

/// The baby's face and name, which is also the way to switch babies. One
/// view for Home, History and Insights so the three never drift apart again.
public struct BabySwitcherHeader: View {
    private let babyName: String
    private let avatar: BabyAvatar
    private let switcherID: String
    private let headingID: String
    private let onSwitchBaby: (() -> Void)?

    public init(babyName: String, avatar: BabyAvatar, switcherID: String, headingID: String,
                onSwitchBaby: (() -> Void)?) {
        self.babyName = babyName
        self.avatar = avatar
        self.switcherID = switcherID
        self.headingID = headingID
        self.onSwitchBaby = onSwitchBaby
    }

    public var body: some View {
        if let onSwitchBaby {
            Button(action: onSwitchBaby) {
                HStack(spacing: 8) {
                    label
                    Image(systemName: "chevron.down")
                        .font(.footnote.weight(.bold))
                        .foregroundStyle(Palette.secondaryText)
                }
                .frame(minHeight: 44)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(String(localized: "Switch baby", bundle: .module))
            .accessibilityValue(babyName)
            .accessibilityIdentifier(switcherID)
        } else if !babyName.isEmpty {
            label
                .frame(minHeight: 44)
                .accessibilityElement(children: .combine)
                .accessibilityAddTraits(.isHeader)
                .accessibilityIdentifier(headingID)
        }
    }

    private var label: some View {
        HStack(spacing: 8) {
            BabyAvatarView(avatar, size: 32)
            Text(babyName)
                .font(.title2.weight(.bold))
                .foregroundStyle(Palette.primaryText)
                .lineLimit(2)
                .minimumScaleFactor(0.8)
        }
    }
}
```

- [ ] **Step 2: Replace the three hand-written headers.**

**Home** (`HomeView.swift`):
- Add the stored property and init parameter `babyAvatar: BabyAvatar = .neutral`, placed after `babyBirthDate`.
- In `header`, replace the whole `Button(action: onSwitchBaby) { … }` and its four modifiers with:

```swift
            BabySwitcherHeader(babyName: babyName, avatar: babyAvatar,
                               switcherID: "home.babySwitcher", headingID: "home.babyName",
                               onSwitchBaby: onSwitchBaby)
```

- Change the outer `HStack(alignment: .firstTextBaseline, spacing: 8)` to `HStack(alignment: .center, spacing: 8)`, so the age sits level with the icon row.
- `HomeCopy.switchBaby` becomes unused. Delete it, and delete its entry in HomeFeature's xcstrings if no other file uses it (`rtk proxy grep -rn "switchBaby" Packages/BabyTracker/Sources`).

**History** (`HistoryView.swift`):
- Add `babyAvatar: BabyAvatar = .neutral` to the properties and init.
- Replace the body of `name` with `BabySwitcherHeader(babyName: babyName, avatar: babyAvatar, switcherID: "history.babySwitcher", headingID: "history.babyName", onSwitchBaby: onSwitchBaby)`.
- Delete `HistoryCopy.switchBaby` if it is now unused.

**Insights** (`InsightsView.swift`):
- Make the same change, with ids `insights.babySwitcher` and `insights.babyName`.
- Delete the `"Switch baby"` key from InsightsFeature's xcstrings if nothing else uses it.

**RootView:** pass `babyAvatar: BabyAvatar(iconID: baby.iconID)` to `HomeView`, `HistoryView` and `InsightsView`, and add `import BabyCore` if it is missing.

- [ ] **Step 3: Build and run the package tests.**

Run: `make test`. Expect PASS at an unchanged total. This task adds no unit tests: the header is layout, and the UI tests in Step 4 pin its ids.

Then build:

```bash
pgrep xcodebuild || xcodebuild -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' build
```

- [ ] **Step 4: Run the switcher UI tests (a subset only).**

```bash
pgrep xcodebuild || xcodebuild test -scheme BabyTracker \
  -destination 'platform=iOS Simulator,id=<simulator-udid>' \
  -parallel-testing-enabled NO -maximum-concurrent-test-simulator-destinations 1 \
  -only-testing:BabyTrackerUITests/GrowthFlowUITests \
  -only-testing:BabyTrackerUITests/SettingsFlowTests \
  -only-testing:BabyTrackerUITests/HomeSyncRecoveryUITests \
  -only-testing:BabyTrackerUITests/UkrainianLayoutSmokeTests
```

Expected: all pass. They find `home.babySwitcher` and `history.babySwitcher` by id, and `GrowthFlowUITests:106` matches on `value == "Second Baby"`, which the header keeps.

- [ ] **Step 5: Commit.**

```bash
git add Packages/BabyTracker Apps/BabyTrackerApp/RootView.swift
git commit -m "feat(ui): one BabySwitcherHeader with the baby's icon on Home, History and Insights"
```

---

### Task 8: Icons in the switcher list and Settings › Babies

**Files:**
- Modify:
  - `Packages/BabyTracker/Sources/BabyFeature/BabySwitcherSheet.swift:27-35`
  - `Packages/BabyTracker/Sources/SettingsFeature/SettingsView.swift:75-77`

**Interfaces:**
- Consumes: `BabyAvatarView` and `BabyAvatar(iconID:)`.

- [ ] **Step 1: Switcher sheet row.** Inside the label's `HStack`, before `Text(baby.name)`, add:

```swift
                            BabyAvatarView(BabyAvatar(iconID: baby.iconID), size: 28)
```

Add `import BabyCore` if needed.

- [ ] **Step 2: Settings row.** Make the same insertion before `Text(baby.name)` in `SettingsView`'s Babies section. Keep the row's existing `.frame(minHeight: 44)` and `.contentShape` exactly where they are; the comments there explain why.

- [ ] **Step 3: Build, then run the Settings and Growth UI tests.**

```bash
pgrep xcodebuild || xcodebuild test -scheme BabyTracker \
  -destination 'platform=iOS Simulator,id=<simulator-udid>' \
  -parallel-testing-enabled NO -maximum-concurrent-test-simulator-destinations 1 \
  -only-testing:BabyTrackerUITests/SettingsFlowTests \
  -only-testing:BabyTrackerUITests/GrowthFlowUITests
```

Expected: PASS.

- [ ] **Step 4: Commit.**

```bash
git add Packages/BabyTracker
git commit -m "feat(ui): baby icons in the switcher list and Settings › Babies"
```

---

### Task 9: End-to-end check, review, and the deploy gate

- [ ] **Step 1: Full verification.**
  - `make test`: PASS at the bumped total.
  - `make server-test`: PASS.
  - `make server-build`: PASS.
  - `make server-protocol-e2e`: PASS. It runs the Swift client against a live server, which covers a synced baby create with `icon`.

- [ ] **Step 2: Manual check on the shared simulator.**
  - Run `pgrep xcodebuild` first, then install the debug build.
  - Add Max (Boys), Leo (Boys) and Sophia (Girls).
  - Screenshot Home, History and Insights. Confirm that:
    - the three headers are identical in size and layout;
    - Max shows `boy.1`, Leo shows `boy.2` and Sophia shows `girl.1`;
    - the switcher list and Settings › Babies show the same icons.
  - Switch to Sophia and confirm all three headers follow.
  - Save the screenshots under `docs/verification/2026-10-08-baby-icons/`.

- [ ] **Step 3: Whole-branch review.** Use superpowers:requesting-code-review against `main..feat/baby-icons`. Check it against the spec and the Review Focus list above, and fix what it confirms.

- [ ] **Step 4: Merge.**
  - Check `main` for new commits and for uncommitted edits from other sessions. Rebase or merge `main` into the branch, and run `make test` and `make server-test` again.
  - Then use superpowers:finishing-a-development-branch.

- [ ] **Step 5: GATE. Deploy order needs the user's approval; this modifies an external system.**
  - **Server first, before this branch merges to `main`.** Phones are built from `main`, so migration 29 has to be live before any phone can run this build.
    - A phone on this build that syncs with a schema-28 server gets `invalid_request` for the whole request.
    - The client then isolates the icon-bearing baby mutation and everything that depends on it, and quarantines them **permanently**. Upgrading the server later does not bring them back.
    - Deploy the server to Unraid with the documented upgrade steps (memory: unraid-deploy-access).
    - Confirm `schema_compatibility.current_version = 29`.
  - **Then the phones.** Install a clean-tree build on both, and check on device:
    - existing babies get icons after launch;
    - both phones agree on them;
    - a rename on one phone keeps the icon on the other.
  - Only then ask the user to test (memory: manual-test-only-after-own-verification).
  - **No way back.** Once a phone runs this build, never install a pre-V10 build on it. The V10 store would be moved aside and its unsynced data lost.
