# Timed Bottle and Sleep End Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans to implement this plan task by task. Steps use checkbox syntax for tracking.

**Goal:** Correct completed sleep end times and time bottle segments within a feed.

**Architecture:** Reuse the existing sleep record update path. Extend feed/timer component protocol to v3 with a timed bottle phase and optional bottle seconds, preserving legacy untimed bottles. Home drives start/finish intents through the sync-aware timer reducer.

**Tech Stack:** Swift 6.2, SwiftUI, SwiftData, Go, PostgreSQL, XCTest and Swift Testing.

**Spec:** `docs/superpowers/specs/2026-09-29-timed-bottle-sleep-end.md`

## Global constraints

- Preserve v1/v2 data and reject old clients before advancing a cursor past v3 state.
- No installed physical app launch during verification.
- Keep production server deployment and protocol-dependent device installation separate from local verification.

## Tasks

- [ ] Independent sleep end editor, validation, sync update, and focused tests.
- [ ] v3 server schema, wire validation, timer start/finish bottle mutations, readback, compatibility gate, and disposable database tests.
- [ ] Swift v3 bottle component wire/store compatibility and focused tests.
- [ ] Swift timer state, reducer, remote apply, Home/Live Activity and logging flows, with focused unit and simulator UI tests.
- [ ] Whole-branch review, full package/server/build gates, integration to local main, and delivery decision.

## Review focus

- A bottle can start alone, between breast segments, and after a previous bottle.
- A stale or simultaneous stop/finish on another caregiver's device does not create a duplicate feed or lose duration.
- An unfinished bottle cannot silently disappear when Stop is tapped.
- Existing untimed bottles and older clients retain explicit compatibility behavior.
- Sleep end edits across midnight and daylight saving time preserve the selected actual instant and reject invalid ranges.
