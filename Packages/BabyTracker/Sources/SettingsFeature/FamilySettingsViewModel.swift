import BabyCore
import BabyStore
import BabySync
import Foundation
import Observation

/// The production family-settings model.
///
/// It owns no transport of its own: the coordinator holds endpoint, Apple and
/// Keychain concerns, and the family client holds P1's family operations. This
/// object's job is the *order* of those calls and the state the screens read,
/// which is where the rules that matter live — a family cannot be created
/// before a display name is confirmed, an invitation cannot be accepted from
/// an origin the user never confirmed, and an owner cannot delete their
/// account while they still own a family.
@MainActor
@Observable
public final class FamilySettingsViewModel: FamilySyncSettingsModel {
    public private(set) var profile = CaregiverProfile(accountID: UUID(), displayName: "", role: "caregiver")
    public private(set) var members: [FamilyMember] = []
    public private(set) var confirmationCopy = ""
    public private(set) var pendingInvitations: [PendingInvitation] = []
    public private(set) var invitationsRefreshFailed = false
    /// Recorded in call order so a test can prove the P1 lifecycle commands
    /// were used, and used in the right sequence.
    public private(set) var lastLifecycleCommands: [FamilyLifecycleCommand] = []
    /// App-private cleanup after the server has accepted account deletion.
    @ObservationIgnored public var onAccountDeleted: (@MainActor (UUID) -> Void)?
    /// Set once Apple has supplied, or the user has confirmed, a display name.
    public private(set) var displayNameConfirmed = false
    /// A stored observable mirror of the coordinator's session mode. Views
    /// that scope private content by account must redraw at the mode change,
    /// even when the transition does not write a baby-store row.
    public private(set) var mode: FamilySyncMode

    public let store: BabyStore
    private let coordinator: AccountCoordinator
    private let client: any FamilyClient
    private let defaults: UserDefaults
    private let makeEngine: FamilySyncEngineFactory?
    private let renewAccountSession: AccountSessionRenewal?
    private let exportClientFactory: (@MainActor (ConfirmedEndpoint) -> (any ExportServing)?)?
    private let exportCapabilityProbe: (@MainActor (ConfirmedEndpoint) async throws -> Bool)?
    public private(set) var exportAvailable: Bool?
    @ObservationIgnored private var cachedExportClient: (any ExportServing)?
    @ObservationIgnored private var cachedExportEndpoint: ConfirmedEndpoint?
    private let startLiveActivity: @MainActor (ActiveTimer, BabyStore) -> Void
    /// Where `InvitationCodeStore` keeps this phone's own copy of each
    /// invitation's link. Real Keychain access group in production
    /// (`FamilySyncComposition`); an in-memory fake in tests that never
    /// touch a real Keychain otherwise.
    private let codeStoreBackend: any KeychainBackend

    /// The running engine, exactly while this device is in a family. Nil for
    /// a local-only user, which is what keeps every sync hook below a no-op
    /// for them.
    public private(set) var engine: (any FamilySyncDriving)?
    @ObservationIgnored private var engineKey: FamilySessionKey?
    /// The last activation did not finish. Membership is kept regardless —
    /// the server already made this device a member — and the next
    /// foreground tries again.
    @ObservationIgnored private var activationPending = false
    @ObservationIgnored private var activating = false
    /// Mirrors the scene: polling runs exactly while the app is on screen,
    /// including for an engine built mid-session by creating or joining.
    @ObservationIgnored private var isForeground = false

    /// Tells the push registrar when a family session is active and when it
    /// ends. Nil in tests that are not about push, and in the UI-test graphs.
    @ObservationIgnored public var pushLifecycle: PushLifecycle?
    /// How the next successful activation came about. `.signedIn` from a
    /// sign-in, create, join or recovery until an activation succeeds; a
    /// launch or a `withSyncStopped` restart is `.restored`.
    @ObservationIgnored private var nextPushActivation: PushActivation = .restored
    /// The push work in flight, in order: an `activated` always runs after
    /// the work before it, including an `ending`. Not awaited by activation:
    /// an upload must not hold up sign-in or the launch sync.
    @ObservationIgnored private var pushWork: Task<Void, Never>?
    /// Bumped by every `endPush`. An `activated` still queued when push ends
    /// belongs to the ended session and is dropped.
    @ObservationIgnored private var pushGeneration = 0

    /// `makeEngine` is nil in tests that are not about sync, and the model
    /// then behaves exactly as it did before sync was wired.
    public init(
        store: BabyStore,
        coordinator: AccountCoordinator,
        client: any FamilyClient,
        defaults: UserDefaults? = nil,
        makeEngine: FamilySyncEngineFactory? = nil,
        renewAccountSession: AccountSessionRenewal? = nil,
        exportClientFactory: (@MainActor (ConfirmedEndpoint) -> (any ExportServing)?)? = nil,
        exportCapabilityProbe: (@MainActor (ConfirmedEndpoint) async throws -> Bool)? = nil,
        codeStoreBackend: any KeychainBackend = InMemoryKeychainBackend(),
        startLiveActivity: @escaping @MainActor (ActiveTimer, BabyStore) -> Void = { timer, store in
            LiveActivityController.start(for: timer, in: store)
        }
    ) {
        self.store = store
        self.coordinator = coordinator
        self.mode = coordinator.mode
        self.client = client
        self.defaults = defaults ?? UserDefaults(suiteName: BabySchema.appGroupID) ?? .standard
        self.makeEngine = makeEngine
        self.renewAccountSession = renewAccountSession
        self.exportClientFactory = exportClientFactory
        self.exportCapabilityProbe = exportCapabilityProbe
        self.codeStoreBackend = codeStoreBackend
        self.startLiveActivity = startLiveActivity
        // The name is asked before the first Apple sign-in and has to be
        // there again for every later one (a 24 h re-authentication, a
        // revoked credential), so it outlives the session that sent it.
        if let saved = DisplayNameRecord.load(from: self.defaults) {
            profile = CaregiverProfile(accountID: profile.accountID, displayName: saved, role: profile.role)
            displayNameConfirmed = true
        }
        coordinator.modeDidChange = { [weak self] mode in self?.mode = mode }
    }

    public var syncStatusLine: FamilySyncStatusLine? {
        guard hasFamily, let engine else { return nil }
        return FamilySyncStatusCopy.line(for: engine.status)
    }

    public var syncEngineStatus: FamilySyncEngine.Status? {
        guard case .shared = mode else { return nil }
        return engine?.status
    }

    /// Whether this phone is still catching up after coming to the
    /// foreground: what it shows may predate the other phone's last taps.
    public var syncIsCatchingUp: Bool {
        guard case .shared = mode else { return false }
        return engine?.isCatchingUp ?? false
    }

    /// Returns once the foreground catch-up has an answer (at most
    /// `FamilySyncEngine.defaultCatchUpHold`), or at once.
    public func waitForSyncCatchUp() async {
        guard case .shared = mode else { return }
        await engine?.waitForCatchUp()
    }

    public var pendingDeliveryCount: Int? {
        guard case .shared(let key) = mode else { return nil }
        // Delivery state, not an empty pull or a timestamp, decides whether
        // this phone still owes writes. A store read failure is never success.
        return try? store.pendingOutbox(in: key.realmID).filter { !$0.deliveryState.isTerminal }.count
    }

    /// `members`, owner first and then by display name — the order the
    /// server's own list has no reason to follow, since it is written to in
    /// whatever order caregivers joined.
    public var sortedMembers: [FamilyMember] {
        members.sorted { lhs, rhs in
            let lhsOwner = lhs.familyRole == .owner
            let rhsOwner = rhs.familyRole == .owner
            if lhsOwner != rhsOwner { return lhsOwner }
            return lhs.displayName.localizedCaseInsensitiveCompare(rhs.displayName) == .orderedAscending
        }
    }

    public var confirmedOrigin: ServerOrigin? { coordinator.confirmedEndpoint?.origin }

    public func refreshExportAvailability() async {
        guard hasFamily, isOwner, let endpoint = coordinator.confirmedEndpoint,
              let exportCapabilityProbe else { exportAvailable = false; return }
        exportAvailable = nil
        do {
            let available = try await exportCapabilityProbe(endpoint)
            exportAvailable = hasFamily && isOwner && coordinator.confirmedEndpoint == endpoint ? available : false
        } catch {
            exportAvailable = false
        }
    }

    public func makeExportClient() -> (any ExportServing)? {
        guard hasFamily, isOwner, exportAvailable == true, let endpoint = coordinator.confirmedEndpoint,
              let exportClientFactory else { return nil }
        if cachedExportEndpoint == endpoint { return cachedExportClient }
        cachedExportEndpoint = endpoint
        cachedExportClient = exportClientFactory(endpoint)
        return cachedExportClient
    }

    /// This device's role in the family. The store's write boundary is what
    /// actually enforces it; this is what the screens read to decide what to
    /// show.
    public var role: FamilyRole { profile.familyRole }

    // MARK: Endpoint and identity

    public func confirmEndpoint(_ endpoint: ConfirmedEndpoint) async throws {
        coordinator.confirmEndpoint(endpoint)
        // The family client cannot exist before there is somewhere legitimate
        // to send its requests.
        (client as? DeferredFamilyClient)?.endpointConfirmed(endpoint)
    }

    /// Signs in with the name the user already confirmed. Asked first, not
    /// after: an account's first sign-in is refused without one, and Apple
    /// only ever offers a name on the very first authorization — which a
    /// second device, or a reinstall, never sees.
    public func signInWithApple() async throws {
        guard displayNameConfirmed else { throw FamilySyncError.displayNameConfirmationRequired }
        do {
            _ = try await coordinator.signInWithApple(displayName: profile.displayName)
        } catch FamilySyncError.displayNameConfirmationRequired {
            displayNameConfirmed = false
            throw FamilySyncError.displayNameConfirmationRequired
        }
        if case .shared = coordinator.mode {
            // A new session: the server may have swept this device's token
            // with the old one.
            owePushUpload()
            // Fresh Apple proof clears the old engine's sign-in-required
            // state even when the family key and recovery epoch are unchanged.
            await stopEngine()
            // Activate (including any access-token renewal) before loading
            // caregiver details. Polling and its live socket must not race
            // those reads with a token from the just-replaced session.
            buildEngineForCurrentSession(startPolling: false)
            defer { if isForeground { engine?.startForegroundPolling() } }
            await activate()
            try await reloadFamily()
        }
    }

    public func refreshAppleCredentialState() async throws -> AppleCredentialState {
        let syncedKey = engineKey
        let codeStore = invitationCodeStore
        // Stopped first because a revoked answer purges the shared realm; a
        // still-authorized one gets its engine straight back.
        let state = try await withSyncStopped { try await coordinator.refreshAppleCredentialState() }
        if state.endsAuthorization {
            // Only known afterwards, so the DELETE may be refused; the
            // registrar forgets the session either way.
            await endPush()
            // The shared realm was just purged, so a later sign-in has to
            // start from the family's records like any new device.
            leaveFamilySync(forgetting: syncedKey)
            members = []
            clearInvitationsState()
            confirmationCopy = FamilyLifecycleCopy.selfHostRevocation
            try? codeStore.removeAll()
        }
        return state
    }

    // MARK: Profile

    public func confirmDisplayName(_ name: String) async throws {
        let trimmed = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { throw FamilySyncError.displayNameConfirmationRequired }
        profile = CaregiverProfile(accountID: profile.accountID, displayName: trimmed, role: profile.role)
        displayNameConfirmed = true
        DisplayNameRecord.save(trimmed, to: defaults)
    }

    public func updateDisplayName(_ name: String) async throws {
        let trimmed = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { throw FamilySyncError.displayNameConfirmationRequired }
        profile = try await client.updateDisplayName(trimmed)
        displayNameConfirmed = true
        DisplayNameRecord.save(profile.displayName, to: defaults)
    }

    // MARK: Family membership

    public func createFamily(name: String) async throws {
        guard displayNameConfirmed else { throw FamilySyncError.displayNameConfirmationRequired }
        let displayName = profile.displayName
        let membership = try await renewingAccountSession {
            try await self.client.createFamily(name: name, displayName: displayName)
        }
        try coordinator.promoteAfterMembership(membership)
        try await enterFamily(as: .owner)
    }

    /// Accepting is the *only* step that redeems an invitation. Parsing a link,
    /// scanning a QR code and pasting a code all stop short of this on purpose.
    public func acceptInvitation(_ invitation: InvitationIntent) async throws {
        guard displayNameConfirmed else { throw FamilySyncError.displayNameConfirmationRequired }
        guard let endpoint = coordinator.confirmedEndpoint, invitation.origin == endpoint.origin else {
            throw FamilySyncError.endpointConfirmationRequired
        }
        let displayName = profile.displayName
        let membership = try await renewingAccountSession {
            try await self.client.confirmJoin(token: invitation.token, displayName: displayName)
        }
        try coordinator.promoteAfterMembership(membership)
        try await enterFamily(as: .joiner)
    }

    /// The join field's entry point: `code` may be a bare invitation code or
    /// a full pasted link. Either way it becomes an `InvitationIntent` before
    /// anything is sent, and `acceptInvitation(_:)` below still enforces the
    /// origin check — a bare code is paired with this device's own confirmed
    /// endpoint, so pairing one cannot be a way around it.
    public func acceptInvitation(code: String) async throws {
        guard let endpoint = coordinator.confirmedEndpoint else {
            throw FamilySyncError.endpointConfirmationRequired
        }
        let intent = try InvitationParser.intent(fromCodeOrLink: code, confirmedOrigin: endpoint.origin)
        try await acceptInvitation(intent)
    }

    public func createInvitation() async throws -> FamilyInvitation {
        try await createInvitation(role: .caregiver)
    }

    /// Invites someone who may read the family's history and change none of it.
    ///
    /// A distinct link rather than "invite, then demote": between joining and
    /// being demoted an ordinary caregiver can write, and on a shared family
    /// that means records a parent never meant to let that device create.
    public func inviteViewer() async throws -> FamilyInvitation {
        try await createInvitation(role: .viewer)
    }

    /// Mints the invitation, saves its link under this phone's own copy —
    /// the only place it can ever be read back from, since the server only
    /// answers with it once — and reloads the pending list so the new
    /// invitation shows up straight away.
    public func createInvitation(role: FamilyRole) async throws -> FamilyInvitation {
        guard self.role.canInvite else { throw FamilySyncError.ownerOnly }
        let invitation = try await client.createInvitation(role: role)
        try? invitationCodeStore.save(invitation.shareURL, for: invitation.id)
        await reloadInvitations()
        return invitation
    }

    /// Promotes or demotes another caregiver. Owner-only, and refused for the
    /// owner's own row — stepping down is `transferOwnership`, which hands the
    /// family to a named successor rather than leaving it without an owner.
    public func changeRole(of memberID: UUID, to newRole: FamilyRole) async throws {
        guard role.canManageMembers else { throw FamilySyncError.ownerOnly }
        guard memberID != profile.accountID else { throw FamilySyncError.ownerTransferRequired }
        try await perform(.changeRole(memberID, newRole))
        try await reloadFamily()
    }

    public func invitationShareURL(_ invitation: FamilyInvitation) throws -> URL {
        invitation.shareURL
    }

    /// This phone's own saved copy of an invitation's link, if it ever had
    /// one — nil for a code minted on another phone, or one this phone can no
    /// longer read back after a reinstall.
    public func shareURL(for invitationID: UUID) -> URL? {
        invitationCodeStore.shareURL(for: invitationID)
    }

    /// Refreshes the pending list from the server and prunes saved codes to
    /// match it. Never throws: a caregiver who is offline keeps the last list
    /// they saw, with `invitationsRefreshFailed` set so the screen can offer
    /// a retry instead of going blank.
    public func reloadInvitations() async {
        do {
            let invitations = try await client.pendingInvitations()
            pendingInvitations = invitations
            invitationsRefreshFailed = false
            try? invitationCodeStore.prune(keeping: Set(invitations.map(\.id)))
        } catch {
            invitationsRefreshFailed = true
        }
    }

    public func reloadMembers() async throws {
        try await reloadFamily()
    }

    public func previewInvitation(_ intent: InvitationIntent) async throws -> InvitationPreview {
        try await client.previewInvitation(token: intent.token)
    }

    /// Revokes a pending invitation. The server may already consider it gone
    /// — redeemed while this screen was open, revoked from another device, or
    /// simply expired — and none of those are this caller's problem to see as
    /// a failure: the invitation is unusable either way, which is all "revoke"
    /// ever promised.
    public func revokeInvitation(_ invitationID: UUID) async throws {
        do {
            try await client.revokeInvitation(invitationID)
        } catch let error as HTTPClientError where Self.invitationAlreadyGone(error) {
            // Already unusable server-side — fall through to the same cleanup
            // a successful revoke gets.
        }
        try? invitationCodeStore.remove(invitationID)
        await reloadInvitations()
    }

    private static func invitationAlreadyGone(_ error: HTTPClientError) -> Bool {
        guard case .status(_, let code) = error else { return false }
        return ["invitation_consumed", "invitation_revoked", "invitation_invalid", "invitation_expired"].contains(code)
    }

    // MARK: Leaving and disconnecting

    public func disconnectCurrentFamily() async throws {
        confirmationCopy = FamilyLifecycleCopy.disconnect
        guard case .shared(let familyKey) = coordinator.mode else { return }
        // Read before the purge below can move `store.activeRealm` back to
        // `.local` — this device's saved invitation codes belong to the
        // family it is about to leave, not whatever realm it lands on next.
        let codeStore = invitationCodeStore
        // While the session can still authenticate the DELETE. A disconnect
        // that fails brings sync back, and its activation uploads again.
        await endPush()
        try await withSyncStopped {
            try await coordinator.revokeDeviceBindingAndSessions(familyKey, client: client)
        }
        leaveFamilySync(forgetting: familyKey)
        members = []
        clearInvitationsState()
        try? codeStore.removeAll()
    }

    public func leaveFamily() async throws {
        confirmationCopy = FamilyLifecycleCopy.leave
        let codeStore = invitationCodeStore
        var serverLeft = false
        await endPush()
        try await withSyncStopped(restartUnless: { serverLeft }) {
            try await client.leaveFamily()
            serverLeft = true
            if case .shared(let familyKey) = coordinator.mode {
                try coordinator.finishFamilyCleanup(familyKey)
                leaveFamilySync(forgetting: familyKey)
            }
        }
        members = []
        clearInvitationsState()
        try? codeStore.removeAll()
    }

    // MARK: Owner controls

    public func removeMember(_ memberID: UUID) async throws {
        try await perform(.removeMember(memberID))
        try await reloadFamily()
    }

    /// Whether the owner may offer `member` "Make owner". The server allows
    /// transfer only to a caregiver — offering it for a viewer would just be
    /// a confirmation sheet that always fails.
    public func canMakeOwner(_ member: FamilyMember) -> Bool {
        isOwner && member.familyRole == .caregiver
    }

    public func isMe(_ member: FamilyMember) -> Bool {
        member.id == profile.accountID
    }

    public func confirmation(for action: MemberAction, member: FamilyMember) -> FamilyConfirmationCopy {
        switch action {
        case .remove: .remove(member)
        case .makeOwner: .makeOwner(member)
        case .changeRole(let newRole): .changeRole(member, to: newRole)
        }
    }

    /// Hands the family over. The invitations stay live and are the new
    /// owner's to see and revoke through the server's list; this phone, no
    /// longer an owner, never reloads that list again to prune its own kept
    /// codes, so it lets go of them here.
    public func transferOwnership(to memberID: UUID) async throws {
        try await perform(.transferOwnership(memberID))
        profile = CaregiverProfile(accountID: profile.accountID, displayName: profile.displayName, role: "caregiver")
        clearInvitationsState()
        try? invitationCodeStore.removeAll()
        try await reloadFamily()
    }

    /// An active owner cannot delete their account: doing so would leave a
    /// family with records and caregivers and nobody able to administer it.
    public func deleteAccount() async throws {
        confirmationCopy = FamilyLifecycleCopy.deleteAccount
        if isOwner, hasFamily || isRecoveryOnly { throw FamilySyncError.ownerTransferRequired }
        let codeStore = invitationCodeStore
        var serverDeleted = false
        await endPush()
        try await withSyncStopped(restartUnless: { serverDeleted }) {
            try await perform(.deleteAccount)
            serverDeleted = true
            onAccountDeleted?(profile.accountID)
            if case .shared(let familyKey) = coordinator.mode {
                try await coordinator.revokeDeviceBindingAndSessions(familyKey, client: client)
                leaveFamilySync(forgetting: familyKey)
            }
        }
        members = []
        clearInvitationsState()
        try? codeStore.removeAll()
    }

    // MARK: Family lifecycle

    public func scheduleFamilyDeletion() async throws {
        confirmationCopy = FamilyLifecycleCopy.scheduleDeletion
        let codeStore = invitationCodeStore
        try await client.scheduleFamilyDeletion()
        members = []
        clearInvitationsState()
        try? codeStore.removeAll()
    }

    public func recoverFamily() async throws {
        members = try await client.recoverFamily()
        coordinator.completeRecovery()
        profile = CaregiverProfile(accountID: profile.accountID, displayName: profile.displayName, role: "owner")
        // A recovery-only session had no family session, so the server's
        // sweep may have dropped this device's token meanwhile.
        owePushUpload()
        await installEngineForCurrentSession()
        await activate()
    }

    public func forfeitFamilyRecovery() async throws {
        var forfeited: FamilySessionKey?
        if case .recovery(let familyKey) = coordinator.mode { forfeited = familyKey }
        let codeStore = invitationCodeStore
        await endPush()
        try await client.forfeitFamilyRecovery()
        coordinator.forfeitRecovery()
        leaveFamilySync(forgetting: forfeited)
        members = []
        clearInvitationsState()
        try? codeStore.removeAll()
    }

    // MARK: Running sync

    /// Launch, before any view appears: resumes the family session the
    /// Keychain still holds, so a caregiver who force-quit keeps syncing.
    /// No network here — `startSyncing` does that once the scene is up.
    public func restoreSession() {
        guard coordinator.restoreFamilySession() != nil else { return }
        if let endpoint = coordinator.confirmedEndpoint {
            // The family operations (members, invitations) need somewhere to
            // go too, not only sync.
            (client as? DeferredFamilyClient)?.endpointConfirmed(endpoint)
        }
        // Nothing to replace at launch, so no engine needs stopping first.
        buildEngineForCurrentSession()
    }

    /// The launch sync. Polling starts *first*: a remote timer only gets its
    /// Lock Screen card from a sync that runs while polling does, and the
    /// launch sync is the one most likely to bring one.
    ///
    /// Activation rather than a bare sync, every launch: it is the only place
    /// an owner's import is finished, and for a device already activated it
    /// is one ordinary sync.
    public func startSyncing() async {
        isForeground = true
        guard let engine else { return }
        engine.startForegroundPolling()
        reconcileLiveActivities()
        await activate()
        reconcileLiveActivities()
        // Best effort: the member list and this device's role, which a cold
        // launch otherwise does not know until the next family operation.
        do { try await reloadFamily() } catch {
            AppLog.sync.error("family reload failed: \(Self.logKind(of: error), privacy: .public)")
        }
    }

    public func sceneBecameActive() async {
        isForeground = true
        guard let engine else { return }
        engine.startForegroundPolling()
        reconcileLiveActivities()
        if activating { return }
        if activationPending {
            await activate()
        } else {
            await engine.syncNow(.foreground)
        }
        reconcileLiveActivities()
    }

    public func sceneEnteredBackground() {
        isForeground = false
        engine?.stopForegroundPolling()
    }

    /// After every committed store write. The engine decides whether anything
    /// is actually owed, so a remote apply bumping the revision costs nothing.
    public func localWriteHappened() {
        engine?.localWriteHappened()
    }

    /// A background push: one sync through the engine's ordinary path, as a
    /// live hint would run it. True when the engine ended up to date.
    ///
    /// An engine that has not activated in this process activates instead,
    /// as `sceneBecameActive` does. That is the case for a push that relaunched
    /// the app in the background: the session was restored at launch, but
    /// with no scene nothing called `startSyncing`. Activation is that
    /// launch's one pull, and it finishes an owner's import. One already
    /// running is joined through `syncNow` instead of started twice. Polling
    /// is not started: there is no scene to poll for.
    public func syncNowForBackgroundPush() async -> Bool {
        guard case .shared = mode, let engine else { return false }
        if activationPending && !activating {
            await activate()
        } else {
            await engine.syncNow(.backgroundPush)
        }
        if case .upToDate = engine.status { return true }
        return false
    }

    /// Waits for the push work started so far. For tests: activation hands
    /// push its session without waiting for the upload.
    func pushSettled() async {
        await pushWork?.value
    }

    // MARK: Internals

    /// Scoped to `store.activeRealm`, computed fresh on every use rather than
    /// held: the realm this device is in can change (joining, recovering,
    /// leaving), and a stale store would read or write the wrong family's
    /// codes.
    private var invitationCodeStore: InvitationCodeStore {
        InvitationCodeStore(backend: codeStoreBackend, realm: store.activeRealm)
    }

    /// Every place `members` is cleared because this device just left,
    /// disconnected, or lost its family clears the invitation state the same
    /// way: a screen still showing yesterday's pending invitations, or a
    /// stale refresh-failed flag, after the family itself is gone would be
    /// its own small lie.
    private func clearInvitationsState() {
        pendingInvitations = []
        invitationsRefreshFailed = false
    }

    /// Creating or joining, renewing the pre-family session once if it lapsed.
    ///
    /// The session a sign-in leaves behind is account-scoped and lives ten
    /// minutes. Nothing renewed it — the engine only renews a family's — so
    /// someone who signed in and came back later to create or join got a 401
    /// every time, until they signed in with Apple again.
    private func renewingAccountSession<T>(_ work: () async throws -> T) async throws -> T {
        do {
            return try await work()
        } catch let error as HTTPClientError where error == .status(401, "access_expired") {
            guard let renewAccountSession, case .authenticated(let key) = coordinator.mode,
                  let endpoint = coordinator.confirmedEndpoint else { throw error }
            try await renewAccountSession(key, endpoint)
            return try await work()
        }
    }

    private func enterFamily(as entry: FamilyEntry) async throws {
        guard case .shared(let familyKey) = coordinator.mode else { return }
        owePushUpload()
        // Recorded before anything else can fail: how this device entered is
        // what every later launch activates by, and guessing it wrong either
        // re-imports nothing for an owner or uploads a joiner's own history.
        FamilyEntryRecord.save(entry, for: familyKey, in: defaults)
        await installEngineForCurrentSession()
        try await reloadThenActivate()
    }

    /// Activation failures are the engine's status line, not the caller's
    /// error: membership already exists on the server and must not look
    /// undone. A failed member reload still surfaces, but only after
    /// activation has had its turn.
    private func reloadThenActivate() async throws {
        var reloadFailure: (any Error)?
        do { try await reloadFamily() } catch { reloadFailure = error }
        await activate()
        if let reloadFailure { throw reloadFailure }
    }

    /// Builds the engine for the current `.shared` session, stopping one
    /// bound to a different key first.
    private func installEngineForCurrentSession() async {
        guard case .shared(let familyKey) = coordinator.mode else { return }
        if engine != nil, engineKey == familyKey { return }
        await stopEngine()
        buildEngineForCurrentSession()
    }

    private func buildEngineForCurrentSession(startPolling: Bool = true) {
        guard engine == nil, let makeEngine, case .shared(let familyKey) = coordinator.mode,
              let endpoint = coordinator.confirmedEndpoint else { return }
        let fresh = makeEngine(familyKey, endpoint)
        engine = fresh
        engineKey = familyKey
        // A new engine has not activated yet, whatever the last one did.
        activationPending = true
        if isForeground && startPolling { fresh.startForegroundPolling() }
    }

    /// Runs the activation matching how this device entered the family.
    ///
    /// By entry, not by the server's current role: a caregiver later made
    /// owner joined fresh and has no pre-family history to import, and an
    /// owner who handed ownership on still has to finish their own import.
    private func activate() async {
        guard let engine, let familyKey = engineKey else { return }
        activating = true
        defer { activating = false }
        let pushGenerationBefore = pushGeneration
        do {
            switch FamilyEntryRecord.entry(for: familyKey, in: defaults) {
            case .owner: try await engine.activateAsOwner()
            case .joiner, nil: try await engine.activateAsJoiner()
            }
            activationPending = false
            if let endpoint = coordinator.confirmedEndpoint,
               mayHandPushTheSession(familyKey, endedSince: pushGenerationBefore) {
                startPush(familyKey, endpoint, nextPushActivation)
            }
            nextPushActivation = .restored
        } catch {
            // Not the caller's error — the status line shows it — but the
            // one place that says why a phone is not syncing.
            AppLog.sync.error("family activation failed: \(Self.logKind(of: error), privacy: .public)")
            activationPending = true
        }
    }

    /// What a log line may say about a failure: its kind, never its
    /// contents. A description can carry a URL, a name or a note.
    static func logKind(of error: any Error) -> String {
        switch error {
        case let code as SyncErrorCode: code.rawValue
        case let error as HTTPClientError: String(describing: error)
        case let error as URLError: "URLError \(error.code.rawValue)"
        default: String(reflecting: type(of: error))
        }
    }

    /// Whether an activation that just returned may hand push its session.
    ///
    /// An activation that "succeeded" does not prove the session is alive.
    /// A removal found by its own sync purges the realm, marks the account
    /// signed out and ends push from inside the engine call, and the call
    /// still returns normally. A later launch restores the same Keychain
    /// session, and the engine returns at once because the account may not
    /// sync. Neither may re-register the token: the upload would only be
    /// refused. The generation check covers any ending during the call; the
    /// account check is the same gate the engine itself uses.
    private func mayHandPushTheSession(_ familyKey: FamilySessionKey, endedSince generation: Int) -> Bool {
        guard pushGeneration == generation else { return false }
        return (try? store.accountAllowsOrdinarySync(familyKey.leaseKey)) == true
    }

    /// Hands push the session that just activated, after any push work
    /// already queued, without waiting for it. Dropped if push ended while it
    /// waited: that session is over.
    private func startPush(_ familyKey: FamilySessionKey, _ endpoint: ConfirmedEndpoint, _ activation: PushActivation) {
        guard let pushLifecycle else { return }
        let previous = pushWork
        let generation = pushGeneration
        pushWork = Task { @MainActor [weak self] in
            await previous?.value
            guard let self, self.pushGeneration == generation else { return }
            await pushLifecycle.activated(familyKey, endpoint, activation)
        }
    }

    /// Sign-out, leaving, removal: the registrar deletes the token and
    /// forgets the session. Awaited (the registrar bounds it), and before the
    /// engine stops, while the session can still authenticate the DELETE.
    /// `withSyncStopped`'s temporary stop never calls this.
    ///
    /// It does not wait for the push work before it. An upload stuck on the
    /// network would hold up leaving. The registrar already lands an upload
    /// in flight before its DELETE, within its own deadline. An activation
    /// still queued is dropped by the generation bump. Later push work still
    /// queues behind the ending, so a new session's upload follows the DELETE.
    private func endPush() async {
        await beginEndingPush()?.value
    }

    /// `endPush` without waiting for the DELETE: the generation bump and the
    /// queueing happen before this returns.
    private func beginEndingPush() -> Task<Void, Never>? {
        guard let pushLifecycle else { return nil }
        pushGeneration += 1
        let previous = pushWork
        let ending = Task { @MainActor in await pushLifecycle.ending() }
        pushWork = Task { @MainActor in
            await previous?.value
            await ending.value
        }
        return ending
    }

    /// Ordinary sync found this device removed from the family, and the
    /// engine has already purged the realm. Push ends in the same order as a
    /// leave, but is not awaited: the engine calls this mid-sync, and the
    /// DELETE will be refused anyway (the session is gone).
    func membershipEndedBySync() {
        _ = beginEndingPush()
    }

    /// A sign-in, join, re-authentication or recovery has begun. The upload
    /// is owed from now: the next activation is `.signedIn`, and the stored
    /// fingerprint is forgotten at once. If the activation fails and the app
    /// is killed, the next launch's `.restored` activation still uploads.
    private func owePushUpload() {
        nextPushActivation = .signedIn
        pushLifecycle?.invalidate()
    }

    /// Stops the engine and waits until nothing it started is in flight.
    private func stopEngine() async {
        guard let engine else { return }
        self.engine = nil
        engineKey = nil
        activationPending = false
        await engine.stop()
    }

    /// Runs `work` — a leave, a disconnect, a revocation check — with sync
    /// stopped, because each of them can purge the shared realm and a sync
    /// still in flight would write its pages straight back into it.
    ///
    /// Whatever `work` leaves behind decides what happens next: still in the
    /// family (it failed, or Apple still authorizes the account) and the
    /// engine comes back and activates; out of it, and it stays gone.
    ///
    /// `restartUnless` covers the case the mode cannot see: the server has
    /// already let this device go and only the local cleanup after it threw.
    /// Sync must not come back for a family the server says it left.
    private func withSyncStopped<T>(
        restartUnless membershipEnded: () -> Bool = { false },
        _ work: () async throws -> T
    ) async throws -> T {
        await stopEngine()
        let result: Result<T, any Error>
        do { result = .success(try await work()) } catch { result = .failure(error) }
        if !membershipEnded() { buildEngineForCurrentSession() }
        if engine != nil { await activate() }
        return try result.get()
    }

    /// Forgets how this device entered a family it has left, so a later
    /// sign-in starts from the family's records like any new device.
    private func leaveFamilySync(forgetting familyKey: FamilySessionKey? = nil) {
        if let familyKey { FamilyEntryRecord.clear(for: familyKey, in: defaults) }
    }

    /// Gives every running timer without a Lock Screen card one.
    ///
    /// A card is only requested from a foreground sync — ActivityKit refuses
    /// one from the background — so a timer another caregiver started while
    /// this app was backgrounded arrived without one and would never get it.
    /// Family devices only: a local-only user's timers behave as before.
    private func reconcileLiveActivities() {
        guard engine != nil else { return }
        for baby in (try? store.babies()) ?? [] {
            for timer in (try? store.activeTimers(for: baby.id)) ?? [] where timer.liveActivityID == nil {
                startLiveActivity(timer, store)
            }
        }
    }

    private func perform(_ command: FamilyLifecycleCommand) async throws {
        try await client.perform(command)
        lastLifecycleCommands.append(command)
    }

    private func reloadFamily() async throws {
        profile = try await client.profile()
        members = try await client.members()
        displayNameConfirmed = displayNameConfirmed || !profile.displayName.isEmpty
        if !profile.displayName.isEmpty { DisplayNameRecord.save(profile.displayName, to: defaults) }
        // The server is the authority on the role; this mirrors its answer into
        // the durable gate so the widget and App Intents — which never make
        // this call — honour a demotion too.
        if case .shared(let familyKey) = coordinator.mode {
            try store.setWritePermission(profile.familyRole, for: familyKey.realmID)
        }
    }
}

/// How this device entered a family: by creating it, bringing its history,
/// or by joining it, starting from the family's records.
enum FamilyEntry: String, Codable, Sendable {
    case owner, joiner
}

/// Remembers `FamilyEntry` per family session, across launches.
///
/// Not the server's role, which changes with an ownership transfer, and not
/// the session record, which Apple sign-in rewrites. Keyed without the
/// recovery epoch, which changes under a renewed session that is still the
/// same device in the same family. App Group defaults: it is not a secret.
enum FamilyEntryRecord {
    static let defaultsKey = "familySync.entry.v1"

    static func entry(for key: FamilySessionKey, in defaults: UserDefaults) -> FamilyEntry? {
        guard let raw = (defaults.dictionary(forKey: defaultsKey) as? [String: String])?[id(key)] else { return nil }
        return FamilyEntry(rawValue: raw)
    }

    static func save(_ entry: FamilyEntry, for key: FamilySessionKey, in defaults: UserDefaults) {
        var all = defaults.dictionary(forKey: defaultsKey) as? [String: String] ?? [:]
        all[id(key)] = entry.rawValue
        defaults.set(all, forKey: defaultsKey)
    }

    static func clear(for key: FamilySessionKey, in defaults: UserDefaults) {
        var all = defaults.dictionary(forKey: defaultsKey) as? [String: String] ?? [:]
        all[id(key)] = nil
        defaults.set(all, forKey: defaultsKey)
    }

    private static func id(_ key: FamilySessionKey) -> String {
        [
            key.canonicalOrigin,
            key.serverInstanceID.uuidString.lowercased(),
            key.familyID.uuidString.lowercased(),
            key.accountID.uuidString.lowercased(),
            key.deviceID.uuidString.lowercased(),
        ].joined(separator: "|")
    }
}

/// Renews the session an account holds before it is in a family.
public typealias AccountSessionRenewal = @MainActor (AccountSessionKey, ConfirmedEndpoint) async throws -> Void

/// The display name the user confirmed, across launches. App Group defaults:
/// it is shown to the rest of the family, so it is not a secret.
enum DisplayNameRecord {
    static let defaultsKey = "familySync.displayName.v1"

    static func load(from defaults: UserDefaults) -> String? {
        guard let name = defaults.string(forKey: defaultsKey)?.trimmingCharacters(in: .whitespacesAndNewlines),
              !name.isEmpty else { return nil }
        return name
    }

    static func save(_ name: String, to defaults: UserDefaults) {
        defaults.set(name, forKey: defaultsKey)
    }
}
