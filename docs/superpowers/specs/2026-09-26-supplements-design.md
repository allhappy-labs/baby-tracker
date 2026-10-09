# Supplements design

## Intent

Parents can log a named supplement for a baby beside Feed, Sleep, and Diaper. Names are free text so each family can track what matters to them. Repeating a supplement should take few taps, and the last logged name should be ready when the sheet opens.

## Interaction

- The Home quick log dock has a Supplements button. It opens a sheet, even when there is no saved supplement.
- The sheet always has a plus button. Plus reveals a free text field for a new name. Saving a new name logs that supplement at the chosen time and makes it available for reuse.
- With no names, the sheet invites the first addition. With one name, show it as plain text and a Log button, without a picker. With two or more names, show a picker and a Log button.
- Opening the sheet selects the most recently logged supplement for this baby. The user can change the selection before logging.
- Trim names and reject blank input. Treat names equal ignoring case and surrounding whitespace as the same option for the baby, retaining the first saved spelling. Prevent a duplicate log from a failed save by making each Save one atomic write.
- History shows each log with its name and time and supports editing its time/name and deleting it, following existing entry behavior. A name remains reusable while at least one nondeleted log uses it. This is the small scope interpretation of a user-created list; independent catalog management is outside this feature.

## Data and sync

- A supplement log is a baby-owned, dated `SupplementEntry` with a free-text `name`, timezone, optional note, attribution, tombstone, and sync metadata consistent with other entries. It also carries an immutable `logged_at` timestamp for the actual logging action, separate from the editable `started_at` event time. That timestamp survives sync acknowledgement, edits, imports, and fresh snapshots so all clients select the same last-used option.
- It participates in the existing local SwiftData schema, change log, import, realm move, deletion, remote apply, history, and family sync protocol. Server storage, validation, snapshots, import, quotas, and child deletion treat it like the other baby-owned entries.
- New entity kind `supplement` uses payload version 1. New clients must not silently drop supplement records from snapshots or sync pages. Existing clients that do not know the entity require the server's established compatibility handling; expose a protocol/schema version boundary if necessary.

## Verification

- Store tests cover create, blank/duplicate names, option ordering, last used selection, editing, soft delete, migration and remote apply.
- Server tests cover protocol validation, mutation, snapshot, and deletion. A live local server sync test demonstrates a log on one client reaching another.
- UI and model tests cover zero, one and multiple options, plus visibility, default selection, save failure, and history rendering. Run Swift package tests, Go tests, and an iOS simulator build.
