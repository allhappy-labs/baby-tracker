import Foundation
import SwiftData
// For `NSPersistentStoreRemoteChange`: SwiftData is Core Data underneath, and
// that notification is the only cross-process write signal available to us.
import CoreData
import BabyCore

/// A bounded store boundary signal. The sync layer supplies any correlation;
/// this module never learns about a server, account, or diagnostic sink.
public struct StoreSyncMilestone: Sendable {
    public enum Phase: Sendable {
        case localCommit, remoteCommit, observablePublish
    }

    public let phase: Phase
    public let monotonicNanoseconds: UInt64
}

/// The current guarded write's classification. The write boundary owns its
/// lifetime, so a rollback cannot leak an intent into the next transaction.
final class StoreSyncWriteScope {
    var originatedCareWrite = false
    var appliedRemoteData = false
}

/// Owns the only ModelContext in the system. Nothing outside this module
/// touches SwiftData directly — features call intent-named methods instead.
@MainActor
@Observable
public final class BabyStore {
    public let container: ModelContainer
    public let context: ModelContext
    let dateProvider: DateProvider
    let deviceID: String

    /// How long a break may last before the next feed or sleep part starts a
    /// new session. Read at every join decision; tests replace it. A closure
    /// rather than a cached value because the limits can change mid-session
    /// in Settings and the next decision must see the new ones immediately.
    @ObservationIgnored public var breakLimits: () -> BreakLimits = { TrackingPreferences().limits }
    public let coordinationLockURL: URL
    @ObservationIgnored private var cachedWriteBoundary: AppGroupWriteBoundary?
    var writeBoundary: AppGroupWriteBoundary {
        if let cachedWriteBoundary { return cachedWriteBoundary }
        let boundary = AppGroupWriteBoundary(store: self, lockURL: coordinationLockURL)
        cachedWriteBoundary = boundary
        return boundary
    }

    public init(
        container: ModelContainer,
        dateProvider: DateProvider,
        deviceID: String,
        coordinationLockURL: URL? = nil
    ) {
        self.container = container
        self.context = ModelContext(container)
        self.dateProvider = dateProvider
        self.deviceID = deviceID
        self.coordinationLockURL = coordinationLockURL
            ?? BabySchema.defaultGroupDirectory.appending(path: "BabyTracker.coordination.lock")
    }

    /// A throwaway store with a throwaway coordination lock.
    ///
    /// The lock is per store, in the temporary directory. It used to fall back
    /// to the real App Group container, which meant every in-memory store in
    /// the test suite shared one lock file with every other — and with the
    /// real app installed on the same Mac. It also meant every test process
    /// touched another app's group container, which macOS guards with a
    /// "would like to access data from other apps" prompt: `open()` blocked
    /// on that dialog at 0% CPU, and the package gate appeared to hang for as
    /// long as nobody noticed the window.
    public static func inMemory(
        dateProvider: DateProvider,
        deviceID: String = "test-device"
    ) throws -> BabyStore {
        BabyStore(
            container: try BabySchema.container(inMemory: true),
            dateProvider: dateProvider,
            deviceID: deviceID,
            coordinationLockURL: FileManager.default.temporaryDirectory
                .appending(path: "BabyTracker-\(UUID().uuidString).coordination.lock")
        )
    }

    /// A snapshot in progress, if one is. In memory on purpose: a snapshot
    /// that does not finish is restarted, and half its pages are worthless
    /// without the rest.
    @ObservationIgnored var pendingSnapshot: PendingSnapshot?

    /// Timers another caregiver started, changed or ended, as remote apply
    /// found them, waiting for the app layer to announce them. In memory on
    /// purpose: a Live Activity is presentation, and one missed across a
    /// relaunch is simply not shown.
    @ObservationIgnored var remoteTimerTransitions: [RemoteTimerTransition] = []

    /// Bumped on every committed write. Observers that are otherwise polling —
    /// Home is the one that matters — watch this instead of re-querying on a
    /// timer, so a screen with nothing live on it does no work at all.
    public private(set) var revision = 0

    /// Opt-in, process-local timing hook. Never receives row contents or IDs.
    @ObservationIgnored public var onSyncMilestone: (@MainActor (StoreSyncMilestone) -> Void)?
    @ObservationIgnored var syncWriteScope: StoreSyncWriteScope?

    /// Called from every method that commits a write of its own rather than
    /// going through `record`.
    func didWrite() {
        revision &+= 1
    }

    /// Called after the App Group lock is released. A diagnostic sink may do
    /// work of its own, so it must never run inside the store's write boundary.
    func publishSyncMilestone(_ phase: StoreSyncMilestone.Phase?, committedAt: UInt64, publishedAt: UInt64) {
        guard let phase, let onSyncMilestone else { return }
        onSyncMilestone(StoreSyncMilestone(phase: phase, monotonicNanoseconds: committedAt))
        if phase == .remoteCommit {
            onSyncMilestone(StoreSyncMilestone(phase: .observablePublish, monotonicNanoseconds: publishedAt))
        }
    }

    /// The registered notification tokens, held together so they can be
    /// unregistered in one place.
    ///
    /// `nonisolated(unsafe)` because `deinit` is not main-actor isolated and
    /// must still unregister. It is only ever assigned from the main-actor
    /// methods below, and read in `deinit`, which by definition runs when no
    /// other reference survives.
    private nonisolated(unsafe) var remoteChanges: RemoteChangeSubscription?

    /// A write made by the widget or an App Intent runs against its own
    /// `ModelContainer`, so it never calls `didWrite()` here. Without this, a
    /// widget-logged diaper is invisible to a running app until something else
    /// forces a refresh.
    ///
    /// Two notifications, because one is not enough:
    ///
    /// - `NSPersistentStoreRemoteChange` is the cross-**process** one, and the
    ///   only one that can fire for a real user. SwiftData is Core Data
    ///   underneath and enables persistent history tracking, so the coordinator
    ///   posts this in every process with the App Group store open — which is
    ///   how the widget extension's write reaches a running app. It carries no
    ///   context identity, so the self-filter below cannot apply to it; our own
    ///   saves may therefore bump twice. That is a redundant re-fetch, not a
    ///   correctness bug, and it is preferred over a debounce, which would
    ///   trade a wasted fetch for a window in which the screen is wrong.
    /// - `ModelContext.didSave` is same-process only. It cannot fire today —
    ///   this store owns the only `ModelContext` in the app process, and its
    ///   own saves are filtered out — but it is what makes an App Intent that
    ///   one day runs in-process, against its own context, visible immediately
    ///   rather than at the next foreground.
    ///
    /// Registered synchronously rather than from inside a `Task`: a `Task` body
    /// does not run until the main actor next suspends, so a save that lands in
    /// that window is missed — which is exactly the window right after launch,
    /// when the widget's write is most likely to arrive.
    ///
    /// Idempotent: every screen may call it, but only the first call registers,
    /// so one save is one revision bump and one reload per screen.
    ///
    /// `center` is injectable so a test can post into a centre of its own:
    /// tests run in parallel in one process, and `.default` is shared, so
    /// counting bumps against it is not deterministic. Production always uses
    /// the default centre, which is where Core Data and SwiftData post.
    ///
    /// **UNVERIFIED IN PRODUCTION.** Every test over this posts
    /// `.NSPersistentStoreRemoteChange` itself, so they prove the handler is
    /// wired and nothing more. That Core Data ever posts it for a SwiftData
    /// store opened with `ModelConfiguration(groupContainer:)` — which needs
    /// persistent history tracking enabled — is not established anywhere in
    /// this repo. This mechanism has already been inert in production once,
    /// under four green tests of exactly this shape. Check 11 in
    /// `docs/verification/2026-09-19-widget-live-activity.md` is the one that
    /// settles it: widget Diaper tap with the app in the FOREGROUND, History
    /// expected to update without the app being backgrounded first.
    public func observeRemoteChanges(center: NotificationCenter = .default) {
        guard remoteChanges == nil else { return }

        let subscription = RemoteChangeSubscription(center: center)

        subscription.tokens.append(
            center.addObserver(
                forName: ModelContext.didSave,
                object: nil,
                queue: .main
            ) { [weak self] notification in
                // Only the identity of the saving context leaves the
                // notification. Its payload carries the inserted and updated
                // objects — entry contents — and is never read, logged, or
                // forwarded. Taken here rather than inside the isolated block
                // because `Notification` is not `Sendable`; an
                // `ObjectIdentifier` is.
                let source = (notification.object as AnyObject?).map(ObjectIdentifier.init)

                MainActor.assumeIsolated {
                    guard let self else { return }
                    // Our own saves already called `didWrite()` from `record`;
                    // bumping again would reload every screen twice per write.
                    guard source != ObjectIdentifier(self.context) else { return }
                    self.didWrite()
                }
            }
        )

        subscription.tokens.append(
            center.addObserver(
                forName: .NSPersistentStoreRemoteChange,
                object: nil,
                queue: .main
            ) { [weak self] _ in
                // Nothing at all is read off this notification — not even the
                // object. Its history tokens describe changed rows, which is
                // entry content by another route. Its arrival is the whole
                // signal: someone wrote, re-read.
                MainActor.assumeIsolated { self?.didWrite() }
            }
        )

        remoteChanges = subscription
    }

    /// Stops observing. `deinit` calls this too, so a store that is dropped —
    /// which in the test suite is most of them — leaves no live token behind.
    public func stopObservingRemoteChanges() {
        remoteChanges?.cancel()
        remoteChanges = nil
    }

    deinit {
        remoteChanges?.cancel()
    }

    public var now: Date { dateProvider.now }

    public var activeRealm: RealmID {
        let descriptor = FetchDescriptor<RealmTransitionCheckpoint>()
        return (try? context.fetch(descriptor).first).flatMap { RealmID(encodedKey: $0.activeRealmKey) } ?? .local
    }

    /// The choke point. Writes the change-log entry, the protocol mutation
    /// when the realm is shared, and saves — all in one transaction, so a
    /// mutation can never exist without its log entry or its outbox row.
    ///
    /// The whole thing runs inside `AppGroupWriteBoundary`, which holds the
    /// App Group advisory lock and rejects a write racing a migration, an
    /// import or a realm switch. Callers that are already inside a boundary
    /// must use `recordLocked` instead: the lock is not reentrant.
    public func record(
        _ operation: ChangeOperation,
        on entity: some ChangeLoggable,
        expectedWriteContext: RealmWriteContext? = nil
    ) throws {
        let expected = try expectedWriteContext ?? captureWriteContext()
        try withRealmWrite(expected: expected) {
            try self.recordLocked(operation, on: entity)
        }
    }

    /// The body of `record`, for callers holding the boundary already.
    func recordLocked(
        _ operation: ChangeOperation,
        on entity: some ChangeLoggable,
        emitMutation: Bool = true
    ) throws {
        do {
            if let supplement = entity as? SupplementEntry, operation != .delete {
                supplement.name = try savedSupplementName(supplement.name, for: supplement.babyID, excluding: supplement.id)
            }
            if let shot = entity as? ImmunizationEntry, operation != .delete {
                shot.vaccine = try savedVaccineName(shot.vaccine, excluding: shot.id)
            }
            let entry = ChangeLogEntry(
                entityID: entity.entityID,
                entityType: entity.entityTypeName,
                operation: operation,
                payload: try entity.payloadData(),
                timestamp: now,
                deviceID: deviceID,
                realm: activeRealm
            )
            context.insert(entry)
            if emitMutation { try appendEntityMutation(operation, on: entity) }

            AppLog.store.debug(
                "recorded \(operation.rawValue, privacy: .public) \(AppLog.describe(entryKind: entity.entityTypeName, id: entity.entityID), privacy: .public)"
            )
            if realmIsShared { syncWriteScope?.originatedCareWrite = true }
        } catch {
            // The line that makes a silent failure explicable afterwards. The
            // boundary owns the rollback.
            AppLog.store.error(
                "rolled back \(operation.rawValue, privacy: .public) \(AppLog.describe(entryKind: entity.entityTypeName, id: entity.entityID), privacy: .public): \(error.localizedDescription, privacy: .private)"
            )
            throw error
        }
    }

    public func changeLog() throws -> [ChangeLogEntry] {
        var descriptor = FetchDescriptor<ChangeLogEntry>()
        descriptor.sortBy = [SortDescriptor(\.timestamp, order: .forward)]
        return try context.fetch(descriptor)
    }

    /// Unsynced entries, oldest first. Nothing calls this in v1 — it exists so
    /// the seam is real rather than imagined.
    public func pendingChanges() throws -> [ChangeLogEntry] {
        var descriptor = FetchDescriptor<ChangeLogEntry>(
            predicate: #Predicate { $0.syncedAt == nil }
        )
        descriptor.sortBy = [SortDescriptor(\.timestamp, order: .forward)]
        return try context.fetch(descriptor)
    }
}

/// Holds the notification tokens for one store's observation.
///
/// A reference type so `BabyStore.deinit`, which is not main-actor isolated,
/// can unregister without reaching into isolated state. `@unchecked Sendable`
/// is sound here: `tokens` is only mutated from `observeRemoteChanges` and
/// `cancel`, both of which run on the main actor or in `deinit`, where no
/// other reference to the store — and so to this box — survives.
private final class RemoteChangeSubscription: @unchecked Sendable {
    private let center: NotificationCenter
    var tokens: [any NSObjectProtocol] = []

    init(center: NotificationCenter) {
        self.center = center
    }

    func cancel() {
        for token in tokens { center.removeObserver(token) }
        tokens = []
    }
}
