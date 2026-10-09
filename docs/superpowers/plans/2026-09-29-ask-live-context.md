# Ask Live and Recent Context Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Give Ask fresh running activity, a usable 48-hour timeline, and bounded older-history retrieval.

**Architecture:** `AskContextBuilder` assembles source-linked local facts; `AskPrompt` selects a budgeted context with explicit coverage; `AskLogTool` supplies older windows. `AppleAskModel` routes present-state questions to records and verifies all generated claims.

**Tech Stack:** Swift 6, SwiftData, Foundation Models, Swift Testing.

**Spec:** `docs/superpowers/specs/2026-09-29-ask-live-context.md`

## Global Constraints

- Selected child only; device-local reads only.
- Running timer state is logged state, not physical observation.
- Preserve general parenting questions and saved-chat compatibility.
- Keep prompt within the model's measured usable context and identify omitted coverage.

## Review Focus

- Multiple active timers: report each, without choosing one as the whole state.
- Feed step changes: use the current breast or bottle step and its own start time.
- Busy 48-hour history: disclose truncation and retrieve missing details.
- Calendar and rolling windows: preserve the selected time zone and half-open bounds.
- Deleted or other-child records: never appear in context or answers.

---

### Task 1: Fresh live and recent facts

**Files:** `AskContextBuilder.swift`, `AskFactBuilder.swift`, Ask context tests.

- [x] Write failing tests for all running timers, current feed step, 48-hour boundaries, and selected-child isolation.
- [x] Run focused tests and confirm the missing facts fail.
- [x] Add source-linked live and recent facts, preserving exact aggregate calculations.
- [x] Run focused tests until green.

### Task 2: Prompt routing and budget

**Files:** `AskModel.swift`, `AskPrompt.swift`, Ask model tests.

- [x] Write failing tests for current-state routing, live-fact priority, and visible coverage after truncation.
- [x] Run focused tests and confirm failures.
- [x] Build a budgeted prompt that always includes live state and uses recent records while capacity remains.
- [x] Run focused tests until green.

### Task 3: Older local history

**Files:** `AskLogQuery.swift`, `AskLogTool.swift`, Ask log tests.

- [x] Write failing tests for explicit older date ranges and invalid bounds.
- [x] Run focused tests and confirm failures.
- [x] Extend the read-only local tool and question routing for those ranges.
- [x] Run focused tests until green.

### Task 4: Verify and review

- [x] Run the full Swift package suite and iOS simulator build.
- [x] Review the final diff for source linking, privacy, current-state claims, and prompt limits.
- [x] Record verification and remaining device-model limitations in `TASKS.md`.
