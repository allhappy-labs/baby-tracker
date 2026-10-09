# Baby icons and one shared switcher — design

Date: 2026-10-08. First of two sub-projects from the same request. The second, a pumping timer with amount, gets its own spec later.

## Intent

Today a baby appears in the app only as a name. Parents with more than one baby want to tell them apart at a glance:
- **Every baby gets an icon.** There is a boy set and a girl set. Two babies of the same sex, such as twin boys, never share an icon while the set has room.
- **One switcher.** Home, History and Insights each draw their own copy of the "name + chevron" switcher, and the copies differ in size. They become one component that looks and behaves the same on all three.

Decisions made with the user:
- **The icon is assigned automatically.** There is no picker.
- **The icon is stored per baby and synced.** It does not shift when a sibling is deleted or the babies are reordered.
- **Each set has 4 icons.** A 5th baby of the same sex reuses an icon.
- **The switcher appears only where one already exists.** Ask and Settings get no switcher.

  At the time of that answer only Home and History had one. Insights has since gained a third copy on `main` (`InsightsView.swift:115`). It is replaced too, because the goal is one switcher everywhere it appears.

Success means four things:
- Every baby whose sex is known shows an icon from the right set, in the switcher header, the switcher list and Settings › Babies.
- Same-sex siblings get different icons, up to 4.
- Both phones in a shared family show the same icon for the same baby.
- The Home, History and Insights headers are the same view.

## 1. Data

### Icon id

The icon id is a string `"<set>.<n>"`, where the set is `boy` or `girl` and `n` is 1–4. That gives exactly 8 valid values. A baby with no icon has none.

- **iOS:** `Baby` gains `iconID: String?`. Swift exposes a `BabyAvatar` enum in `BabyCore` with the 8 cases plus `.neutral` for drawing. A `BabyAvatar(iconID:)` that cannot parse the id returns `.neutral`.
- **Server:** migration `000029_baby_icon.sql` adds a nullable `icon text` column to `babies`, with a `CHECK` limiting it to the 8 values.
  - `protocol.BabyPayload` gains `Icon *string \`json:"icon,omitempty"\``.
  - Validation rejects any other value.
  - The Swift wire model for the baby payload gains the same optional field.

### Older app versions keep the icon

`putBaby` replaces the whole row today, so a phone on an older build that renames a baby would erase its icon. The upsert writes `icon = COALESCE(EXCLUDED.icon, babies.icon)`. An edit without an icon keeps the stored one.

The trade-off is that a client cannot clear an icon. Nothing in this design needs to.

## 2. Assignment

Assignment is a pure function in `BabyCore`, so it can be unit-tested:

```swift
static func assign(sex: GrowthReferenceSex, siblings: [String?]) -> String
```

`siblings` holds the icon ids of the other living (not tombstoned) babies in the same realm. The function returns the lowest-numbered icon of the set that no sibling uses. If all 4 are in use, it returns the one used by the fewest siblings, and the lowest number wins a tie.

The function is called in four places:
- **Add baby.** Boys/Girls is already required, so a new baby always gets an icon.
- **Edit baby, Boys/Girls changed.** The baby gets a fresh pick from the new set, ignoring its own current icon.
- **Edit baby, Boys/Girls set for the first time** on an older baby. It is assigned the same way.
- **Existing babies.** On launch, and after a remote apply, the app fills in any baby that has Boys/Girls in its `GrowthProfile` but no icon.
  - It works in a fixed order: `sortOrder`, then `id`. Each baby's pick counts the icons already given to the babies before it.
  - Two phones doing this at the same time reach the same result. The writes go through the normal edit path, so they sync.
  - A shared-realm baby is written only when the current account may edit it.

Babies with no Boys/Girls keep no icon and draw `.neutral`.

There is a known gap. Two phones that are both offline and each add a boy at the same moment can pick the same icon. Nothing repairs this automatically. Changing Boys/Girls back and forth reassigns the icon, which is acceptable for something this rare.

### The launch fill-in

The fill-in runs before the first pull, so the local row can be older than the server's.
- **Server copy first.** A baby with no icon takes the icon in its server copy (`CanonicalEntityBase`) when that icon is in its set. An older build kept the other phone's icon only there. This is server state, so it is written without a change or an outbox row, and it may run at any time.
- **In a family, assign only after a sync.** Recording an icon sends the whole baby (name, birth date, order, icon), and the server takes the last writer. A stale row would revert a rename the other phone made. So in a shared realm the remaining babies are assigned only once a sync has succeeded since launch, and the fill-in runs again when that happens. The local realm assigns straight away.
- **Wrong set.** An icon outside the set for the baby's current Boys/Girls counts as missing. It takes the server copy's icon if that one fits, and is otherwise reassigned under the same rule. Saving the baby with Boys/Girls unchanged also replaces it.

### Importing into a family

The server copy of an imported baby has no icon: the import resolver publishes a baby with its name, birth date and order only (`importer/conflict.go`).
- The owner keeps the icons already on its rows.
- A joiner fills in after its first sync, in (`sortOrder`, `id`) order. This can change some of the owner's icons once.
- It converges, because the last write wins and both phones then hold the same icons. No data is lost.

## 3. Art

There are 8 vector icons plus a neutral one, in `Icons.xcassets`. They are template images like the tracker glyphs, but heavier: a 2.2-weight head outline with ears, a simple face, and **filled** hair or accessory shapes, so each identity reads as a silhouette at 28–32 pt. Boys and girls are told apart by hair and accessories, not by the face. No icon uses partial opacity.

The set the user approved on 2026-10-08 (redraw v3, after an independent visual review):

| Set | 1 | 2 | 3 | 4 |
|---|---|---|---|---|
| boy | curl, `#6FB2FF` | spiky fringe, `#4FD8B0` | cap with brim, `#FFD84D` | pacifier, `#8A84FF` |
| girl | big bow, `#FF86B8` | pigtail puffs, `#FFB35C` | headband with a flower, `#D89BFF` | top-knot bun, `#FF6B6B` |

Each variant has its own tint in `Palette`, readable on the dark ground. Same-sex siblings therefore differ by both shape and colour.

The neutral face has newborn wisps and no accessory, and uses the secondary text colour (`#9AA4C9`).

The approved files are `mockups/2026-10-08-baby-icons/svg/<id>.svg`. The asset catalog uses them unchanged.

`BabyAvatarView(avatar:size:)` in `BabyUI` draws an icon at a given size and has no accessibility label of its own. The baby's name next to it carries the meaning.

## 4. Shared switcher

`BabySwitcherHeader(babyName:avatar:onSwitchBaby:)` in `BabyUI` draws `[icon] Name ⌄`:
- The icon is 32 pt.
- The name uses `.title2.weight(.bold)`, the size History and Insights use today. It wraps to 2 lines and scales down to 0.8.
- The tap area is at least 44 pt tall and uses `.buttonStyle(.plain)`.
- The accessibility label is "Switch baby" and the value is the baby's name.
- The caller passes the accessibility identifier, so the existing `home.babySwitcher`, `history.babySwitcher` and `insights.babySwitcher` ids stay.
- With no `onSwitchBaby` (one baby, or the switch is not available), the header shows the icon and name as a plain heading. The existing `*.babyName` header ids stay too.

The header replaces:
- the header button in `HomeView.swift:187`. Home's name shrinks from large title to `title2`, to match the other two.
- `name` in `HistoryView.swift:269`;
- `name` in `InsightsView.swift:115`.

The age shown opposite the name on Home stays where it is.

Other places that show babies:
- **Switcher list** (`BabySwitcherSheet`). Each row shows a 28 pt icon before the name. The checkmark stays.
- **Settings › Babies** (`SettingsView.swift:76`). Each row shows a 28 pt icon before the name. Rows still open Edit baby.
- **Not changed:** Ask, Settings' title, widgets and the Live Activity.

## 5. Testing

- **`BabyCore` unit tests** for `assign`:
  - an empty family;
  - twins;
  - a 5th same-sex baby;
  - a tie on the least-used icon;
  - a Boys/Girls change;
  - filling in existing babies in a fixed order, including a gap left by a deleted sibling.
- **`BabyStore` tests:**
  - add baby stores an icon;
  - a Boys/Girls edit reassigns it;
  - filling in existing babies is idempotent;
  - the icon survives a sync round trip.
- **Server Go tests:**
  - the migration applies;
  - an invalid icon is rejected;
  - a put without `icon` keeps the stored value;
  - a put with `icon` replaces it.
- **UI tests:** only the existing switcher tests for Home, History and Insights, updated to the shared header. No new UI tests.
- **Manual check on the shared simulator:** two boys and a girl show three different icons, and all three headers look the same.
