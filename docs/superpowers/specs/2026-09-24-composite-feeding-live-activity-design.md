# Composite Feeding and Live Activity — Design Spec

**Date:** 2026-09-24  
**Status:** Approved for implementation  
**Amends:** `2026-09-19-baby-tracker-v1_1-design.md` feeding and Live Activity sections

## Purpose

A real feeding may contain several ordered parts: left breast, right breast,
pumped breast milk, and formula. Baby Tracker currently stores breast intervals
plus at most one bottle, so a second bottle overwrites the first. The running
screen also stops refreshing after some navigation/lifecycle paths, labels a
side only as “Left” or “Right”, draws the stop action as two nested controls,
and renders a generic feeding Live Activity whose text is not reliably legible
in dark mode.

This change makes the real feeding session the domain object parents see and
edit. Its components remain ordered and individually meaningful.

## Product contract

- One feeding session contains one or more ordered components.
- Component kinds are breast, breast-milk bottle, formula bottle, mixed bottle,
  and solids. Breast components retain side and duration; bottle components
  retain volume and contents; solids retain foods and amount.
- The common flow may be `Left breast -> Right breast -> Breast milk bottle ->
  Formula bottle`. Adding a later component never overwrites an earlier one.
- Components are shown in order in History and can be edited or removed
  separately. Removing the last component removes the empty session after the
  existing destructive confirmation.
- Legacy feeds migrate without semantic loss: side intervals keep their order;
  a legacy bottle follows the breast intervals; standalone bottles and solids
  become one-component sessions.
- Daily feed count counts sessions, not components. Bottle-volume totals sum all
  bottle components. Breast-duration totals sum all breast components.

## Domain model

`FeedEntry` remains the stable session identity and synchronization entity.
Replace its parallel feeding fields with an ordered `[FeedComponent]` value.
`FeedComponent` has a stable UUID so an editor can address one component without
using a fragile array index. It is a Codable value with these exact cases:

```swift
public enum FeedComponent: Codable, Sendable, Equatable, Identifiable {
    case breast(id: UUID, side: BreastSide, seconds: Int)
    case bottle(id: UUID, volumeML: Double, contents: BottleContents)
    case solids(id: UUID, foods: [String], amount: SolidsAmount)
}
```

The active timer stores completed components plus an optional running breast
component. Saving a bottle closes the running breast interval, appends the
bottle, and leaves the overall feeding session open so another bottle can be
added. Starting or switching to a breast side opens a new timed breast
component. Stopping commits the ordered components to one `FeedEntry`.

Compatibility accessors such as `sides`, `totalBreastSeconds`, `volumeML`, and
`contents` may remain as computed read-only projections while call sites are
migrated. No writer may collapse multiple bottles through those projections.

## Persistence and synchronization

- Add SwiftData schema V3 and migrate V2 feeds/timers to component arrays.
- The wire protocol gains payload version 2 for feed and timer payloads with an
  ordered `components` array. Version 1 remains readable during migration and
  is canonically upgraded; new clients emit version 2.
- Add a deployable server compatibility migration before server code consumes
  the new shape. Store components in a child table keyed by family, feed/timer,
  position, and component UUID. Validate per-kind required/forbidden fields.
- Snapshot, import, canonical hashing, receipts, timer reducers, usage charging,
  export, deletion, and overlap tests all use the same ordered representation.
- Old app/server overlap must either preserve the V1 representation exactly or
  return the existing explicit unsupported-version error; it must never discard
  a second bottle.

## Running-screen behavior

- The main elapsed value is always derived from `startedAt` and the current
  clock; it is never accumulated in view state.
- Home reconnects the correct tick publisher on appearance and whenever the
  scene becomes active. It refreshes immediately before reconnecting.
- The hero numeral grows from 68pt to 82pt relative to `.largeTitle`, retains
  monospaced digits, Dynamic Type scaling, one-line scaling, and VoiceOver copy.
- Running breast copy is exactly `Feeding · Left breast` or
  `Feeding · Right breast`. A session currently between timed breast components
  reads `Feeding · Bottle` plus the most recently appended bottle detail.
- The stop action is one Liquid Glass surface. Its button style adds no second
  material, fill, stroke, or clipping shape inside the glass container.

## Live Activity

The ActivityKit state carries the current component presentation rather than a
generic feed label:

```swift
public enum FeedActivityKind: String, Codable, Sendable {
    case breastLeft, breastRight, breastMilkBottle, formulaBottle, mixedBottle, sleep
}
```

The activity derives label, tint, and SF Symbol from that enum in one shared
mapping used by Lock Screen and every Dynamic Island size. Breast, breast-milk
bottle, formula bottle, mixed bottle, and sleep must not all share one icon.
The state updates after switching side and after every bottle append.

The Live Activity uses explicit high-contrast foreground colors against its
fixed dark background, including the elapsed timer, secondary label, icons,
and system-action foreground. It must not rely on environment `.primary` or
`.secondary` resolving suitably inside ActivityKit.

## Error and concurrency behavior

- Failed component appends leave the sheet open and preserve entered values.
- Concurrent caregiver changes retain server ordering and normal receipt rules.
- A stale bottle sheet that outlives the timer logs a separate one-component
  bottle session, preserving the milk consumed.
- A zero-duration breast component is not persisted. A zero-volume bottle is
  rejected by the existing sheet rule.
- Stopping an empty session is refused rather than creating a blank feed.

## Acceptance

- Unit and protocol tests cover migration, multiple bottles, ordering, editing,
  aggregates, canonical bytes, and old/new overlap.
- A lifecycle UI test backgrounds and reactivates the app, then proves the
  displayed elapsed time advances from stored timestamps.
- Manual device acceptance reproduces the supplied screen, confirms one stop
  border, larger numerals, explicit breast labels, readable dark-mode Live
  Activity, distinct icons, and the full four-component feeding flow.

