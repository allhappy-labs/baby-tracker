# Caregivers Screen, Invitations and QR Joining Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The owner sees and controls every pending invitation, each with a QR code and a code that can be shown again. A caregiver joins by scanning the QR code in the app or with the iPhone Camera, or by pasting. Members get a clear list and a detail page.

**Architecture:**
- **Server:** a new read-only `GET /v1/family/invitations`, plus `role` and `created_at` on invitation views. No migration.
- **Client:**
  - A Keychain-backed `InvitationCodeStore` keeps each code on the creating phone.
  - The view model owns the pending list.
  - The screen is split into focused views.
  - A VisionKit scanner and a `babytracker://` URL scheme both feed one join confirmation step, backed by the existing preview endpoint.

**Tech Stack:** Swift 6.2, SwiftUI, VisionKit `DataScannerViewController`, CoreImage QR generator, Swift Testing, XCUITest; Go server with Postgres; Docker protocol-e2e harness.

> **Status (2026-09-27):** merged to main at `2682dff` (rebased onto the Supplements release), server redeployed (schema 18), installed on both phones. Evidence: `docs/verification/2026-09-27-caregivers-screen.md`. The camera scan is the one open manual check.

**Spec:** `docs/superpowers/specs/2026-09-26-caregivers-screen-design.md`. It is binding: the layout, copy, error table and test list are all there.

## Global Constraints
- Schema stays 17 with no migration. The list never returns a token or digest. Non-owners get the same refusal as `GET /v1/family/invitations/{id}`.
- List query: `status = 'active' AND expires_at > now()`, `ORDER BY created_at DESC`, `LIMIT 50`.
- QR payload: `babytracker://invite?server=<canonical origin>&token=<token>`. Scheme `babytracker`, registered via an `INFOPLIST_FILE` partial with `CFBundleURLTypes`.
- `NSCameraUsageDescription` = "Baby Tracker uses the camera to scan a caregiver invitation QR code."
- Codes are stored only in the Keychain, keyed by invitation id and scoped to the family realm. They are pruned to the server list on each reload and cleared on leave, disconnect, delete and sign-out.
- "Make owner" is offered only for caregivers. Every confirmation sets its own copy.
- All copy lives in `*Copy` enums. Update `EXPECTED_TESTS` with the package tests. No file over ~400 lines in SettingsFeature.
- Signed commits (Bitwarden agent: `SSH_AUTH_SOCK=~/Library/Containers/com.bitwarden.desktop/Data/.bitwarden-ssh-agent.sock`) with the trailer `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.
- UI tests run only on the dedicated simulator `596D9F68-9755-440C-A618-20D7933F7438`, erased first.
- Deploy order: redeploy the server before installing the app on the phones.

## Review Focus
1. **Revoking an invitation that was redeemed or expired a moment ago** → treated as done, the list reloads, no error banner. (Task 3 test `revokingAnAlreadyRedeemedInvitationJustReloads`.)
2. **Opening a `babytracker://invite` link while already in a family** → an alert and nothing changes; the phone's family is untouched. (Task 5 test `deepLinkWhileInAFamilyOnlyAlerts`.)
3. **Scanning a random QR code** (a URL or Wi-Fi code) → an inline hint, scanning continues, no join request. (Task 5 test `scannerIgnoresNonInvitationPayloads`.)
4. **Reinstalling the app, or an invitation from before this release** → the row shows the "not on this phone" sheet with Revoke; there's no crash and no empty QR. (Task 3 test `pendingInvitationWithoutStoredCodeHasNoShareURL`, Task 4 UI.)
5. **Offline while opening Caregivers** → the last list stays, with a "Couldn't refresh invitations" row and Retry; members are still shown. (Task 3 test `listFailureKeepsLastInvitationsAndFlagsIt`.)

---

### Task 1: Server — list pending invitations; role and created_at on views
**Files:**
- `Server/internal/family/types.go` (`InvitationView` + `Role string`, `CreatedAt time.Time`)
- `internal/family/repository.go`: `invitation()` selects role and created_at; new `pendingInvitations(ctx, authority, principal, familyID) ([]InvitationView, error)` using `WithAuthorizedTransaction(..., "manage_members", ...)`
- `internal/family/service.go`: new `func (service *Service) PendingInvitations(ctx, principal) ([]InvitationView, error)`, mirroring `Invitation()` lines 563-583; `Invite` returns the role and created_at in its view
- `internal/httpapi/router.go`: `GET /v1/family/invitations`, placed like the `{id}` GET and sharing its middleware and rate class; the service interface gains `PendingInvitations`
- `internal/httpapi/family.go`: `pendingInvitationsHandler`, returning `{"invitations":[...]}`
- `internal/protocol/family.go`: the `InvitationSummary` wire type
- `Server/api/openapi.yaml`: the new path, `InvitationSummary`, `role` on the create request (enum caregiver/viewer), `role` and `created_at` on `InvitationResponse`, `viewer` in `FamilyMemberResponse.role`
- Tests: `internal/family/invitations_test.go`, `internal/httpapi/family_test.go`, and the OpenAPI contract test if one pins paths

**Interfaces:**
- Produces the wire shape `GET /v1/family/invitations` → `200 {"invitations":[{"id","family_id","role","status","expires_at","created_at"}]}`.
- `InvitationResponse` gains `role` and `created_at`.

- [x] Failing Go tests:
  - `TestPendingInvitationsListsOnlyActiveUnexpiredNewestFirst`: create 3, revoke 1, redeem 1, expire 1 via the clock → only the right ones, in order, with their roles.
  - `TestPendingInvitationsRefusesNonOwners`: a caregiver and a viewer get the same error as `Invitation()`.
  - `TestPendingInvitationsNeverExposeSecrets`: the JSON has no `token`, `link` or `digest` keys.
  - `TestInviteResponseCarriesRole`: a viewer invite gives `"role":"viewer"`.
  - An HTTP handler test for the route, auth and headers.
- [x] Run `env -u BABY_TRACKER_TEST_DATABASE_URL make server-test` → FAIL
- [x] Implement
- [x] `env -u BABY_TRACKER_TEST_DATABASE_URL make server-test` → PASS
- [x] Commit `feat(server): list a family's pending invitations`

### Task 2: BabySync — client surface and code store
**Files:**
- `Packages/BabyTracker/Sources/BabySync/InvitationParser.swift` (`FamilyInvitation` + `role: FamilyRole`, `expiresAt: Date`)
- `FamilyClient.swift`: `PendingInvitation`, `InvitationPreview`, and the protocol methods
- `HTTPFamilyClient.swift`: create decodes `role` and `expires_at`; `pendingInvitations()` does `GET family/invitations`; `previewInvitation(token:)` does `POST family/invitations/preview` with `{token}`
- `DeferredFamilyClient.swift`: forwarding
- New `InvitationCodeStore.swift`
- Tests: `Tests/BabySyncTests/InvitationCodeStoreTests.swift`, `HTTPFamilyClientTests` (or wherever the create/revoke wire tests live), and `FamilySettingsFixture.swift` (the fake client implements the new methods and holds a mutable `pending` list)

**Interfaces:**
```swift
public struct PendingInvitation: Sendable, Equatable, Identifiable { public let id: UUID; public let role: FamilyRole; public let expiresAt: Date; public let createdAt: Date }
public struct InvitationPreview: Sendable, Equatable { public let familyName: String; public let expiresAt: Date }
// FamilyClient
func pendingInvitations() async throws -> [PendingInvitation]
func previewInvitation(token: String) async throws -> InvitationPreview
// FamilyInvitation gains: public let role: FamilyRole; public let expiresAt: Date
public struct InvitationCodeStore: Sendable {
    public init(backend: any KeychainBackend, realm: String)   // use the existing KeychainBackends protocol/type names
    public func save(_ shareURL: URL, for id: UUID) throws
    public func shareURL(for id: UUID) -> URL?
    public func remove(_ id: UUID) throws
    public func prune(keeping ids: Set<UUID>) throws
    public func removeAll() throws
}
```
- [x] Failing tests:
  - The code store saves, reads, removes, prunes to a set, keeps two realms isolated and clears everything with `removeAll`.
  - The HTTP client decodes the list, including the viewer role and fractional-second dates as the server emits them.
  - The preview decodes `family_display_name`.
  - The create response carries the role and expiry.
- [x] Implement. Check the real `KeychainBackends.swift` names first; use its in-memory backend in tests.
- [x] `make test` (update `EXPECTED_TESTS`)
- [x] Commit `feat(sync): list invitations and keep their codes on this phone`

### Task 3: View model — pending list, members, confirmations
**Files:**
- `Packages/BabyTracker/Sources/SettingsFeature/FamilySettingsViewModel.swift`, `FamilySyncSettingsModel.swift` (protocol additions)
- `FamilySyncComposition.swift` (inject the `InvitationCodeStore`)
- The leave, disconnect, delete and sign-out paths call `removeAll()`
- Tests: new `Tests/SettingsFeatureTests/PendingInvitationsTests.swift`, `MemberManagementTests.swift`

**Interfaces:**
- Consumes Task 2.
- Produces on the model protocol:
```swift
var pendingInvitations: [PendingInvitation] { get }
var invitationsRefreshFailed: Bool { get }
var sortedMembers: [FamilyMember] { get }            // owner first, then displayName case-insensitive
func isMe(_ member: FamilyMember) -> Bool
func reloadInvitations() async                       // never throws; sets invitationsRefreshFailed
func createInvitation(role: FamilyRole) async throws -> FamilyInvitation   // saves code, reloads
func revokeInvitation(_ id: UUID) async throws       // invitation_consumed/revoked/expired → treated as success; removes code; reloads
func shareURL(for id: UUID) -> URL?
func previewInvitation(_ intent: InvitationIntent) async throws -> InvitationPreview
func canMakeOwner(_ member: FamilyMember) -> Bool    // isOwner && member.familyRole == .caregiver
func confirmation(for action: MemberAction, member: FamilyMember) -> FamilyConfirmationCopy   // .remove/.makeOwner/.changeRole(to:)
```
- [x] Failing tests:
  - `twoInvitationsAreBothListedAndEachRevokesIndependently`
  - `creatingSavesTheCodeAndItCanBeShownAgain`
  - `reloadPrunesCodesTheServerNoLongerLists`
  - `pendingInvitationWithoutStoredCodeHasNoShareURL`
  - `revokingAnAlreadyRedeemedInvitationJustReloads`
  - `listFailureKeepsLastInvitationsAndFlagsIt`
  - `leavingClearsStoredCodes`
  - `membersAreOwnerFirstThenByName`
  - `viewerIsNeverOfferedOwnership`
  - `eachMemberActionHasItsOwnConfirmationCopy` (remove names the member; make-owner says you become a caregiver)
  - `invitationLimitShowsTheLimitMessage`
- [x] Implement, with copy in `FamilyCopy` / `FamilyConfirmationCopy`
- [x] `make test`
- [x] Commit `feat(settings): keep every pending invitation in view`

### Task 4: Screen — members, member detail, pending list, invite sheet with QR
**Files:**
- Create in `Packages/BabyTracker/Sources/SettingsFeature/`: `MemberAvatar.swift`, `MemberDetailView.swift`, `PendingInvitationRow.swift`, `InviteSheet.swift` (role step → result step), `InvitationQRCode.swift` (CoreImage `qrCodeGenerator`, correction "M", nearest-neighbour scaling, returns `Image`), and `FamilySharedSections.swift` (members, invitations, leaving)
- Modify `FamilySettingsView.swift`: remove the invitation `@State` and swipe actions, use the new sections, `.refreshable` reloads members and invitations, `roleLabel` uses `FamilyRole.displayName` with "You · " for yourself
- Delete `InvitationCodeView.swift`; its copy moves to `InviteSheetCopy`
- Tests:
  - `Tests/SettingsFeatureTests/InvitationQRCodeTests.swift`: render the payload, read it back with `CIDetector(ofType: CIDetectorTypeQRCode)`, and `InvitationParser.parse` returns the same intent.
  - UI tests in new `UITests/BabyTrackerUITests/CaregiversScreenUITests.swift`, using the app's existing UI-test launch fixtures for a signed-in owner with members. Find how `FamilySyncOnboardingUITests` seeds state, and extend the fixture if an owner-in-family fixture is missing.
- Accessibility ids: `family.member.<id>`, `family.inviteSomeone`, `invite.role`, `invite.create`, `invite.qr`, `invite.code`, `invite.copy`, `invite.share`, `invite.revoke`, `family.pending.<id>`, `member.role`, `member.makeOwner`, `member.remove`

**Interfaces:**
- Consumes Task 3.
- Produces `enum InvitationQRCode { static func payload(for invitation: FamilyInvitation) throws -> String; static func image(for payload: String) -> Image? }`.
  - `payload` parses `invitation.shareURL` with `InvitationParser.parse` and returns `babytracker://invite?server=<intent.origin.canonicalString, percent-encoded>&token=<token>`.
  - Task 6 uses it.
- [x] Failing tests:
  - the QR round trip;
  - UI `testOwnerCreatesTwoInvitationsAndRevokesTheFirst`: both are listed, the first is revoked, the second is still listed and opens its QR again;
  - UI `testMemberDetailChangesRoleAndHidesMakeOwnerForViewer`;
  - UI `testInvitationFromAnotherPhoneOffersOnlyRevoke`;
  - extend `testCaregiversScreenPassesAccessibilityAudit` and the largest-Dynamic-Type test to the invite sheet and member detail.
- [x] Implement per the spec's "Screen" section (iOS 26 look: grouped form, `.glass` / borderedProminent buttons as in the existing screens)
- [x] `make test`, `make build`, the affected UI tests on 596D9F68 (erase first)
- [x] Commit `feat(settings): a caregivers screen with every invitation and member at hand`

### Task 5: Joining — in-app scanner, deep link, join confirmation
**Files:**
- Create `SettingsFeature/InvitationScannerView.swift` (`UIViewControllerRepresentable` around `DataScannerViewController(recognizedDataTypes: [.barcode(symbologies: [.qr])], qualityLevel: .balanced, isHighFrameRateTrackingEnabled: false, isGuidanceEnabled: true)`, plus a fallback view when `!isSupported || !isAvailable`, with an "Open Settings" button when camera access is denied)
- Create `SettingsFeature/JoinConfirmationView.swift` ("Join {family}?")
- Create `SettingsFeature/InvitationRouting.swift`: a pure function `route(url: URL, hasFamily: Bool) -> InvitationRoute`, with `.alreadyInFamily`, `.join(InvitationIntent)` and `.ignore`
- Modify `FamilySettingsView.swift`: the no-family section gets "Scan QR code" first, then "Or paste a code"
- Modify `JoinFamilySheet.swift`: accept an optional prefilled `InvitationIntent`, prefilling the server step with its origin when none is confirmed
- Modify `Apps/BabyTrackerApp/RootView.swift`: `.onOpenURL` → route → alert or Join sheet
- Create `Apps/BabyTracker-Info.plist` with only `CFBundleURLTypes` (scheme `babytracker`)
- `BabyTracker.xcodeproj/project.pbxproj`: set `INFOPLIST_FILE` for the app target in both configs, keep `GENERATE_INFOPLIST_FILE = YES`, add `INFOPLIST_KEY_NSCameraUsageDescription`
- Tests: `Tests/SettingsFeatureTests/InvitationRoutingTests.swift`, `JoinConfirmationTests.swift`, UI tests in `CaregiversScreenUITests.swift`

**Interfaces:** Consumes `previewInvitation` and `acceptInvitation(_ intent:)`. Produces `InvitationRoute` and `JoinFamilySheet(prefilled: InvitationIntent?)`.
- [x] Failing tests:
  - `deepLinkWhileInAFamilyOnlyAlerts`
  - `deepLinkWithoutFamilyOpensJoinWithTheIntent`
  - `nonInvitationURLIsIgnored`
  - `scannerIgnoresNonInvitationPayloads` (the payload-handling closure, tested without a camera)
  - `joinConfirmationShowsFamilyNameThenJoins`
  - `previewExpiredShowsJoinFailureCopyAndDoesNotJoin`
  - `deepLinkForAnotherServerShowsTheOrigin`
  - UI `testScannerFallsBackToPasteWithoutACamera` (the simulator has no camera)
  - UI `testInvitationLinkOpensTheJoinConfirmation` (`XCUIApplication().open(URL)` or `xcrun simctl openurl` from the test host)
- [x] Implement
- [x] `make test`, `make build`, UI tests on 596D9F68 (erase first). Check that the built app's Info.plist contains `CFBundleURLTypes` and `NSCameraUsageDescription` (`plutil -p`).
- [x] Commit `feat(settings): join by scanning an invitation QR code`

### Task 6: Two-phone e2e step
**Files:**
- `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/TwoCaregiverSyncE2ETests.swift`
- `Server/scripts/protocol-e2e.sh` (require `step 16:`) and its Go structure test
- [x] Add step 16:
  1. The owner model creates a caregiver and a view-only invitation; `pendingInvitations` lists both, with their roles.
  2. A third phone (reuse the `Phone` helper) redeems the view-only one through the QR payload string. Build it the same way `InviteSheet` does; share one helper, `InvitationQRCode.payload(for:)`, from Task 4.
  3. The owner reloads and sees only the caregiver invitation.
  4. The owner revokes it and the list is empty.
  5. The stored codes are pruned to none.
- [x] `make server-protocol-e2e` → 16/16 (run serially on an idle machine; `rtk proxy make ...` for raw output)
- [x] `make server-test`
- [x] Commit `test(e2e): the owner sees and revokes each pending invitation`

### Task 7: Invitation web page — good UI and UX
**Files:**
- `Server/internal/httpapi/assets/invitation.html`: the redesign per the spec section "Invitation web page"
- `Server/internal/httpapi/invitation_handoff.go`: `handoffCSP` also hashes the single `<style>` block into `style-src 'sha256-…'`; it panics if the style block is missing, as it already does for the script
- `Server/internal/httpapi/family_test.go`: extend `TestInvitationHandoffIsStaticPrivacySafeAndHashPinned`

**Interfaces:** None. The `/invite` URL shape and the `babytracker://invite?server=…&token=…` link are unchanged.

- [x] Failing Go tests, added to the existing handoff test:
  - exactly one `<style>` block, and the CSP contains `style-src 'sha256-<hash of it>'`;
  - the CSP still has `default-src 'none'`, `img-src 'none'` and `connect-src 'none'`;
  - the body contains no `<img`, `url(`, `@import`, `https://` or `http://`;
  - every existing contract string is still present, and the page still clears the fragment before rendering the token;
  - the body contains `prefers-color-scheme`, `aria-live` and the spec's copy strings ("You're invited to a family", "Open in Baby Tracker", "Copy code", "This invitation link isn't complete").
- [x] Run `cd Server && go test ./internal/httpapi/ -run Handoff` → FAIL
- [x] Implement the page and the CSP change
- [x] `cd Server && go test ./internal/httpapi/` → PASS
- [x] Visual check:
  - Serve the page with its real headers from a throwaway local Go program (not committed). It should reuse `invitationHandoffHandler` via an `httptest`-style server on `127.0.0.1`, or a `_test.go` helper behind a build tag.
  - Screenshot it with headless Chrome (`"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new --window-size=390,844 --screenshot=<path> '<url>#token=<43 chars>'`) for light, dark (`--force-dark-mode`), and invalid (`#token=bad`), plus a desktop width of 1280.
  - Save the screenshots under the plan workspace `task-7-screens/` and confirm that the CSP doesn't block the styles: the page must render styled, not as bare HTML.
- [x] `env -u BABY_TRACKER_TEST_DATABASE_URL make server-test`
- [x] Commit `feat(server): a friendly invitation page`

### Task 8: Gates, redeploy, install, device checks
- [x] All gates on the final branch: `make test`, `make build`, the full UI suite on 596D9F68 (erased), `make server-test`, `make server-smoke`, `make server-protocol-e2e`
- [x] Fast-forward merge to main
- [x] Redeploy the server from main with the procedure in `docs/verification/2026-09-26-live-sync.md`:
  - `git archive` → scp with a sha256 check;
  - keep `source.<old7>-rollback` and `deployment.env.<old7>-rollback`;
  - build, migrate, `up -d --wait app`;
  - verify the script, the revision label and schema 17;
  - check that `GET /v1/family/invitations` without a bearer gets 401 through Serve;
  - check that `https://baby-sync.example-tailnet.ts.net/invite` returns 200 with `style-src 'sha256-` in its CSP.
- [x] Build and install on `Phone A` (00000000-0000000000000000) and `Phone B` (00000000-0000000000000000). Then check on a real phone that `xcrun devicectl device process launch --payload-url 'babytracker://invite?server=https://baby-sync.example-tailnet.ts.net&token=<43 x A>'` opens the app. Evidence: the app is foreground and a preview request appears in the server log.
- [x] Verification doc `docs/verification/2026-09-26-caregivers-screen.md`, with a device checklist for the one manual step: scan the QR with the in-app scanner and with the Camera app. Commit.
- [x] Remove the worktree and branch.
