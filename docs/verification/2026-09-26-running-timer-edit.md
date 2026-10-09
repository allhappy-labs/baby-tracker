# Running-timer fix, timer editing and invitation codes — verification 2026-09-26

## Status: AUTOMATED GATES CLOSED — ON-DEVICE CHECK OPEN

Deployed revision `737a764` (server + both phones).

## Automated evidence (branch head 737a764)
| Gate | Result |
| --- | --- |
| `make test` | `OK: 724 tests` |
| `make build` | `BUILD SUCCEEDED` |
| UI suite (dedicated simulator, erased) | `Executed 63 tests, with 0 failures` |
| `make server-test` | exit 0, 29 packages `ok` |
| `make server-smoke` | exit 0 |
| `make server-protocol-e2e` | exit 0; two-caregiver proof steps 1–14; cleanup proof: zero containers and zero volumes |

New in the e2e: step 11. The joiner edits the running feed through `RunningTimerEditModel`: start 2 min earlier, left side 300 s, formula 70 ml. The owner sees the edited start and parts, with the same component ids. Step 12 stops the feed, and both phones then hold exactly one completed feed, starting at the edited start.

Running dial fix (`a55dd9e`): `RunningTimerLifecycleTests.testRunningFeedKeepsCountingAfterAWriteWhileItRuns` failed before the fix, with the dial stuck at 0:04 for 5 s after adding a bottle. It passes after the fix, with the dial counting 0:07 → 0:08.

## Deployment evidence (Unraid)
- Source: `git archive 737a764 Server`. The sha256 matched on the box. The previous version is kept as `source.a5c6948-rollback` plus `deployment.env.a5c6948-rollback`.
- Image revision label `737a764ed2b7a64c4d8f7be468f38ebd530f9672`; schema `17`, with no migration in this release.
- `verify-unraid-deployment.sh verify` passed; `/healthz` and `/readyz` both returned 200 over Tailscale; 0 ERROR/WARN log lines in the first minute.
- Both iPhones (`Phone A`, `Phone B`) received the build via `devicectl`.

## Device checklist
- [ ] Start a feed; the timer counts every second while you watch; add a bottle and it keeps counting.
- [ ] Tap the timer; move the start 5 min earlier; save. "Started …" and the elapsed time update on this phone, the other phone, and the Lock Screen of both.
- [ ] After switching sides once, edit the finished side's minutes and the bottle volume; the other phone shows the same.
- [ ] Settings → Caregivers → Invite caregiver shows a code; Copy works. On a phone not yet in the family: Join a family → paste the code → Join.
