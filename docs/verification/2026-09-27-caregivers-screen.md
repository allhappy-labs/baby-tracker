# Caregivers screen, invitations and QR joining — verification 2026-09-27

## Status: DEPLOYED AND INSTALLED — ONE MANUAL CHECK OPEN (camera scan)

Deployed revision `2682dff` (server + both phones). This release also ships the Supplements feature merged by another session: server migration 18, not previously deployed.

## Automated evidence (branch rebased onto main 403d29c, head 2682dff)
| Gate | Result |
| --- | --- |
| `make test` | `OK: 840 tests` |
| `make build` | `BUILD SUCCEEDED` |
| UI suite (dedicated simulator, erased) | `Executed 80 tests, with 0 failures` |
| `make server-test` | exit 0, 29 packages ok |
| `make server-smoke` | exit 0; readiness 200, schema 18 |
| `make server-protocol-e2e` | exit 0; proof steps 1–13, 16, 17, 14, 15, 18; cleanup proof: zero containers and volumes |

Step 18, run against the real server image:
1. The owner creates a caregiver invitation and a view-only invitation; the list shows both.
2. A third phone joins with the view-only one, through its QR payload.
3. The owner then sees only the caregiver invitation and revokes it.
4. The list is empty and the stored codes are gone.

Reviews:
- Each task was reviewed on its own, and the whole branch was reviewed on opus.
- The final review found that the scanner never asked for camera access. It is fixed: with permission not yet decided, the app asks first and then checks again.
- The fix wave also made five smaller fixes:
  - a stale join prompt is cleared;
  - the list reloads when ownership changes;
  - the old owner's saved codes are cleared after handing ownership over;
  - the invitation list query now names the family explicitly;
  - the member confirmation sheet gives way to an arriving link.

## Deployment evidence (Unraid)
- Source: `git archive 2682dff Server`, sha256 `2760ffef…c1db`, which matched on the box. The previous source is kept as `source.f186224-rollback` plus `deployment.env.f186224-rollback`.
- Migration ran; schema is `18`; the image revision label is `2682dff8e954d0bb17ec9bdc7e631c2cc1d219e2`.
- `verify-unraid-deployment.sh verify` passed: health 200, readiness 200, the live socket's upgrade reaches the app, no host ports, and the unrelated containers are unchanged.
- There were 0 ERROR or WARN lines in the first minute.
- `GET /v1/family/invitations` without a bearer returns 401 through Tailscale Serve.
- `/invite` returns 200 with `style-src 'sha256-…'` in its CSP.

## Phones
- Built from the clean worktree at `2682dff`, not the main checkout, which holds another session's uncommitted widget edit. Installed on `Phone A` and `Phone B`.
- The built Info.plist has the URL scheme `babytracker` and `NSCameraUsageDescription`.
- On-device deep link, at 03:00:
  - `devicectl --payload-url 'babytracker://invite?server=https%3A%2F%2Fbaby-sync.example-tailnet.ts.net&token=<43×A>'` **launched Baby Tracker on `Phone A`**.
  - In the server log for that window: the startup requests (`/v1/server`, `/v1/account`, `/v1/family`, `/v1/sync`, `/v1/live` 101), and **no `/v1/family/invitations/preview` request**. That is correct for a phone already in a family: it shows the "already in a family" alert, and the token goes nowhere.
  - `Phone B` was locked (`RequestDenied`), so this wasn't retried there.
  - The simulator UI tests cover a link opening "Join {family}?" on a phone that isn't in a family.

## Device checklist (for the user)
- [ ] Owner phone: Settings → Caregivers shows members with avatars and "You · Owner", and the pending invitations list, where any old leftover invitations can be revoked.
- [ ] Invite someone → View only → Create: a QR code, the code, Copy, Share and the expiry are shown. Close it, reopen it from Pending invitations, and the same QR code appears.
- [ ] On a phone that isn't in a family: Caregivers → Scan QR code. The camera permission prompt appears, and scanning the owner's QR code shows "Join {family}?".
- [ ] Scan the QR code with the iPhone Camera app: Baby Tracker opens.
- [ ] Open a shared https invite link in Safari: the styled page appears, and "Open in Baby Tracker" opens the app.
