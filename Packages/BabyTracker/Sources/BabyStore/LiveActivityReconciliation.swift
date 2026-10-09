import Foundation
import os
import BabyCore
public import BabyProtocol

/// What to do with a pushed card, given what this phone can vouch for.
public enum LiveActivityReconciliationDecision: Equatable, Sendable {
    /// The store is at least as new as the card and names the same
    /// lifecycle: show the store's state.
    case apply
    /// The card shows something newer than the store, or the store's newer
    /// state is still a queued local edit: leave the card to the server.
    case preserveUntilSync
    /// The store is at least as new as the card but names another lifecycle:
    /// the card is taken over for the store's one.
    case adopt
    /// The card belongs to another realm.
    case end
}

/// The ordering rule between local cards and server pushes.
///
/// A pushed card carries the revision the server built it from. The phone
/// may overwrite it only with state the server has acknowledged — the durable
/// applied cursor — and never with a queued local edit: the server may still
/// refuse that edit, and if its state then equals what it already pushed it
/// sends nothing, which would leave the refused edit on the card for good.
public enum LiveActivityReconciliation {
    public static func decide(
        local: LiveActivityRevision?,
        remote: LiveActivityRevision?,
        hasPendingLocalMutation: Bool,
        sameScope: Bool
    ) -> LiveActivityReconciliationDecision {
        guard sameScope else { return .end }
        // A card no server has pushed to: nothing to protect, so today's
        // immediate local behaviour.
        guard let remote else { return .apply }
        // Nothing acknowledged here to compare with: the card stands.
        guard let local else { return .preserveUntilSync }
        if remote.familySequence > local.familySequence || hasPendingLocalMutation { return .preserveUntilSync }
        return local.generationID == remote.generationID ? .apply : .adopt
    }

    /// The running timer, or the open break's session, a baby and kind has
    /// right now.
    public struct Lifecycle: Sendable, Equatable {
        public let timerID: UUID?
        public let sessionID: UUID?

        public init(timerID: UUID?, sessionID: UUID?) {
            self.timerID = timerID
            self.sessionID = sessionID
        }

        public static let none = Lifecycle(timerID: nil, sessionID: nil)

        /// Whether a card claiming these references shows this lifecycle.
        func contains(timerID: UUID?, sessionID: UUID?) -> Bool {
            if let mine = self.timerID, mine == timerID { return true }
            if let mine = self.sessionID, mine == sessionID { return true }
            return false
        }
    }

    /// One scoped card of a realm, baby and kind, as the duplicate rule sees it.
    public struct Candidate: Sendable, Equatable {
        public let id: String
        /// The server may hold an active registration for it: its token was
        /// sent, whether or not the receipt has come back.
        public let isServerKnown: Bool
        public let generationID: UUID?
        public let timerID: UUID?
        public let sessionID: UUID?
        public let remote: LiveActivityRevision?

        public init(id: String, isServerKnown: Bool, generationID: UUID?, timerID: UUID?, sessionID: UUID?, remote: LiveActivityRevision?) {
            self.id = id
            self.isServerKnown = isServerKnown
            self.generationID = generationID
            self.timerID = timerID
            self.sessionID = sessionID
            self.remote = remote
        }
    }

    /// The one card a realm, baby and kind keeps.
    ///
    /// The server sends a lifecycle's updates to the smallest activity ID
    /// among its active registrations and ends the others itself, so among
    /// the registered cards of the current lifecycle the phone keeps exactly
    /// that one — whichever receipt came first. Without one: the card the
    /// running timer owns, then a pushed card (newest revision first), then
    /// the smallest ID. Deterministic, so two passes never disagree.
    public static func winner(among candidates: [Candidate], ownedID: String?, lifecycle: Lifecycle) -> String? {
        let current = candidates.filter { $0.isServerKnown && lifecycle.contains(timerID: $0.timerID, sessionID: $0.sessionID) }
        if let primary = current.map(\.id).min() { return primary }
        if let ownedID, candidates.contains(where: { $0.id == ownedID }) { return ownedID }
        return candidates.min { a, b in
            if a.isServerKnown != b.isServerKnown { return a.isServerKnown }
            let aSequence = a.remote.map { ($0.familySequence, $0.presentationVersion) }
            let bSequence = b.remote.map { ($0.familySequence, $0.presentationVersion) }
            switch (aSequence, bSequence) {
            case let (x?, y?) where x != y: return x > y
            case (.some, nil): return true
            case (nil, .some): return false
            default: return a.id < b.id
            }
        }?.id
    }
}

public extension LiveActivityRevision {
    /// The server's APNs timestamp — Unix seconds — as a date. Not to be
    /// confused with the content's own dates, which ActivityKit encodes as
    /// seconds since 2001.
    var presentationDate: Date { Date(timeIntervalSince1970: TimeInterval(presentationTimestamp)) }
}

/// The realm whose cards the server pushes to: one server instance, recovery
/// epoch and family.
public struct LiveActivityRealmFence: Sendable, Hashable {
    public let serverInstanceID: UUID
    public let recoveryEpoch: Int64
    public let familyID: UUID

    public init(serverInstanceID: UUID, recoveryEpoch: Int64, familyID: UUID) {
        self.serverInstanceID = serverInstanceID
        self.recoveryEpoch = recoveryEpoch
        self.familyID = familyID
    }

    /// A card's scope in this realm; nil for anything the contract refuses.
    public func scope(babyID: UUID, kindRaw: String) -> LiveActivityScope? {
        try? LiveActivityScope(serverInstanceID: serverInstanceID, recoveryEpoch: recoveryEpoch, familyID: familyID,
                               babyID: babyID, kindRaw: kindRaw)
    }

    public func contains(_ scope: LiveActivityScope) -> Bool {
        scope.matches(serverInstanceID: serverInstanceID, recoveryEpoch: recoveryEpoch, familyID: familyID)
    }

    /// Whether the store's realm is this family on this server.
    public func matches(_ realm: RealmID) -> Bool {
        guard case .shared(_, let instance, let family) = realm else { return false }
        return instance == serverInstanceID && family == familyID
    }
}

/// What the push registration coordinator tells the card controller, shared
/// by the process.
///
/// The coordinator (BabySync) owns the registration ledger; the controller
/// (here) owns the cards. This is the narrow seam between them: whether the
/// server pushes to the store's realm, which cards it holds a registration
/// for, which lifecycles the parent dismissed — and, the other way, which
/// cards the controller retired, so their end is never reported as a
/// dismissal.
public final class LiveActivityPushState: @unchecked Sendable {
    public static let shared = LiveActivityPushState()

    /// Whether the server pushes to a realm's cards.
    public enum Mode: Sendable, Equatable {
        /// Not known yet — a cold launch before the coordinator has a
        /// definitive answer. Cards with server content are left alone and
        /// no plain card is stacked beside a realm-scoped one.
        case unknown
        /// The server pushes to this realm's cards.
        case managed(LiveActivityRealmFence)
        /// A definitive no: capability absent, another server, no family, a
        /// sign-out, Live Activities switched off. The app takes the cards back.
        case withdrawn
    }

    /// One card's registration as last sent.
    public struct Registration: Sendable, Equatable {
        /// The server's canonical generation, or the one the card claimed.
        public let generationID: UUID?
        public let timerID: UUID?
        public let sessionID: UUID?
        /// The server's receipt came back.
        public let accepted: Bool

        public init(generationID: UUID?, timerID: UUID?, sessionID: UUID?, accepted: Bool) {
            self.generationID = generationID
            self.timerID = timerID
            self.sessionID = sessionID
            self.accepted = accepted
        }
    }

    /// The lifecycle a card shows, as the card controller last wrote it: the
    /// running timer and the effective feed session (the timer's, or the open
    /// break's). For a card no push has reached yet this is the only place
    /// those references exist; the coordinator registers the card with them so
    /// the server can adopt it into the lifecycle and push to it.
    public struct References: Sendable, Equatable {
        public let timerID: UUID?
        public let sessionID: UUID?

        public init(timerID: UUID?, sessionID: UUID?) {
            self.timerID = timerID
            self.sessionID = sessionID
        }
    }

    private struct State {
        var mode: Mode = .unknown
        var registrations: [String: Registration] = [:]
        var dismissed: Set<UUID> = []
        var retired: Set<String> = []
        /// The last stamp this process gave each card, so successive phone
        /// writes inside one server second still increase.
        var stamps: [String: Date] = [:]
        /// Per card, whatever the realm's mode: a card's lifecycle does not
        /// change because the coordinator is between answers.
        var references: [String: References] = [:]
    }

    private let lock = OSAllocatedUnfairLock(initialState: State())
    private let handler = OSAllocatedUnfairLock<(@MainActor @Sendable ([String]) -> Bool)?>(initialState: nil)
    private let referencesHandler = OSAllocatedUnfairLock<(@MainActor @Sendable () -> Void)?>(initialState: nil)

    public init() {}

    public var mode: Mode { lock.withLock { $0.mode } }

    /// The realm whose cards are push-managed; nil when none is (yet).
    public var fence: LiveActivityRealmFence? {
        if case .managed(let fence) = mode { return fence }
        return nil
    }

    public var isWithdrawn: Bool { mode == .withdrawn }

    /// Installs a realm as push-managed, or — nil — returns to not knowing.
    /// Anything published for another realm goes with it.
    public func install(_ fence: LiveActivityRealmFence?) {
        set(fence.map(Mode.managed) ?? .unknown)
    }

    /// The definitive no.
    public func withdraw() {
        set(.withdrawn)
    }

    private func set(_ mode: Mode) {
        lock.withLock { state in
            guard state.mode != mode else { return }
            state = State(mode: mode, retired: state.retired, stamps: state.stamps, references: state.references)
        }
    }

    /// Records the lifecycle the controller just requested, adopted or wrote
    /// on a card, and tells the coordinator when that changed — a changed
    /// reference is registered again with a newer token version.
    @MainActor
    public func setReferences(_ references: References, for activityID: String) {
        let changed = lock.withLock { state -> Bool in
            guard !state.retired.contains(activityID), state.references[activityID] != references else { return false }
            state.references[activityID] = references
            return true
        }
        guard changed, let handler = referencesHandler.withLock({ $0 }) else { return }
        handler()
    }

    /// The lifecycle the controller last gave this card; nil when it has not
    /// in this process.
    public func references(for activityID: String) -> References? {
        lock.withLock { $0.references[activityID] }
    }

    /// Called whenever a card's references change. Set by the coordinator.
    public func setReferencesHandler(_ handler: (@MainActor @Sendable () -> Void)?) {
        referencesHandler.withLock { $0 = handler }
    }

    public func publish(registrations: [String: Registration], dismissedLifecycles: Set<UUID>) {
        lock.withLock { state in
            state.registrations = registrations.filter { !state.retired.contains($0.key) }
            state.dismissed = dismissedLifecycles
        }
    }

    public func registration(for activityID: String) -> Registration? {
        lock.withLock { $0.registrations[activityID] }
    }

    /// Whether the parent dismissed the card of this timer or feed session.
    public func isDismissed(lifecycle id: UUID) -> Bool {
        lock.withLock { $0.dismissed.contains(id) }
    }

    /// Every card retired in this process.
    public var retired: Set<String> { lock.withLock { $0.retired } }

    /// Records retired cards durably and makes sure their end is never sent;
    /// true only once that is recorded. Set by the coordinator, or by a
    /// process that has only the registration ledger.
    public func setRetireHandler(_ handler: (@MainActor @Sendable ([String]) -> Bool)?) {
        self.handler.withLock { $0 = handler }
    }

    /// Whether something in this process — the coordinator, or a process that
    /// installed the provisional state — already records retirements.
    public var hasRetireHandler: Bool { handler.withLock { $0 != nil } }

    /// Called before the controller ends a card the server must not read as
    /// dismissed — a losing duplicate, or a card this phone's own mutation
    /// ends. False when the retirement could not be recorded durably: the
    /// caller then leaves the card up, since its end would reach the server
    /// as a dismissal of the lifecycle.
    @MainActor
    public func retire(_ ids: [String]) -> Bool {
        guard !ids.isEmpty else { return true }
        guard let handler = handler.withLock({ $0 }), handler(ids) else { return false }
        lock.withLock { state in
            state.retired.formUnion(ids)
            for id in ids {
                state.registrations[id] = nil
                state.references[id] = nil
            }
        }
        return true
    }

    /// The stamp for a phone write to a card the server may push to: half a
    /// second past the server's last presentation second on it, never later
    /// — the server's next push is at least the next whole second, so it
    /// always wins. A card with no server second yet gets the earliest stamp
    /// there is, which no push can be older than. Successive writes inside
    /// one second still increase.
    public func phoneStamp(for activityID: String, lastServerSecond: Int64?) -> Date {
        let base = TimeInterval(lastServerSecond ?? 0)
        return lock.withLock { state in
            var stamp = base + 0.5
            if let previous = state.stamps[activityID]?.timeIntervalSince1970, previous >= stamp, previous < base + 1 {
                stamp = min(previous + 0.001, base + 0.999)
            }
            let date = Date(timeIntervalSince1970: stamp)
            state.stamps[activityID] = date
            return date
        }
    }
}

/// Reads what this phone can vouch for: the durable applied family cursor,
/// and whether a timer, feed or sleep mutation is still queued.
///
/// It never manufactures acknowledgement for optimistic local state: a
/// queued edit is reported as pending, not folded into the sequence.
@MainActor
public struct LiveActivityRevisionReader {
    /// The generation a phone-built revision claims when the store has
    /// nothing running and no break open: it matches no server generation.
    static let noLifecycle = UUID(uuidString: "00000000-0000-5000-8000-000000000000")!

    let store: BabyStore

    public init(store: BabyStore) {
        self.store = store
    }

    /// The applied cursor, only if it is this realm's — same instance,
    /// family and recovery epoch.
    public func appliedCursor(in fence: LiveActivityRealmFence) -> Cursor? {
        guard fence.matches(store.activeRealm), let cursor = try? store.syncCursor(),
              cursor.serverInstanceID == fence.serverInstanceID, cursor.familyID == fence.familyID,
              cursor.recoveryEpoch == fence.recoveryEpoch else { return nil }
        return cursor
    }

    /// Whether a timer, feed or sleep mutation for this baby is still queued
    /// or awaiting its receipt. A row whose baby cannot be read counts.
    public func hasPendingMutation(babyID: UUID) -> Bool {
        let kinds: Set<String> = [WireEntityKind.timer.rawValue, WireEntityKind.feed.rawValue, WireEntityKind.sleep.rawValue]
        guard let rows = try? store.liveOutboxRows() else { return true }
        return rows.contains { row in
            guard !row.deliveryState.isTerminal, kinds.contains(row.entityKindRaw) else { return false }
            guard let baby = Self.babyID(in: row.payload) else { return true }
            return baby == babyID
        }
    }

    /// The revision this phone can vouch for: the applied cursor, and the
    /// lifecycle the store has. Nil without a cursor for this realm.
    ///
    /// Its generation is the server's for this card when known — the
    /// registered canonical one, or the card's own when the lifecycle is the
    /// one it shows — and otherwise names the store's own lifecycle, which no
    /// server generation equals. Version and timestamp are the card's: the
    /// phone has no ordering of its own to add.
    public func localRevision(
        scope: LiveActivityScope,
        remote: LiveActivityRevision?,
        lifecycle: LiveActivityReconciliation.Lifecycle,
        registeredGeneration: UUID?
    ) -> LiveActivityRevision? {
        let fence = LiveActivityRealmFence(serverInstanceID: scope.serverInstanceID, recoveryEpoch: scope.recoveryEpoch, familyID: scope.familyID)
        guard let cursor = appliedCursor(in: fence) else { return nil }
        let generation: UUID
        if let registeredGeneration {
            generation = registeredGeneration
        } else if let remote, lifecycle.contains(timerID: remote.timerID, sessionID: remote.sessionID) {
            generation = remote.generationID
        } else {
            generation = lifecycle.timerID ?? lifecycle.sessionID ?? Self.noLifecycle
        }
        return try? LiveActivityRevision(
            scope: scope, generationID: generation, timerID: lifecycle.timerID, sessionID: lifecycle.sessionID,
            familySequence: max(0, cursor.sequence), presentationVersion: remote?.presentationVersion ?? 1,
            presentationTimestamp: remote?.presentationTimestamp ?? 0,
            canWrite: (remote?.canWrite ?? true) && store.canWriteInActiveRealm
        )
    }

    private static func babyID(in payload: Data) -> UUID? {
        guard let mutation = try? JSONSerialization.jsonObject(with: payload) as? [String: Any],
              let fields = mutation["payload"] as? [String: Any],
              let raw = fields["baby_id"] as? String else { return nil }
        return UUID(uuidString: raw)
    }
}
