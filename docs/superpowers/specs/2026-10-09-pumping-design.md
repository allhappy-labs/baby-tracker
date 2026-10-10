# Pumping — design

Date: 2026-10-09. Part 1 of the pumping request. The Lock Screen card on the *other* phone (server push) is deliberately out of scope, and the user chose to leave it out for now.

Related:
- [Baby icons and shared switcher](2026-10-08-baby-icons-switcher-design.md): the same deploy-order rule applies.
- [Entry panels](2026-10-09-entry-panels-design.md): the pump panel follows its layout.

## Intent

A parent pumps milk, sometimes while the other parent gives a bottle. The app should time a pumping session, record the side and the amount, and show the session alongside the babies' care.

Decisions made with the user:
- **Pumping belongs to the family, not a baby.** One session shows on every baby's screens. On a phone that is not in a shared family, "family" means all babies on the phone.
- **Side:** Left, Right or Both, with one timer and one total amount.
- **No milk stash.** Pumping only logs sessions; bottles stay as they are.
- **Started from the More (…) sheet** on Home. The dock is unchanged.
- **It runs at the same time as a feed or sleep timer.** A bottle and pumping run side by side.
- **A Lock Screen card on the phone that started the session.** The other phone sees the session in the app after it syncs.
- **Finished sessions show** in every baby's History, in Home's Today card, and in Ask's answers. They do not show in Insights.

Success means five things:
- A pumping session can be started, stopped and given an amount while a bottle runs.
- It shows on every baby's Home and History on both phones in a family.
- Deleting a baby never deletes pumping history.
- Ask can say how much was pumped and when the last session was.
- Nothing about feeds, sleeps or their timers changes.

## 1. Data

### The pump entry

A new synced entry kind `pump`, owned by the family. It has no `baby_id`.

| Field | Type | Notes |
|---|---|---|
| `id` | UUID | |
| `started_at` | instant | |
| `ended_at` | instant, optional | Empty while the session runs |
| `time_zone_id` | string | Same rules as the other entries |
| `side` | `left`, `right` or `both` | |
| `volume_ml` | decimal, optional | 0–1000. Stays empty until entered, and may stay empty for good |
| `note` | text, optional | Same limits as other notes |

The usual bookkeeping applies: realm, created by, last modified, tombstone.

- **Running** means the entry is not tombstoned and `ended_at` is empty. It is an ordinary entry. It is not an `ActiveTimer`.
  - The timer system assumes a baby at every level: the database, the stop-to-entry conversion and the Lock Screen push. A family-scoped timer would touch all of that.
  - An open-ended entry has precedent: an open sleep.
- **At most one running pump per family.**
  - On the server, a unique partial index `(family_id) WHERE ended_at IS NULL AND tombstoned_at IS NULL` enforces it. A second start that loses the race is rejected as superseded, the same way a duplicate timer start is.
  - The client checks for a running pump before it creates one.
- **Validation:**
  - `side` must be in the set;
  - `volume_ml` is between 0 and 1000 and limited to 2 decimals;
  - `ended_at` is not before `started_at`;
  - a session lasts at most 6 hours;
  - `started_at` is not in the future (the same clock-skew allowance as other entries).

### Server

- **Migration `000030_pumping.sql`** adds `public.pump_entries`. It follows the supplements/growth pattern: standard meta columns, `PRIMARY KEY (family_id, id)`, the `last_sequence` FK, RLS `family_context`, grants and privilege-manifest rows, and a compatibility bump to 30. The entity-kind CHECKs on the journal, snapshot and import manifests are re-created to include `pump`. There is **no** baby FK.
- **Protocol:** a `PumpPayload` (payload version 1) with `create`, `update` and `delete`. It is added to every kind list, decode switch and operation matrix.
- **Records:** a `pumps.go` like `supplements.go`. The baby-required guard in `Repository.Apply` skips `pump`.
- **Baby deletion:** the child-delete cascade does **not** include pumps.
- **Snapshot, journal, export:** `pump` is included, so the JSON backup carries pumps.
- **Import:** pumps are new, so a legacy import has none. The manifest code must not assume every non-baby kind has a `baby_id`.

### Phone

- **Model:** a `PumpEntry` SwiftData model.
- **Schema:** `SchemaV11` keeps `SchemaV10` frozen where models change, with a lightweight V10→V11 stage.
- **Sync:** `PumpEntry` is ChangeLoggable but **not** `BabyOwnedRecord`. It is added to the wire kind, mutation factory, content hash, remote apply, realm moves and wipes, and the import capture. Every place that drops rows without a `baby_id` must keep pumps.
- **Queries:**
  - `pumps(in range)` reads the whole realm;
  - `runningPump()` returns the open pump, if there is one.
  - The per-baby History and Home queries merge realm pumps into their results.

### Deploy order

Server first (migration 30), then the merge, then the phones. A schema-29 server rejects a request carrying a `pump` mutation as `invalid_request`, and the client then quarantines those mutations for good. This is the same rule as for baby icons.

## 2. Starting, running and stopping

- **Start:**
  - More (…) → **Pump** opens a small panel: **Left / Right / Both**, then **Start**.
  - The panel offers only **Edit running** when a pump is already running, whichever phone started it.
- **While it runs:**
  - Every baby's Home shows a compact **pump strip** above the dock: pump icon, "Pumping · Both · 12:34" (elapsed, live), and **Stop**.
  - The hero keeps showing any running feed or sleep.
  - The strip is a separate view, so feed or sleep controls never change because a pump is running.
  - Tapping the strip (not Stop) opens the pump panel in editing mode: side, Start, End reading "Running", and note.
- **Stop:**
  - Stop ends the session at the current moment and opens **How much?**.
  - **How much?** uses the bottle amount control (`BottleVolumeField`) in the user's ml/oz setting, with **Save** and **Skip**.
  - Skip leaves the amount empty.
- **Lock Screen card:**
  - The phone that starts the session starts a local Live Activity for it, separate from the feed and sleep cards, so a bottle card and a pump card can sit side by side.
  - The card shows the pump icon, "Pumping · Both", elapsed time, and **Stop**.
  - Stop on the card ends the session through an App Intent, the same way the bottle card's Stop does. The app asks **How much?** the next time it opens, using the existing pending-amount path.
  - The card ends whenever the session ends, from either phone, once this phone learns of it. It also ends after the usual staleness limit.
- **The other phone** sees the strip after its next sync and can stop the session or edit it there. It gets no card; that would need server push, which this Part 1 leaves out.

### Pump panel

The pump panel follows the [entry panels](2026-10-09-entry-panels-design.md) layout as an **Interval** activity:
- the activity's own fields: side, and the amount (shown once the session has ended);
- Start and End (End reads "Running" while the session is live);
- Details for the note;
- Save, plus Delete when editing.

The same panel logs a past session (More → Past entry → Pump) and edits one from History.

If entry panels has not landed when this is built, the pump panel uses the same building blocks it specifies (`EditTimeRow`, fitted height), so it needs no rework later.

## 3. Where finished sessions show

- **History:**
  - Every baby's History lists pumps in time order with its other entries, for example "08:00 · pump icon · 20min • 90 ml · Both".
  - A running pump reads "Pumping…".
  - A pump with no amount shows "—".
  - The tracker filter gets **Pump**.
  - Tapping a row opens the pump panel. Delete soft-deletes the session.
  - Pumps appear in every baby's History. They are the same rows, not copies.
- **Home Today card:** a pump item with today's pumped total in the user's unit and "last 2h 27m ago". It counts finished sessions only, and is the same on every baby's Home. Sessions without an amount count toward "last" but add nothing to the total.
- **Ask:**
  - `pump` joins the kinds Ask's existing reads accept: totals (amount and count), entries, and latest.
  - A running pump appears in the session header with the running timers.
  - Ask's sources link to the History row.
  - Ask gets no new tool and no keyword routing. The model picks the read, as with every other kind.
- **Not included:** Insights charts and the backup restore (restore does not exist yet).

## 4. Look and copy

- **Icon:** a new line-style pump glyph in `Icons.xcassets` (`BabyIcon.pump`), drawn in the tracker icon style.
- **Colour:** a `Tracker.pump` entry with its own `Palette` colour, distinct from feed and bottle.
- **Copy:** all copy goes into the String Catalogs for the six existing languages, and must fit Ukrainian at the largest accessibility text size.

## 5. Testing

- **Store tests:**
  - starting a pump while one runs is refused;
  - a pump runs alongside a feed and a sleep timer;
  - stopping sets the end time;
  - the amount can be empty;
  - edit and delete;
  - deleting a baby leaves pumps;
  - pumps appear in every baby's History and Home queries;
  - Today's total and last-session values.
- **Sync tests:**
  - the wire payload shape;
  - remote apply create/update/delete;
  - the content hash.
- **Schema:** a V10 store opens in place under V11.
- **Server (Go):**
  - validation bounds;
  - one running pump per family, with the race loser superseded;
  - a pump survives a baby delete;
  - snapshot, journal and export include pumps;
  - the migration and the kind CHECKs.
- **Protocol end-to-end:** two phones; one runs a bottle while the other starts and stops a pump with an amount; both phones and a fresh snapshot agree.
- **Ask:** pump totals and the latest session come back through the existing reads.
- **UI tests (a small subset):** start from More, the strip appears next to a running bottle, Stop then How much? then Save, and the History row. Each asserts logic, never layout geometry.
- **Simulator screenshots:** the strip alongside a running bottle, the Lock Screen card, the History row, and the Today card.
