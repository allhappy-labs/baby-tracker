# Ask Conversation Redesign Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans to implement this plan task by task. Steps use checkbox syntax for tracking.

**Goal:** Replace rigid Ask responses with useful grounded conversation and a readable chat screen.

**Architecture:** A child-scoped context builder reads current records and computes compact recent/trend facts. The on-device adapter generates a structured, fact-referenced answer or labeled general answer. The validator substitutes Swift-owned values and keeps only verified source references. The view model owns pending and error state, and the SwiftUI view renders an accessible chat transcript.

**Tech Stack:** Swift 6.2, SwiftUI, FoundationModels, SwiftData, Swift Testing, XCTest UI.

**Spec:** `docs/superpowers/specs/2026-09-29-ask-conversation-redesign.md`

## Global constraints

- iOS 26 minimum; keep macOS package tests working.
- Chats remain device-only and child/account isolated; New Chat deletes the current chat only.
- The model receives read-only, bounded child records and never a write API.
- Do not start the installed app on either physical iPhone while verifying this change.

## Tasks

### 1. Current child context

- [x] Add `AskContextBuilder` and tests for a 30-day child-scoped snapshot with recent entries, completed-day trend facts, current-day facts, and Insights. Preserve time zone boundaries and absence semantics.
- [x] Format prompt facts as compact human-readable values with stable keys and escaped user-entered text; test injection boundaries and the context cap.

### 2. Flexible model and answer validation

- [x] Remove the action/activity rejection path from `AskViewModel` and replace `AskModel.interpret` with a broad question path that uses recent complete turns and fresh context.
- [x] Add structured generated prose with exact fact placeholders and used source IDs. Validate known keys, length, unsupported assertions, and numeric text; substitute Swift values and produce a concise fallback when invalid.
- [x] Add a separate, clearly labeled general-answer path without private log claims, maintaining no diagnosis or feeding prescription.
- [x] Add tests for “Is he eating better?”, follow-ups, unknown keys, overlong responses, no-data cases, and changed records on the next question.

### 3. Chat state and presentation

- [x] Show a pending question immediately, clear composer during generation, restore draft on failure/cancel, and preserve deletion-generation fencing.
- [x] Replace turn cards with aligned message bubbles, short assistant prose, descriptive source chips, a compact New Chat action, and a multiline composer with a clear send state.
- [x] Add simulator UI tests for the rendered exchange, composer, error state, source navigation, persistence, and unavailable tab.

### 4. Review and integration

- [x] Run focused package tests and Ask UI tests while iterating, then `make test`, `make build`, and the applicable final UI gate.
- [x] Inspect final diff and simulator screenshot, fix defects, update tracker, commit with Conventional Commits, fast-forward local `main`, and clean the isolated worktree.

## Review focus

- A follow-up referencing “that” or “yesterday” uses recent dialogue without treating a stale answer as current data.
- At 03:00, partial today is never compared as if it were a complete day.
- A deleted source or newly synced edit does not leak stale or another baby's data.
- Long chats, large text, keyboard, and Dynamic Type keep the composer and latest answer usable.
- Unavailable Apple Intelligence, model errors, and New Chat mid-generation preserve the right draft and conversation.
