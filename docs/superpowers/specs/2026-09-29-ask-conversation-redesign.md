# Ask conversation redesign

## Intent

Ask should feel like a useful private conversation about the selected child. A parent can phrase a question naturally, ask a follow-up, and see a short answer that uses the current local records when relevant. It should not expose raw metric dumps or generic numbered source links. General parenting questions may be answered as general information, clearly separate from recorded facts. The on-device model remains the only model provider; chat remains private to the installation and New Chat deletes the current conversation.

## Conversation

- Show the selected child's name once in the navigation title. A user question appears as a compact trailing bubble; an answer appears as readable leading prose, with no card around the entire turn.
- Keep a multiline composer above the tab bar and keyboard. The send control has a 44-point target and a clear enabled state. Show the submitted question immediately and a generating indicator while the answer is pending. Failed generation restores the draft and offers retry.
- Show a short empty-state explanation and three useful suggestions. Keep the transcript anchored to the latest exchange without trapping the user at the bottom. Make New Chat a compact toolbar action with the existing destructive confirmation.
- Render only the sources actually used by a response, with human labels such as “Feed · 08:42” or “Insights · Feeding”, using the source's recorded timestamp or figure name. Preserve source navigation and deleted-source handling.

## Answer pipeline

- Remove the fixed action/activity classifier that rejects natural questions. Build a read-only, child-scoped context from the current store for each submitted question. Include the child's name, current local time and time zone, the exact coverage interval, recent events, completed-day trends, and relevant Insights figures. Give the model the last two complete question-answer turns for reference resolution; prior answers are conversational context, not authoritative records.
- For log questions, have the model write concise prose with fact placeholders and source keys. Resolve each placeholder from the freshly built Swift snapshot. Reject unknown placeholders, ungrounded numbers, medical judgments, and overlong responses; use a short question-relevant factual fallback when generation cannot be validated. Distinguish incomplete today from complete days and missing records from measured zero.
- For general questions, generate a concise answer separately and label it as general information, not derived from the child's log. Do not diagnose, prescribe feeding amounts, or claim current external knowledge. If a question mixes general and personal context, prefer the log path and state what the records cannot determine.
- Bound record and prompt size by a known time window and the model's context budget. Never send or save full retrieved record dumps, and never expose writes to the model.

## Preservation and verification

Keep existing identity and baby isolation, private file protection, persistence, and cancellation after New Chat. Existing saved turns must still decode and render. Tests cover open wording, follow-ups, personal/general separation, verified substitution, missing facts, context selection, immediate pending state, retained draft on failure, and rendered UI on a simulator. A real Apple Intelligence device run is useful but must not be inferred from simulator fake-model tests. The prior instruction not to start installed physical apps remains in force.
