# Feeding sessions and editable step times

## Intent

Stopping a feeding timer for a break must not force a caregiver to treat the next breast or bottle step as a different meal. After a new timer stops and its feed is saved, the app may suggest combining it with a nearby earlier feed. The caregiver decides; nothing combines automatically. A diaper stays an independent record and does not prevent a suggestion. Timed bottle and breast steps must each have editable start and end times, as completed sleep does.

The behavior applies to new and existing History, local and shared-family data, and all relevant counts. It must not invent times missing from older records or treat breastfeeding as a measured milk volume.

## Representation

Link existing `FeedEntry` records into a logical feeding session with `feedingSessionID`. Each feed keeps its ID, components, notes, and sync history. A legacy feed with no link is its own session, identified by its feed ID. The session ID is an opaque grouping value, not a foreign key to a surviving feed. Combining links the later feed or group to the earlier session. Separating at a chosen boundary gives that feed and later group members a new session ID. Deleting one feed removes only that feed.

This preserves source records and makes Combine reversible. Physically merging rows would discard identities and require guessed component times. A display-only grouping would leave caregivers and statistics disagreeing about counts.

## Suggestion and History

After Stop successfully saves a feed, offer **Combine with previous feed?** if the same baby's most recent earlier session ended at most 60 minutes before the new feed started. Use a logged instant for an untimed feed. A sleep record between them suppresses the suggestion; a diaper or supplement does not. **Keep separate** is the default. Dismissal keeps both feeds saved and does not nag again. A failed link update leaves them separate and reports the failure.

History shows a linked session as one expandable row with ordered breast and bottle steps and total measured bottle volume. A diaper stays a separate, editable History row at its own time. The group displays both its clock span and the sum of active feeding durations, so breaks are visible without counting as feeding time. History offers **Combine with previous feed** and **Separate here** for old records and corrections; manual Combine can work outside the suggestion window.

The first feed's start anchors a session to its History day, even across midnight. Home, History, and daily Insights count a linked session once on that day; a rolling Ask query counts it once if any member falls in the requested window. Bottle volume remains the sum of recorded components, attributed to the actual day of each bottle step where its time is known, or to its parent feed's day for older untimed components. Ask sources still open the underlying feed records.

## Editable step times

The editor shows **Start time** and **End time** for each selected timed bottle or breast component, with date and time controls like sleep. Components gain optional absolute start and end timestamps alongside their existing duration. New timers fill both boundaries when switching sides, finishing a bottle, or stopping the feed. Editing a boundary recalculates only that component's duration; it does not shift other steps or fill a break. When both bounds are known, end must be after start; a newly set bound cannot be in the future. Breast duration remains positive. Timed components within one feed cannot overlap.

A manually logged bottle retains an editable start and no presumed end. A caregiver may supply an end to make it timed. Old single-step feeds may use parent start and end only when those bounds truly describe that step. Old multi-step feeds generally have durations but no individual step timestamps; the editor shows those timestamps as unknown until corrected. Existing duration and volume stay intact unless edited. When all component bounds are known, the parent feed's bounds follow the outer bounds. With unknown components, preserve the parent envelope and expand it only if a corrected step requires it.

## Sync and compatibility

Add the session link and optional component bounds to SwiftData, the feed wire shape, and server storage through a migration and a new feed payload capability. Older clients must not flatten or overwrite the new fields, or advance a cursor past a shape they cannot preserve. Link changes use normal feed update mutations. Multi-member Combine or Separate is one local transaction that syncs each changed feed. Feed and component IDs remain stable. The server validates family and baby ownership, time ordering, and agreement with duration when both step bounds are known. Concurrent caregiver edits must not delete or duplicate feeds.

## Acceptance checks

- A second breast, bottle, or mixed feed within 60 minutes gets a post-Stop suggestion. Keeping separate changes no records. A diaper in the gap does not suppress it; sleep does.
- Linking two or three feeds creates one History row and one feed count in Home, Insights, and Ask without changing measured volume, durations, diaper records, or source links. Separating restores the prior count. A midnight-spanning session belongs to its first feed's day.
- Each timed bottle and breast step permits independent start/end correction. Future, reversed, overlapping, and zero-duration breast times cannot be saved. A manual bottle has an optional end.
- Older multi-step feeds do not show fabricated component times. Two synced caregivers see the same grouping and corrections. Verify migration, legacy-client protection, concurrent edits, installed-device editing, and rollback before rollout.
