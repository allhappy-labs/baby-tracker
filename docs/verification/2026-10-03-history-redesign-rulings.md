# History redesign — controller rulings

Rulings made during subagent-driven execution of `docs/superpowers/plans/2026-10-03-history-redesign.md`, copied from the execution ledger before its scratch workspace was deleted. Each records what was decided, why, and the cost if wrong.

## Rulings

1. Ruling H-R11: every feed/sleep delete in Tasks 2–6 uses deleteFeedPart/deleteSleepPart (single entry, solids included), removeComponent (one component), or deleteSession (whole session) — never softDelete on FeedEntry/SleepEntry — softDelete leaves joined members/timers linked to a tombstoned root — if wrong: none (new calls are supersets).
2. Ruling H-R12: editors call checkPartStart only when the part's start actually changed — a volume-only edit on a pre-existing overlap must stay saveable.
3. Ruling H-R13: the BaselineStoreTests isolation bug (SIGABRT with LiveCardSequenceTests) is fixed in Task 2 as a separate commit: @MainActor @Suite(.serialized), confirmed by 5 consecutive `make test` OK runs — gates must be reliable for the rest of the plan.
4. Ruling H-R14: H-R4 extends to sleeps — store adds previousPartEnd(ofSleep:for:) and checkSleepPartStart(_:ofSleep:for:); editor checks sleeps too (only when start changed, H-R12) — spec §3 "a part may not start before the previous part of its session ends" covers whole sleeps (H-R2).
5. Ruling H-R15: an edit may not move a part's start after the next part's start in its session (feed and sleep) — message "The start can't be after the next part began, at %@." (new key, 5 languages) — reordering breaks Split (separate*(at:) on a non-first-by-order member writes nothing); if wrong: a user must delete+re-add instead of dragging a part past its neighbour (rare).
6. Ruling H-R16: bottle edit hosted in PartEditSheet (Form + Bottle sheet's own chip/step maths) is accepted as "the sheet you log it with, in edit mode"; visual check in T7 screenshots.
7. Ruling H-R17: SessionSheetModel.addPart starts the new part 1 s after the session's last start when any member already starts at the computed start (strictly after every existing member), test pins position (last) — keeps order deterministic.
8. Ruling H-R18: added parts never end in the future — ends clamp to now; if a Left/Right part would then be shorter than the store's 30 s minimum (session ended < 31 s ago), Left/Right are disabled in the Add part menu until enough time passes (sleep: clamp, minimum 60 s same rule) — matches PastEntrySheet's no-future rule; if wrong: a parent must wait ~30 s to add a breast part right after a session (rare).
9. Ruling H-R19: feed chips use whole minutes like the approved mockup ("L 12m"; under a minute "45s"), not ElapsedFormatter.precise — port affected old assertions — if wrong: seconds hidden in the list (still in the part editor).
10. Ruling H-R20: HistoryListRow gains `runningSince: Date?` (non-nil only for running rows) so Task 6 ticks the live duration from the row — small signature addition to the plan's interface.
11. Ruling H-R21: a running part that continues a session shows as its own row at the top of today (accepted).
12. Ruling H-R22: third empty state .nothingRecent(dayCount:) accepted.
13. Ruling H-R23: endless scroll is required (spec §1) — page older days in as the list nears its end (e.g. 14-day pages); no fixed 30-day cap.
14. Ruling H-R24: a legacy 0 ml bottle stays editable — Save allowed when the volume is unchanged (note/time edits); only changing to 0 is refused.
15. Ruling H-R25: viewers get no sheet from Ask focus (read-only reading of H-R6) — accepted; deleted-before-open sheet closes silently — accepted.
16. Ruling H-R26: accept I1 as a known limitation of old data, documented in the verification doc — production has 0 solids components ever (probe 2026-10-03) and the new client never creates mixed solids entries (solids never join; separate entries) — if wrong: such an entry's solids part is editable only by deleting/re-logging the feed.

## Task and milestone notes

- Task 1: complete (87dc772) — review Approved, 6 minors in task-1-review.md.
- Task 2: complete (d2db5e9 f90390f e2d582d bd09b8e) — re-review Approved. Carry to T7: confirmation-dialog cancel may not be a button on iPhone — use <prefix>.confirmDelete, check .keep exists before use.
- Task 3: complete (ec32abe cfc98de 1b16f3d e5e4f26 620c311) — re-review Approved. Parked minor: side/note-only edit of lone timeless part stamps startedAt=feed.startedAt (valid; fix startedAt: timesChanged ? newStart : startedAt) — fold into final fix wave.
- Task 4: complete (0b7b4e9 a0bbc6e) — re-review Approved. Minors parked: TimelineView keeps 5 s redraw after re-enable; sleep Add part button may not look dimmed + no hint why Left/Right disabled (check T7 screenshots). Carry to T7: UI seeds end sessions ≥1 min before launch.
- Task 5: complete (4f086bb 586788d 1e0ce18) — follow-up (H-R19/H-R20/ported test) reviewed as part of Task 6's package. make test 1382.
- Task 6: complete (473931c 15a80be 2cb0bd2 279688c 3e5bcb6). Carry to T7: M1 history.row.<n>.running not findable (merged a11y children) — query differently or expose; M2 linked-mixed seed is time-of-day dependent (before ~01:24 lists under Yesterday) — make seeds time-robust.
- Final review: With fixes — I1 combine/separate don't relink active timer; I2 session-id resolution matches member before session key → swipe-delete deletes wrong feed; I3 Join row double tap chains joins; I4 spec §5 E2E sync of Add part/Split/Join/delete-first missing. 12 minors. Merge clean; Makefile conflict with feat/family-live-activity-push (whoever merges second). Fix wave dispatched.
- Final fix wave: DONE 6adcd14..d1f49f2 (I1–I4 + M1 M3 M6 M8 M10 + parked T3 minor; make test 1401, build, protocol-e2e step 24, loc clean, History UI 17/0). Remaining risk: remote Join can orphan local running timer (handled, not prevented); M3 end check editor-only. Scoped re-review dispatched.
