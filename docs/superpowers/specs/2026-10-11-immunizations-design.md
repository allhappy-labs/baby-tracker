# Immunizations — design

Date: 2026-10-11.

Related:
- [Supplements](2026-09-26-supplements-design.md): the same free-text name with reuse.
- [Entry panels](2026-10-09-entry-panels-design.md): the vaccine panel follows its Moment layout.
- [Pumping](2026-10-09-pumping-design.md): built after this; the same deploy-order rule applies.
- [Arrangeable Home buttons](2026-10-11-home-buttons-order-design.md): built before this; Vaccine joins its order.
- [Growth measurements](2026-09-30-growth-measurements-design.md): the Insights card and details screen follow its pattern.

## Intent

Parents want a record of the vaccinations each baby has had: which vaccine, when, and which dose. It should be quick to log at or after the appointment, and easy to read back as a list, the way a vaccination booklet reads.

Decisions made with the user:
- **Record only.** No vaccination schedule, no due dates, no reminders, no medical guidance.
- **The vaccine name is free text with reuse**, like supplements. No built-in vaccine list.
- **One optional extra field: the dose number.** No lot number, injection site or structured reaction; a reaction goes in the note.
- **Shots show in History and Ask, and in a per-baby Immunizations screen** opened from an Insights card.
- **Name suggestions come from the whole family**, so a name typed for one baby is offered for a sibling.
- **The dose fills itself in** as one more than the baby's highest dose of that vaccine.
- **The panel opens with no vaccine selected.**
- **The Immunizations screen groups by vaccine and has no Add button.**
- **Immunizations are built before pumping**, on main with arrangeable Home buttons landed.

Success means five things:
- A shot can be logged from More in a few taps, now or backdated, with a name, an optional dose and an optional note.
- It shows in that baby's History, PDF export and Immunizations screen on both phones in a family.
- Logging the second dose of a vaccine needs no typing: pick the name, and the dose reads 2.
- Ask can say when the last vaccination was and how many doses of a vaccine a baby has had.
- Nothing about the other trackers, Home or the timers changes.

## 1. Data

### The immunization entry

A new synced entry kind `immunization`, owned by the baby.

| Field | Type | Notes |
|---|---|---|
| `id` | UUID | |
| `baby_id` | UUID | |
| `given_at` | instant | May be backdated |
| `time_zone_id` | string | Same rules as the other entries |
| `vaccine` | text | Free text, trimmed, 1–64 characters: the supplement name rule |
| `dose` | integer, optional | 1–10 |
| `note` | text, optional | Same limits as other notes |

The usual bookkeeping applies: realm, created by, last modified, tombstone.

- **Validation:**
  - `vaccine` passes the supplement name rule (`SupplementName.normalized`): not blank after trimming, at most 64 characters and 256 bytes, no control characters. Phone and server both check it;
  - `dose`, when present, is a whole number from 1 to 10. Phone and server both check it;
  - `given_at` is not after now. Only the phone checks this, strictly, as Growth does. The server has no future check for supplements and no clock allowance for measurements, so a server check would reject "now" from a phone whose clock runs a few seconds fast;
  - `given_at` is not before the baby's birth date. The phone checks this against the calendar date in the entry's time zone, as Growth does. The server does not check it, because the birth date can be edited later; an entry that becomes pre-birth after such an edit is kept and shown.
- **Several shots at one visit** are separate entries with the same time. Nothing links them.
- **Two entries with the same vaccine and dose** are allowed. The app does not judge the record.

### Names and doses

- **Suggestions:** the distinct `vaccine` values of every undeleted immunization in the realm, across all babies, ordered by the most recent `given_at` that uses each.
- **Equality:** names equal ignoring case and surrounding whitespace are one option, and the spelling of the earliest shot is kept. The supplement normaliser and name key are reused, not copied.
- **No catalog:** a name is offered while at least one undeleted entry uses it. There is nothing to manage.
- **Next dose:** for a baby and a name, one more than the highest `dose` among that baby's undeleted entries with that name, capped at 10. With no earlier entry, or none carrying a dose, it is 1. It is a default for the panel only and is never stored on its own.

### Server

The server keeps every care record in one table, `care_entities`, with a numeric kind. Immunization is **kind 9**. There is no per-kind table.

- **Migration `000033_immunizations.sql`**, the first after `000032_baseline.sql`:
  - widens the kind CHECKs on `care_entities` and `sync_page_bindings.resume_kind` to 1–9;
  - lets kind 9 carry `started_at`, which holds `given_at`. `label` stays empty; the vaccine lives in the stored wire payload;
  - adds `immunization` to the import manifest's kind CHECK;
  - updates the two kind comments, copies the privilege manifest to version 33 and sets the compatibility row to 33.
  - The compatibility window becomes exactly 33. The pumping plan still says `000030`, written before the squash; pumping is built second and takes `000034`.
- **Protocol:** an `ImmunizationPayload` (payload version 1) with `create`, `update` and `delete`: `baby_id`, `given_at`, `time_zone_id`, `vaccine`, `dose`, `note`. `dose` and `note` are always present and may be null. It is added to every kind list, decode switch, operation matrix and the canonical hash.
- **Care rules (`internal/care`):** validation, typed columns, the action map and the baby lookup, so a shot for a baby outside the family is refused like any other record.
- **Baby deletion:** the cascade already tombstones every record of the baby; immunization gets its place in the cascade order, before timers.
- **Snapshot and journal:** generic over kinds once the kind is named. Immunizations page after measurements.
- **Export:** the JSON backup gains an `immunizations` section after `measurements`. The legacy export golden is not rewritten; its test allows the one new, empty section.
- **Import:** the manifest accepts the kind.
- **Quotas:** counted like other baby-owned records, with no code change.
- **Not included:** the read-only MCP (`internal/familyread`, `internal/mcpapi`) keeps its five kinds. Immunizations do not appear in its history or summary.

### Phone

- **Model:** an `ImmunizationEntry` SwiftData model, `ChangeLoggable` and `BabyOwnedRecord`.
- **Schema:** `SchemaV11`, with `SchemaV10` frozen and a lightweight V10→V11 stage. Pumping then takes `SchemaV12`.
- **Sync:** added to the wire kind, mutation factory, content hash, remote apply, realm moves and wipes, and the import capture. Nothing changes on the phone for export; the server builds the archive.
- **Store (`BabyStore+Immunizations`):**
  - `logImmunization` and `editImmunization`; delete is the store's generic soft delete;
  - `immunizations(for babyID)` for the screen and the card;
  - `vaccineNames()` for the realm's suggestions;
  - `nextDose(of:for:)`.
  - The per-baby History and entry queries include immunizations. `EntryRef` gains `.immunization`.

### Deploy order

Server first (the migration and the new binary together: the window is exactly 33), then the merge, then **both phones**, and only then the first shot.

- A server on schema 32 rejects a request carrying an `immunization` mutation as `invalid_request`, and the client then quarantines those mutations for good.
- A phone on the earlier build cannot skip the new kind: the server never marks a change optional. Once the family has one shot, that phone stops syncing and asks for an update. So no shot is logged until every phone in the family runs this build.

## 2. Logging

- **Start:** Vaccine is a Home action in the [arrangeable order](2026-10-11-home-buttons-order-design.md): a new `HomeActionKind` `vaccine`, added to the end of the default order. The order's reading rule appends it for every baby with a stored order, so it starts in More (···) for everyone. Like any action it can be dragged onto the dock in Settings → Home buttons. A tap opens the vaccine panel in every position.
- **Quick Log widget:** if Vaccine is moved high enough to reach the widget, its slot opens the app at the vaccine panel, as Solids, Meds and Growth do.
- **While a timer runs** Vaccine conflicts with nothing and stays available.
- **A past shot** is logged in the same panel by changing its time. Past entry is unchanged: it offers Breast, Bottle and Sleep only, as today.

### Vaccine panel

The panel follows the [entry panels](2026-10-09-entry-panels-design.md) layout as a **Moment** activity. One panel logs and edits.

- **Vaccine:**
  - with no saved names, a text field that invites the first one;
  - with saved names, a picker of the family's names with a "Choose vaccine" placeholder, and a **+** that reveals the text field for a new one. The picker shows even with one saved name, so that name never looks pre-selected;
  - a new panel opens with nothing selected.
- **Dose:** a row with − and +, and a "—" state for no dose. Choosing or typing a vaccine sets it to the next dose for this baby. Changing it by hand stops it following the name for the rest of that panel. In edit mode it shows the stored value and never follows the name.
- **Time:** one date and time row, defaulting to now.
- **Details:** the note, collapsed unless it holds text.
- **Footer:** Save, plus Delete when editing.
- **Validation:** Save is disabled without a name. A time in the future or before the birth date shows the panel's inline message and disables Save.
- **Saving a typed name** that equals an existing one, ignoring case and whitespace, stores the existing spelling.
- Each Save is one atomic write, so a failed save cannot leave a duplicate.

## 3. Where shots show

- **History:**
  - A row reads, for example, "10:15 · vaccine icon · Prevenar 13 · dose 2", or the name alone when there is no dose.
  - The tracker filter gets **Vaccine**.
  - Tapping a row opens the vaccine panel in edit mode. Delete soft-deletes the shot.
  - The day header's totals do not change.
- **PDF export:** shots appear in each day's full log, because the PDF includes every tracker and uses the same day builder. The day totals do not change.
- **Insights card — Immunizations:**
  - It shows the latest shot, for example "Prevenar 13 · dose 2 · 3 Sep", and an **Open immunizations** row with the number of shots.
  - With no shots it reads "No vaccinations logged yet" and has no row to open.
  - It covers all time and ignores the Insights range picker.
- **Immunizations screen**, per baby, pushed from the card inside Insights:
  - One section per vaccine, ordered by the most recent shot in each. Within a section the rows run oldest first.
  - A row reads "Dose 2 · 3 Sep 2026 · 4mo": the dose, the date in the entry's time zone, and the baby's age that day in the short form Growth uses (days under two weeks, weeks under thirteen, months under two years, then years). A row without a dose shows the date and age. A shot dated before the birth date shows no age.
  - Tapping a row opens the vaccine panel in edit mode, over the screen. The screen updates when the panel saves or deletes, and when the other phone's change syncs.
  - There is no Add button; logging has one route.
- **Ask:**
  - `vaccine` joins the kinds Ask's existing reads accept: entries, latest, and totals as a count shown only when there is one.
  - An entries read for `vaccine` covers the baby's whole life whatever range the model names, because Ask's ranges stop at 30 days and shots are months apart. That is how "how many Prevenar doses has she had?" gets its answer.
  - An entry carries the vaccine, the dose and the note.
  - Ask's sources link to the History row.
  - Ask gets no new tool and no keyword routing. The model picks the read, as with every other kind.
- **Not included:** Home's Today card and hero, Live Activities, reminders, a vaccination schedule, the server's read-only MCP, and the backup restore (restore does not exist yet).

## 4. Look and copy

- **Icon:** a new line-style syringe glyph in `Icons.xcassets` (`BabyIcon.vaccine`), drawn in the tracker icon style.
- **Colour:** a `Tracker.vaccine` entry with its own `Palette` colour, distinct from supplements and growth.
- **Copy:** all copy goes into the String Catalogs for the six existing languages, and must fit Ukrainian at the largest accessibility text size. The tile reads "Vaccine"; the card and screen read "Immunizations".
- The app states facts only. It never says a vaccine is due, missing or complete.

## 5. Testing

- **Store tests:**
  - create, edit and soft delete;
  - a blank name is refused; a name over 64 characters is refused; a dose outside 1–10 is refused;
  - a future time and a time before the birth date are refused;
  - name suggestions come from every baby in the realm, most recent first, merged ignoring case and whitespace with the first spelling kept;
  - a name drops out of the suggestions when its last entry is deleted;
  - the next dose for none, one and several earlier doses, for entries without a dose, per baby, and at the cap;
  - deleting a baby in a shared family takes its shots;
  - shots appear in the baby's History query and not in another baby's.
- **Sync tests:**
  - the wire payload shape;
  - remote apply create/update/delete;
  - the content hash.
- **Schema:** a store on the earlier schema opens in place under the new one.
- **Server (Go):**
  - validation bounds;
  - the baby must be in the same family;
  - the baby-delete cascade tombstones shots;
  - snapshot, journal and export include shots;
  - the migration over a schema-32 database, the kind CHECKs, and the catalog golden for 33.
- **Protocol end-to-end:** two phones; one logs a shot, the other edits its dose; both phones and a fresh snapshot agree.
- **Panel model tests:** nothing selected on open; the dose follows the name until changed by hand; edit mode keeps the stored dose; Save disabled states.
- **Presentation tests:** the History row with and without a dose; the card's empty, one-shot and many-shot states; the screen's grouping, ordering and age text.
- **Ask:** shots and the latest shot come back through the existing reads; a shot older than 30 days is still listed.
- **UI tests (a small subset):** log from More with a new name; log a second dose by picking the name and see "dose 2"; the History row; the card opens the screen. Each asserts logic, never layout geometry.
- **Home order:** an order stored before this version reads with `vaccine` appended; the default order ends with it; More shows the Vaccine tile.
- **Simulator screenshots:** the More sheet, the panel, the History row, the Insights card and the Immunizations screen, in English and in Ukrainian at the largest text size.
