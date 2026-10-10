import Foundation
import Observation
import SwiftData
import UIKit
import BabyCore
import BabyStore
import BabyUI
import BabyFeature
import BabySync
import SettingsFeature
import AskFeature

@MainActor
@Observable
final class AppEnvironment {
    /// Optional on purpose. When the container will not open there is no store
    /// to hand out, and inventing an in-memory stand-in would mean a parent
    /// could log entries into something that is thrown away on quit. The app
    /// renders `StoreFailureView` instead, and nothing ever reaches for a
    /// store that is not there.
    private(set) var store: BabyStore?
    /// Non-nil exactly when `store` is nil: the message the failure screen shows.
    private(set) var storeFailure: String?
    let selection = SelectedBabyStore()
    /// The one instance. Settings writes through it and every screen reads
    /// through it, so changing the unit is seen immediately — a second
    /// instance would persist the change and leave the rendered volumes alone.
    let settings: SettingsViewModel
    /// Non-nil exactly when the store opened: the Caregivers screen needs a
    /// store to purge and to gate sync against, and there is no honest
    /// stand-in for one.
    private(set) var familyModel: FamilySettingsViewModel?
    /// Where `AppDelegate` hands APNs callbacks. Only the production graph
    /// has them; the UI-test families have no server to give a token to.
    @ObservationIgnored private(set) var push: PushTokenRegistrar?
    @ObservationIgnored private(set) var wake: BackgroundWakeHandler?
    /// Registers ActivityKit push tokens with the built-in server, from launch
    /// and without any UI. Nil for UI-test families and a store that would
    /// not open.
    private(set) var liveActivityPush: LiveActivityPushDriver?
    let askConversations: AskConversationStore
    let askModel: any AskModel
    private let askDefaults: UserDefaults
    private let askLocalID: UUID
    private let askPostAccountLocalID: UUID

    private static var askDirectory: URL {
        FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appending(path: "AskConversations", directoryHint: .isDirectory)
    }

    var askIdentity: AskIdentity {
        switch familyModel?.mode {
        case .authenticated(let key): return .account(key.accountID)
        case .shared(let key), .recovery(let key): return .account(key.accountID)
        default:
            return .local(askDefaults.string(forKey: "AskFirstAccountID") != nil
                ? askPostAccountLocalID : askLocalID)
        }
    }

    func promoteAskConversation(from old: AskIdentity, to new: AskIdentity, babyID: UUID) throws {
        guard case .local(let localID) = old, case .account(let accountID) = new else { return }
        guard !askDefaults.bool(forKey: "AskHasUsedAccount") else { return }
        try askConversations.promote(localID: localID, accountID: accountID, babyID: babyID)
    }

    func promoteAllLocalAskConversations(to identity: AskIdentity) throws {
        guard let store else { return }
        guard case .account(let accountID) = identity else { return }
        markAskAccountEntered(identity)
        // A failed first promotion may be retried only by that account.
        // Another account must never inherit the original local transcript.
        guard askDefaults.string(forKey: "AskFirstAccountID") == accountID.uuidString else { return }
        guard !askDefaults.bool(forKey: "AskHasUsedAccount") else { return }
        // The visible view may already have switched to a family realm. Read
        // the local realm directly so every local child's chat is considered.
        for baby in try store.babies(in: .local) {
            try promoteAskConversation(from: .local(askLocalID), to: identity, babyID: baby.id)
        }
        finishAskLocalPromotion()
    }

    func markAskAccountEntered(_ identity: AskIdentity) {
        guard case .account(let accountID) = identity else { return }
        if askDefaults.string(forKey: "AskFirstAccountID") == nil {
            askDefaults.set(accountID.uuidString, forKey: "AskFirstAccountID")
        }
    }

    func finishAskLocalPromotion() {
        askDefaults.set(true, forKey: "AskHasUsedAccount")
    }

    /// Derived, not stored. `settings.volumeUnit` is what is persisted; this
    /// is the existing readers' name for it, so the five call sites that
    /// already read `environment.volumeUnit` pick the preference up unchanged.
    var volumeUnit: VolumeUnit { settings.volumeUnit }

    init() {
        // Before the preference is read, not after: `SettingsViewModel` loads
        // the stored unit once, in its own init, and a property initialiser
        // would have run before `openStore` wiped the App Group — which left a
        // UI test launched with `-uiTestResetStore` still in ounces from the
        // previous run.
        Self.resetForUITestsIfRequested()
        #if DEBUG
        let arguments = ProcessInfo.processInfo.arguments
        let isolatedHomeSyncFixture = UITestFamily.isHomeSyncNeedsSignInRequested
        #else
        let isolatedHomeSyncFixture = false
        #endif
        // The opt-in sync fixture must select its Ask namespace before any
        // identity lookup or sign-in promotion can touch real Ask state.
        let askDefaults = isolatedHomeSyncFixture
            ? UserDefaults(suiteName: "com.olhapi.babytracker.uiTestHomeSyncAsk-\(UUID().uuidString)")!
            : UserDefaults.standard
        self.askDefaults = askDefaults
        let askRoot = isolatedHomeSyncFixture
            ? FileManager.default.temporaryDirectory.appending(path: "HomeSyncAsk-\(UUID().uuidString)", directoryHint: .isDirectory)
            : Self.askDirectory
        askConversations = AskConversationStore(root: askRoot)
        #if DEBUG
        if isolatedHomeSyncFixture {
            askModel = UITestAskModel(available: false)
        } else if arguments.contains("-uiTestAskFakeModel") || arguments.contains("-uiTestAskUnavailableModel") {
            askModel = UITestAskModel(available: !arguments.contains("-uiTestAskUnavailableModel"))
        } else {
            askModel = AppleAskModel()
        }
        #else
        askModel = AppleAskModel()
        #endif
        if let raw = askDefaults.string(forKey: "AskLocalInstallationID"),
           let id = UUID(uuidString: raw) {
            askLocalID = id
        } else {
            let id = UUID()
            askDefaults.set(id.uuidString, forKey: "AskLocalInstallationID")
            askLocalID = id
        }
        if let raw = askDefaults.string(forKey: "AskPostAccountLocalID"),
           let id = UUID(uuidString: raw) {
            askPostAccountLocalID = id
        } else {
            let id = UUID()
            askDefaults.set(id.uuidString, forKey: "AskPostAccountLocalID")
            askPostAccountLocalID = id
        }
        settings = SettingsViewModel(defaults: isolatedHomeSyncFixture ? askDefaults : nil)
        openStore()
    }

    /// Test-only, compiled out of release. Empties the App Group's defaults;
    /// the SwiftData database is emptied separately, through `ModelContext`.
    private static func resetForUITestsIfRequested() {
        #if DEBUG
        guard !UITestFamily.isHomeSyncNeedsSignInRequested,
              ProcessInfo.processInfo.arguments.contains("-uiTestResetStore") else { return }
        let defaults = UserDefaults(suiteName: BabySchema.appGroupID) ?? .standard
        defaults.removePersistentDomain(forName: BabySchema.appGroupID)
        try? FileManager.default.removeItem(at: askDirectory)
        // Before the container is opened, so a store written by a different
        // schema is never opened at all.
        discardUITestStore()
        #endif
    }

    /// The failure screen's only action. It reopens the same file — it does not
    /// delete or reset it. Like every open, it moves the store aside only if
    /// another schema wrote it. A transient cause (the device still locked on
    /// first unlock, a file coordination collision with the widget) clears on a
    /// second attempt; a permanent one shows the screen again, unchanged.
    func retry() {
        guard store == nil else { return }
        openStore()
    }

    private func openStore() {
        let arguments = ProcessInfo.processInfo.arguments
        #if DEBUG
        Self.resetForUITestsIfRequested()
        #endif

        let container: ModelContainer
        do {
            // Only the app moves a store another schema wrote: renamed aside,
            // never deleted, and a fresh store opened that sync repopulates
            // (ruling B-R7). The widget and intents refuse such a store.
            #if DEBUG
            // This fixture owns a disposable store from the moment it opens;
            // it never loads or resets the simulator's App Group database.
            container = try BabySchema.container(
                inMemory: UITestFamily.isHomeSyncNeedsSignInRequested, ifIncompatible: .moveAside
            )
            #else
            container = try BabySchema.container(inMemory: false, ifIncompatible: .moveAside)
            #endif
        } catch {
            // No trap. The on-disk store is left untouched — no automatic
            // reset, no "clear and continue". A parent's log is not ours to
            // discard. Our own copy is `.public`; the error's description can
            // name a rejected entity and property, which is entry content by
            // another route, so it stays `.private`.
            AppLog.store.fault(
                "\(StoreFailureCopy.title, privacy: .public): \(error.localizedDescription, privacy: .private)"
            )
            self.store = nil
            self.storeFailure = StoreFailureCopy.title
            return
        }

        finishOpening(container, arguments: arguments)
    }

    private func finishOpening(_ container: ModelContainer, arguments: [String]) {
        // Spec §1.4 asks for container open *and* failure. The open is the
        // line that tells a support thread the store was reached at all —
        // without it, a silent launch and a failed one look identical in the
        // log. No identifiers: which App Group opened is not a parent's data,
        // and nothing about the contents is said.
        AppLog.store.info("store opened")

        #if DEBUG
        let storeDeviceID = arguments.contains(UITestFamily.homeSyncNeedsSignInArgument)
            ? "task7-home-sync-fixture" : Self.deviceID
        #else
        let storeDeviceID = Self.deviceID
        #endif
        let store = BabyStore(
            container: container,
            dateProvider: SystemDateProvider(),
            deviceID: storeDeviceID
        )
        // Writes from the widget and the App Intents run against their own
        // container, so they never call `didWrite()` here. This is what makes
        // them visible to a running app.
        store.observeRemoteChanges()
        self.store = store
        self.storeFailure = nil
        // The Keychain access group matches the App Group so the widget and
        // App Intents read one set of session items, not three.
        let (familyModel, graph) = Self.makeFamilyModel(store: store)
        push = graph?.push
        wake = graph?.wake
        familyModel.onAccountDeleted = { [askConversations] accountID in
            do { try askConversations.removeIdentity(.account(accountID)) }
            catch { AppLog.store.error("Ask account cleanup failed: \(error.localizedDescription, privacy: .private)") }
        }
        // Local only. No request is made until `RootView` starts syncing, or
        // a background push asks for its one pull. A device with no family
        // restores nothing.
        familyModel.restoreSession()
        self.familyModel = familyModel
        // A restored family asks APNs for the token now rather than at the
        // first activation: it is the launch's fingerprint check that needs it.
        if case .shared = familyModel.mode, push != nil {
            UIApplication.shared.registerForRemoteNotifications()
        }
        #if DEBUG
        // Registration on a simulator without a family, which never registers
        // otherwise: SpringBoard asks apsd for a per-app token. It does not
        // make `xcrun simctl push` reach the delegate. On iOS 26.5 the
        // simulator delivers a content-available push as a background fetch,
        // which UIKit never routes to `didReceiveRemoteNotification`.
        if arguments.contains("-uiTestRegisterForPushes") {
            UIApplication.shared.registerForRemoteNotifications()
        }
        #endif
        self.liveActivityPush = graph?.liveActivityPush
        // After the restore, so the first evaluation sees the family session.
        // It probes the server and registers on its own; nothing here waits.
        if let liveActivityPush = graph?.liveActivityPush {
            Task { await liveActivityPush.launch() }
        }
        switch familyModel.mode {
        case .authenticated, .shared, .recovery:
            markAskAccountEntered(askIdentity)
            if !askDefaults.bool(forKey: "AskHasUsedAccount") {
                do {
                    try promoteAllLocalAskConversations(to: askIdentity)
                } catch {
                    AppLog.store.error("Ask local promotion failed: \(error.localizedDescription, privacy: .private)")
                }
            }
        case .signedOut(let key):
            markAskAccountEntered(.account(key.accountID))
        case .localOnly, .endpointConfirmed:
            break
        }
        do {
            let restored = try store.reconcileHiddenCanonicalCompletions()
            if restored > 0 {
                AppLog.store.notice("reconciled \(restored, privacy: .public) hidden canonical completions")
            }
        } catch {
            AppLog.store.error("canonical completion reconciliation failed: \(error.localizedDescription, privacy: .private)")
        }

        // Test-only store manipulation, compiled out of release entirely — see
        // `UITestSupport`. The SwiftData database lives in the App Group container
        // and survives `removePersistentDomain`, so a reset has to empty it too.
        // Done through `ModelContext`, never by touching the store file on disk.
        #if DEBUG
        if arguments.contains("-uiTestResetStore"), !UITestFamily.isHomeSyncNeedsSignInRequested {
            UITestSupport.emptyStore(store)
        }

        if arguments.contains("-uiTestSeedBaby"),
           (try? store.babies())?.isEmpty ?? true,
           let baby = try? store.addBaby(name: "Test Baby", birthDate: .now, referenceSex: .girls, expectedDueDate: nil) {
            selection.select(baby.id)
        }

        if arguments.contains("-uiTestFeedEndedSixMinutesAgo"),
           let baby = (try? store.babies())?.first {
            UITestSupport.seedFeedEndedSixMinutesAgo(store, babyID: baby.id)
        }

        if arguments.contains("-uiTestReadOnlyFamily"),
           let babyID = UITestSupport.seedReadOnlyFamily(store, babyName: "Test Baby") {
            selection.select(babyID)
        }

        if arguments.contains("-uiTestSeedFixture"),
           let baby = (try? store.babies())?.first {
            selection.select(baby.id)
            UITestSupport.seedFixture(store, babyID: baby.id)
        }

        // The read-only family seeds it itself, before the role turns viewer.
        if arguments.contains(UITestSupport.historySessionsArgument),
           !arguments.contains("-uiTestReadOnlyFamily"),
           let baby = (try? store.babies())?.first {
            selection.select(baby.id)
            UITestSupport.seedHistorySessions(store, babyID: baby.id)
        }

        // What a Lock Screen card action leaves when it could not verify the
        // family's current state (LiveActivityCardActions): only the parked
        // reason, never the action. The UI suite parks one as the intent would.
        if arguments.contains("-uiTestParkedActivityHandoff") {
            PendingQuickLogStore().parkActivityHandoff(.refreshFailed)
        }
        #endif
    }

    /// The production graph — or, for a UI test that asks for it, a signed-in
    /// owner — or a signed-in caregiver with no family yet — whose family
    /// server lives in memory (`UITestFamily`).
    ///
    /// The graph comes back only for the production model: it alone registers
    /// for pushes, background and Live Activity alike.
    private static func makeFamilyModel(store: BabyStore) -> (FamilySettingsViewModel, FamilySyncComposition.Graph?) {
        #if DEBUG
        if UITestFamily.isHomeSyncNeedsSignInRequested { return (UITestFamily.makeHomeSyncNeedsSignIn(store: store), nil) }
        if UITestFamily.isRequested { return (UITestFamily.makeOwnerFamily(store: store), nil) }
        if UITestFamily.isNoFamilyRequested { return (UITestFamily.makeNoFamily(store: store), nil) }
        #endif
        let graph = FamilySyncComposition.makeGraph(
            store: store, keychainAccessGroup: BabySchema.appGroupID,
            registerForRemoteNotifications: { UIApplication.shared.registerForRemoteNotifications() }
        )
        return (graph.model, graph)
    }

    #if DEBUG
    /// Removes the UI-test store before it is opened.
    ///
    /// Deleting up front rather than recovering after a failed open, because
    /// the failure itself is what costs the time: Core Data attempts its own
    /// recovery on an incompatible store, and that plus a delete plus a second
    /// open exceeded XCUITest's launch timeout. The suite then reported
    /// eleven runner restarts and eleven tests that never ran, with no failed
    /// assertion anywhere — a clean container passed 47/47 while the same
    /// build against a stale store lost a quarter of the suite.
    ///
    /// `-uiTestResetStore` already means "give me a clean device", so there is
    /// nothing here worth the risk of keeping. It also makes each launch
    /// hermetic, which is what stopped the read-only fixture leaking its realm
    /// into every later test.
    private static func discardUITestStore() {
        // SwiftData puts the group store under Library/Application Support,
        // not at the container root, and the -wal and -shm siblings must go
        // with it — leaving either behind reopens the same broken store.
        let group = BabySchema.defaultGroupDirectory
        let searched = [group, group.appending(path: "Library/Application Support", directoryHint: .isDirectory)]
        var removed = 0
        for directory in searched {
            let contents = (try? FileManager.default.contentsOfDirectory(
                at: directory, includingPropertiesForKeys: nil
            )) ?? []
            for file in contents where file.lastPathComponent.hasPrefix("default.store") {
                try? FileManager.default.removeItem(at: file)
                removed += 1
            }
        }
        AppLog.store.info("discarded \(removed, privacy: .public) UI-test store files before opening")
    }
    #endif

    /// Stable per install. Identifies the origin of change-log entries so the
    /// future sync node can resolve concurrent edits.
    private static var deviceID: String {
        let defaults = UserDefaults(suiteName: BabySchema.appGroupID) ?? .standard
        if let existing = defaults.string(forKey: "deviceID") { return existing }
        let fresh = UUID().uuidString
        defaults.set(fresh, forKey: "deviceID")
        return fresh
    }
}
