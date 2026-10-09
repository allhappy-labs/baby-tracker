# Ask tab design

## Intent and scope

Give a parent a question-first place to understand one baby's recorded care. The first release covers caregiver handoffs, questions about the log, and plain-language explanations of existing Insights. It reads the family's current records and does not create, edit, or delete care entries. The answer must distinguish recorded facts from missing data and must not give a diagnosis or a feeding prescription.

## Interaction

- Add **Ask** as the fifth bottom tab, between History and Insights. Keep Home, History, Insights, and Settings as they are. The selected baby's name is visible in the Ask header. The existing baby switcher changes the active Ask conversation along with the other baby-bound tabs.
- On an empty conversation, focus the screen on the question field. Show suggested questions: “Catch me up,” “What happened overnight?”, and “Explain this week’s Insights.” Tapping one submits it as a normal question. Do not automatically generate a handoff merely by opening the tab.
- Show the conversation in chronological order with a persistent composer at the bottom. An answer states its baby and time window, and supports links to the source entries or Insights figures it used. The handoff is an answer in this conversation, not a separately stored report. Do not present an answer as live data after it is generated; show when it was answered. A new question reads current records, including newly synced records.
- Keep exactly one conversation per baby per caregiver installation. It persists across app launches. Switching babies preserves each baby's separate conversation. A **New Chat** control confirms that the current baby's existing conversation will be permanently deleted, then replaces it with an empty one. It never deletes care records or another baby's conversation.
- If the model is unavailable, existing messages remain readable. Explain the unavailable state and allow retry when the model becomes ready. An unanswered draft remains editable. Generation errors leave the question and any prior answers intact; the user can retry without creating a duplicate turn.
- Questions outside the baby's recorded care or supported Insights receive a short scope explanation. For medical concerns, the app may restate relevant recorded facts but must not assess whether the child is healthy, diagnose, or prescribe care.

## Data boundaries and persistence

- Store chat messages in the app's private local container, outside the shared App Group SwiftData store, sync outbox, server protocol, family export, and widgets. No chat transcript is sent to the baby-tracker sync server or to another caregiver. Use the on-device `SystemLanguageModel` for generation; do not silently route a private chat to Private Cloud Compute or another provider.
- Namespace saved conversations by explicit baby ID and local caregiver identity. For a signed-in family member, include the account ID so a different account using the same installation cannot see the prior account's chat. A local-only installation has its own stable local identity. Handle local-to-family transitions without mixing conversations between babies or people.
- Persist the displayed question, answer, creation time, answer time, and source references. The persisted transcript is a display record, not an indefinitely growing model context. Rebuild a bounded context from recent turns for each request, and retrieve current log facts anew. Do not persist raw model prompts, full retrieved record dumps, or Foundation Models session transcripts.
- On confirmed New Chat, delete the current scoped conversation atomically before displaying the empty state. Remove orphaned conversations when their baby is permanently deleted; account deletion or local-data reset removes that identity's conversations. Prevent a pending model response from restoring a conversation after New Chat or identity/baby change.

## Answer pipeline

1. Capture the explicit baby ID, local identity, question, and current time. Check `SystemLanguageModel` availability and supported language before starting generation.
2. Interpret the question into a constrained request: handoff, recorded-event lookup, aggregate/comparison, Insights explanation, or unsupported. Bound and validate any time window and record kind before querying. Ambiguous questions ask for clarification instead of guessing.
3. Read only nondeleted entries for that baby through `BabyStore` and existing Insights calculations where applicable. Compute counts, durations, volumes, comparisons, and time-zone-aware day boundaries in Swift. Preserve ordered feeding components and distinguish a missing log from a measured zero. For a handoff, use a clear recent window and identify the exact window in the answer.
4. Give the model a compact factual snapshot with stable source IDs and ask for structured answer text plus source IDs. Treat notes and question text as untrusted data. Validate every returned source ID against the snapshot; discard unsupported claims or fail closed with a factual fallback. The model may phrase and summarize; it is never the source of numeric or temporal truth.
5. Save the completed turn locally and render source links. Follow-up questions can use bounded recent chat context, but any requested facts are retrieved from the current store. Never allow the model to call write APIs.

## Architecture

- Add an `AskFeature` package target for the SwiftUI screen, view model, question interpretation, factual snapshot, answer validation, and model adapter. Keep a narrow protocol around model generation so deterministic query and persistence behavior can be tested without a downloaded model.
- Add an app-private conversation repository with atomic load/save/delete operations, separate from `BabyStore` and `BabySync`. The app composition supplies the active baby, local identity, repository, and read-only store access. The UI owns task cancellation when the selected baby, identity, or conversation generation changes.
- Reuse or extract the existing Insights calculation path rather than rebuilding figures with different definitions. Source references resolve through History/Insights navigation, with an unavailable-entry state if a referenced record was later deleted.
- Keep prompts, answer structure, and context size small enough for the on-device model; check actual token budget instead of hardcoding an assumed capacity. Add localized UI copy and accessibility labels following the existing app patterns.

## Verification and acceptance

- Unit tests cover child and account isolation, local-only identity transition, persistence across relaunch, confirmed New Chat deleting exactly one conversation, baby/account removal, and cancellation of a late response after deletion or switching.
- Query tests cover handoff across midnight, selected time zone, ordered mixed feeding components, newly synced edits and tombstones, zero versus no data, and the same figures as Insights. Validate unsupported and ambiguous questions and reject invented source IDs or changed numeric facts.
- UI tests cover the fifth tab, selected baby's name, question-first empty state, suggested questions, conversation persistence, New Chat confirmation, unavailable-model state, retry, and source navigation. Use a fake model for repeatable tests; separately verify on a supported physical device with Apple Intelligence enabled that the real model produces useful answers, that disabling it leaves chat readable, and that no model request reaches the family server.
- Run package tests, the iOS build, relevant simulator UI tests, and a scoped diff review. A simulator build or fake-model test does not establish actual model availability or response quality on a device.

## Out of scope

No shared or cross-device chat, automatic background handoffs, write actions from chat, medical advice, server model fallback, or long-term archive of multiple conversations per baby.
