# Caregivers screen: invitations, QR joining, member management — design

Date: 2026-09-26. Status: approved in conversation; this spec is for review.

## Why
The user's words: "After invite is created it still shows create and only the last one can be revoked. We also should be able to scan invitation qr code. Overall the caregivers screen should be improved to be on par with other similar apps and have a great UX."

Facts behind it (verified in code):
- `FamilySettingsView.swift:23` keeps the created invitation in a single view-local `@State invitation: FamilyInvitation?`. Each create overwrites it, and it is lost when you navigate away. Earlier invitations stay active on the server for 7 days with no way to revoke them.
- The server has no endpoint that lists a family's invitations (`router.go:74-79`, `160-164`). It stores only an HMAC digest of each token (`internal/family/invitations.go:14-27`), so a code can never be read back.
- The QR scanner removed in `b6b2a82` never scanned anything. It asked for camera permission and showed text, but had no capture session and no VisionKit scanner.
- There are no deep links: no `onOpenURL`, no `CFBundleURLTypes`.
- Other defects:
  - Viewers are labelled "Caregiver" (`roleLabel`, `FamilySettingsView.swift:405`).
  - There is no "You" marker.
  - "Make owner" is offered to viewers, but the server allows transfer only to caregivers (`security/state.go:627`).
  - The Remove and Make-owner confirmations don't set their copy, so the sheet can show stale or empty text.
  - The OpenAPI file lacks `role` on the create request and `viewer` in the member role enum.

## Decisions (user)
- A pending invitation's code is kept **on the creating phone** (Keychain) so that phone can show it again. The server lists pending invitations but never stores or returns the secret.
- QR joining works **both ways**: an in-app live scanner, and the iPhone Camera app opening Baby Tracker through a `babytracker://` link.
- The layout below, including a member detail page instead of swipe actions, was approved.

Unchanged: invitations are single-use, expire after 7 days (`BT_INVITATION_TTL`) and are capped at 10 active per family (`BT_MAX_INVITATIONS`). Roles are owner, caregiver and view-only. The code stays the 43-character token.

## Screen (mode `.shared`)
```
[Sync status line]
MEMBERS
 (O) Olena            You · Owner
 (M) Marta          Caregiver     >   (owner only: row opens member detail)
 (G) Grandma         View only     >
PENDING INVITATIONS                     (owner only; hidden when empty)
 [person.badge.plus] Caregiver invitation   Expires in 6 days   >
 [eye]               View-only invitation   Expires tomorrow    >
 [+ Invite someone]                     (owner only)
LEAVING
 Disconnect this phone · Leave family · Delete family (owner)
```
**Members**
- Order: owner first, then by display name (case-insensitive).
- Each row shows a circular initials avatar (a stable colour per member id), the name, and the role label from `FamilyRole.displayName`. Your own row adds "You ·" before the role and has no chevron.

**Member detail** (owner only, pushed)
- Header: avatar, name, role.
- A "Role" picker (Caregiver / View only) with a one-line explanation of each. A change asks for confirmation, then calls `changeRole`.
- "Make owner" appears only when the member is a caregiver. It asks for confirmation with copy that says you become a caregiver.
- "Remove from family" (destructive) asks for confirmation with copy that names the member.
- The confirmations use `FamilyLifecycleConfirmationView` with the copy for that action set explicitly. This fixes the stale-copy bug.
- After an action, the page pops back and the members list reloads.

**Pending invitations**
- The list comes from the server, reloaded on appear, after every invitation action, and by pull-to-refresh on the screen.
- Each row shows the role icon, "Caregiver invitation" or "View-only invitation", and a relative expiry ("Expires in 6 days", "Expires today").
- Tapping a row whose code is on this phone opens the invitation sheet.
- Tapping a row whose code isn't on this phone opens a small sheet: "This code was created before a reinstall or on another phone. Revoke it and create a new one if you need to share it again." It has a Revoke button.
- Swiping a row gives Revoke (destructive) with no extra confirmation. Undo isn't possible, but the cost of a mistake is one new invite.
- When the limit is reached (the server returns `invitation_limit`), a message says "You have 10 open invitations. Revoke one to create another."

**Invite someone** (sheet)
1. The first step has a segmented picker, Caregiver / View only (default Caregiver), each with a one-line explanation, and a "Create invitation" button.
2. The result step shows:
   - a large QR code (CoreImage generator, correction "M", rendered crisp; no network);
   - the code, monospaced and selectable, with "Copy code";
   - "Share", which uses `ShareLink` with a message: "Join our family in Baby Tracker: <https link>. Or open Baby Tracker → Settings → Caregivers → Join and paste this code: <code>";
   - "Expires {date}";
   - "Revoke invitation" (destructive), which closes the sheet;
   - "Done".

The QR code encodes `babytracker://invite?server=<canonical origin>&token=<token>`. `InvitationParser.parse` already accepts that.

## Joining (mode `.authenticated`, no family; also the first-launch Join sheet)
- The primary button is **"Scan QR code"**. It opens a full-screen scanner that uses VisionKit `DataScannerViewController`, recognising QR codes only, with high-frame-rate tracking off and guidance on.
  - The first QR payload that `InvitationParser.intent(fromCodeOrLink:confirmedOrigin:)` accepts closes the scanner and goes into the existing join flow.
  - Payloads it rejects show "That QR code isn't a Baby Tracker invitation" and scanning continues.
  - If `DataScannerViewController.isSupported == false` or `isAvailable == false` (simulator, or camera restricted or denied), the scanner isn't shown. Instead the screen says: "Camera isn't available. Paste the code instead." For a denied camera, there is a button that opens Settings.
- Below it is "Or paste a code": the existing field, Paste and Join, unchanged.
- The camera permission string is `NSCameraUsageDescription` = "Baby Tracker uses the camera to scan a caregiver invitation QR code."

**Deep links**
- The app registers the URL scheme `babytracker`.
- `RootView` handles `.onOpenURL`. It parses the URL with `InvitationParser.parse` and keeps a `pendingInvitation: InvitationIntent`.
- When the phone is already in a family, it shows an alert: "You're already in a family. Leave it first to join another."
- Otherwise it presents the Join sheet with the intent prefilled:
  1. If no server is confirmed, the server step opens prefilled with the intent's origin. The user still confirms it, and the existing endpoint checks run.
  2. The name and sign-in steps follow if they're needed.
  3. A confirmation step shows the family name from `POST /v1/family/invitations/preview`: "Join {family}?" with Join / Cancel.
- The in-app scanner uses the same confirmation step.
- A preview failure (expired, revoked, consumed) shows the matching `JoinFailureCopy` message.
- The `https://…/invite#token=…` handoff page keeps working. Its "Open Baby Tracker" button now opens the app.


## Invitation web page (added 2026-09-26 at the user's request: "prettify the invite related web pages, they should have good UI and UX")
The only invite web page is the handoff page `GET /invite` (`Server/internal/httpapi/assets/invitation.html`). An `https://…/invite#token=…` link opens it. Today it is unstyled.

**Look**
- Polished and mobile-first, and it looks like the app:
  - the system font stack (`-apple-system, …`);
  - rounded cards and the app's accent blue;
  - light and dark via `prefers-color-scheme`;
  - comfortable tap targets of at least 44 pt;
  - respects `prefers-reduced-motion`.
- The brand mark is an inline SVG. It is not an `<img>`, because `img-src` stays `'none'`.

**Content (valid token)**
1. A header: the "Baby Tracker" mark and the title "You're invited to a family".
2. One line on what that means: "Join to log feeds, sleep and diapers together."
3. A primary button, "Open in Baby Tracker", with the existing `babytracker://` link.
4. The code in a monospaced, wrapping, selectable box, with a secondary "Copy code" button. The existing clipboard fallback stays, and the status lives in an `aria-live` region.
5. A short "If the app doesn't open" help: "Open Baby Tracker → Settings → Caregivers → paste the code". It also says the invitation works once and expires.

**Invalid or missing token:** a friendly error card, "This invitation link isn't complete. Ask for a new invitation.", with no buttons.

**Security (unchanged rules):**
- Still a single inline script.
- One inline `<style>`, allowed by a sha256 hash in `style-src`, computed the same way as the script hash.
- No external requests of any kind: no fonts, images, `https://` URLs, `fetch` or analytics.
- The token is still read only from the fragment, and the fragment is cleared before the token is rendered.
- Every existing contract string in `TestInvitationHandoffIsStaticPrivacySafeAndHashPinned` stays.

## Server
**New route: `GET /v1/family/invitations`**
- Owner only. It uses the same authorization as `GET /v1/family/invitations/{id}`: `manage_members` inside `WithAuthorizedTransaction`.
- It returns `{ "invitations": [InvitationSummary] }` for invitations with `status = 'active' AND expires_at > now()`, ordered by `created_at DESC`, limited to 50.
- `InvitationSummary` is `{id, family_id, role, status, expires_at, created_at}` and never includes a token or digest.
- Rate limiting and the protocol and capability headers follow the existing `GET /v1/family/invitations/{id}` route.

**Other changes**
- `InvitationView` gains `Role` and `CreatedAt`. The create response and `GET /v1/family/invitations/{id}` include `role` and `created_at`; this only adds fields.
- OpenAPI:
  - a new path and the `InvitationSummary` schema;
  - `role` on the create request, with enum caregiver/viewer;
  - `role` and `created_at` on `InvitationResponse`;
  - `viewer` in the `FamilyMemberResponse.role` enum.
- No migration: the table already has `role` (viewer allowed by migration 14) and `created_at`. Schema stays 17.

## Client
**BabySync**
- `FamilyInvitation` gains `role: FamilyRole` and `expiresAt: Date`, decoded from the create response.
- New `PendingInvitation {id, role, expiresAt, createdAt}`.
- `FamilyClient` adds:
  - `func pendingInvitations() async throws -> [PendingInvitation]`, implemented by `HTTPFamilyClient` and `DeferredFamilyClient`;
  - `func previewInvitation(token: String) async throws -> InvitationPreview` (`{familyName, expiresAt}`).
- New `InvitationCodeStore`: saves the share URL keyed by invitation id, in the Keychain, using the existing `KeychainBackends`, scoped to the family realm. Its API is `save(_:for:)`, `shareURL(for:)`, `remove(_:)` and `prune(keeping ids:)`.
  - It prunes to the server's list on every reload.
  - It is cleared on leave, disconnect, family deletion and sign-out.

**SettingsFeature**
- `FamilySettingsViewModel` owns `pendingInvitations: [PendingInvitation]`, `createInvitation(role:)` (saves the code, reloads the list), `revokeInvitation(_:)` (removes the code, reloads), `shareURL(for:)` and `previewInvitation(_:)`.
- The view has no invitation state of its own any more.
- New views: `MemberDetailView`, `InviteSheet` (which replaces `InvitationCodeView`), `PendingInvitationRow`, `InvitationScannerView` (a `UIViewControllerRepresentable` around `DataScannerViewController`), `InvitationQRCode` (the CoreImage renderer), `MemberAvatar`, and `JoinConfirmationView`.
- `FamilySettingsView` is split so that no file is over roughly 400 lines.
- All copy lives in `*Copy` enums.

**App**
- An `INFOPLIST_FILE` partial adds `CFBundleURLTypes` (scheme `babytracker`) and is merged with the generated Info.plist.
- `INFOPLIST_KEY_NSCameraUsageDescription` is set in both configurations.
- `RootView` gets `.onOpenURL`.

## Errors
| Case | Behaviour |
| --- | --- |
| List fails (offline) | Keep the last list, show an inline "Couldn't refresh invitations" row with Retry |
| Revoke on an already redeemed or expired invitation | Treat as done, reload the list |
| Create hits `invitation_limit` | Message as above |
| Scanner gets a non-invitation QR | Inline hint, keep scanning |
| Deep link while in a family | Alert, nothing changes |
| Deep link origin ≠ confirmed server | Existing `.endpointConfirmationRequired` path: "This invitation is for a different server" with the origin shown |
| Preview says expired, revoked or consumed | `JoinFailureCopy` message, no join |

## Tests
**Go**
- The list returns only active, unexpired invitations for the caller's family, newest first, with the role.
- A caregiver or viewer caller gets the same refusal as `GET /v1/family/invitations/{id}`.
- The list never contains a token or digest.
- The create response carries the role.
- OpenAPI contract tests are updated.

**Swift unit**
- The view model:
  - lists several invitations and revokes each one independently;
  - saves the code on create and prunes to the server list;
  - clears the code store on leave;
  - `shareURL(for:)` is nil for an unknown id;
  - viewers never see "Make owner";
  - each confirmation's copy is set per action.
- `InvitationCodeStore` against the in-memory Keychain backend.
- Member ordering and the "You" marker.
- The QR payload round trip: render, then read back with `CIDetector`, gives `InvitationParser.parse` the same intent.
- Deep-link routing: in a family → alert; no family → Join sheet with the intent.

**UI** (simulator 596D9F68)
- The owner creates a caregiver and a view-only invitation. Both are listed, the first is revoked, and the second is still listed and opens its QR sheet again.
- Member detail: role change, and no "Make owner" for a viewer.
- The scanner shows "Camera isn't available" on the simulator, and paste still works.
- `xcrun simctl openurl` with a `babytracker://invite` link opens the Join confirmation.
- The accessibility audit and largest Dynamic Type pass on the new screens.

**E2E** (real server image), a new step in `TwoCaregiverSyncE2ETests`
- The owner creates two invitations and the list shows both.
- The joiner redeems one through a `babytracker://` payload.
- The owner's list now shows only the other invitation, which the owner revokes. The list is empty.

**Device**
- Automated checks can't point a camera at a QR code. Everything around the scan is proven first:
  - the QR payload round trip;
  - the `babytracker://` link opening the app on a real phone (`devicectl` with `--payload-url`);
  - the scanner falling back cleanly.
- Only then does the user scan with both phones. That scan is the single manual check.

## Out of scope
- Short or typeable codes.
- Universal links (`https` associated domains).
- Push notifications for joins.
- Invitation email or SMS.
- Restoring codes on another owner device.
