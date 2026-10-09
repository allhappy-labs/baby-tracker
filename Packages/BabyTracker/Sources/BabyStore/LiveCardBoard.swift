import Foundation
import os
#if os(iOS)
import ActivityKit
#endif

/// One Live Activity card, as `LiveActivityController` sees it.
public struct LiveCard: Sendable, Equatable {
    public let id: String
    public let babyID: UUID
    public let kindRaw: String
    public let state: TimerActivityContentState
    public let staleDate: Date?
    /// The realm and target the card was started for. Nil for a card from a
    /// build before push, or from a realm without it.
    public let scope: LiveActivityScope?

    public init(id: String, babyID: UUID, kindRaw: String, state: TimerActivityContentState, staleDate: Date? = nil,
                scope: LiveActivityScope? = nil) {
        self.id = id
        self.babyID = babyID
        self.kindRaw = kindRaw
        self.state = state
        self.staleDate = staleDate
        self.scope = scope
    }
}

/// Everything a new card is requested with.
public struct LiveCardRequest: Sendable, Equatable {
    public let babyID: UUID
    public let babyName: String
    public let kindRaw: String
    public let state: TimerActivityContentState
    /// The realm the card belongs to: only in a push-managed realm.
    public let scope: LiveActivityScope?
    /// Ask ActivityKit for an update token (`pushType: .token`), so the
    /// server can update and end the card. Only in a push-managed realm;
    /// everywhere else the request is exactly what it always was.
    public let isPushEnabled: Bool

    public init(babyID: UUID, babyName: String, kindRaw: String, state: TimerActivityContentState,
                scope: LiveActivityScope? = nil, isPushEnabled: Bool = false) {
        self.babyID = babyID
        self.babyName = babyName
        self.kindRaw = kindRaw
        self.state = state
        self.scope = scope
        self.isPushEnabled = isPushEnabled
    }
}

/// Where Live Activity cards live: ActivityKit on an iPhone, memory in the
/// package suite.
///
/// The seam exists because the bugs in this area are about *sequences* — a
/// stop and the next start arriving in either order, two cards for one timer
/// — and ActivityKit cannot run on macOS, where the suite runs. With the
/// board swapped for `MemoryCardBoard`, every `LiveActivityController` entry
/// point runs for real in a test.
public protocol LiveCardBoard: Sendable {
    /// The cards that can still be updated — not those already ended.
    func cards() -> [LiveCard]
    /// Requests a card and returns its id; nil when the user has Live
    /// Activities switched off. Throws when the system refuses.
    func request(_ request: LiveCardRequest) throws -> String?
    /// `timestamp` orders the update against server pushes: ActivityKit keeps
    /// whichever content carries the later one. Nil uses the overload without
    /// a timestamp — a card no server pushes to.
    func update(id: String, state: TimerActivityContentState, staleDate: Date?, timestamp: Date?) async
    func end(id: String, timestamp: Date?) async
}

public extension LiveCardBoard {
    func update(id: String, state: TimerActivityContentState, staleDate: Date?) async {
        await update(id: id, state: state, staleDate: staleDate, timestamp: nil)
    }

    func end(id: String) async {
        await end(id: id, timestamp: nil)
    }
}

/// The board `LiveActivityController` uses, and the push state it reads. Both
/// task-locals, so a test can swap in its own without touching any other test
/// running in parallel.
public enum LiveCards {
    @TaskLocal public static var board: any LiveCardBoard = defaultBoard
    /// Published by the push registration coordinator: which realm's cards
    /// the server updates. Production has one, shared by the process.
    @TaskLocal public static var pushState: LiveActivityPushState = .shared

    private static var defaultBoard: any LiveCardBoard {
        #if os(iOS)
        ActivityKitCardBoard()
        #else
        NoCardBoard()
        #endif
    }
}

/// Off iOS there are no cards: every call is a no-op.
struct NoCardBoard: LiveCardBoard {
    func cards() -> [LiveCard] { [] }
    func request(_ request: LiveCardRequest) throws -> String? { nil }
    func update(id: String, state: TimerActivityContentState, staleDate: Date?, timestamp: Date?) async {}
    func end(id: String, timestamp: Date?) async {}
}

/// Cards in memory, for tests: what `LiveActivityController` did is read
/// back off `cards()`.
///
/// Ordering follows ActivityKit's rule as this build relies on it: each card
/// remembers the timestamp of the content it shows, and an update or end
/// stamped earlier than that is dropped. A call without a timestamp is
/// stamped with the board's own clock — the phone's, which need not agree with
/// the server's. A request sets no stamp: `Activity.request` takes none, and
/// this build assumes a requested card's content does not outrank an earlier
/// stamped push — signed-device acceptance must confirm it. `receive` and
/// `receiveStart` are the server's pushes.
public final class MemoryCardBoard: LiveCardBoard, @unchecked Sendable {
    private struct State {
        var cards: [LiveCard] = []
        var stamps: [String: Date] = [:]
        var requests: [LiveCardRequest] = []
        var ended: [String] = []
    }

    private let lock = OSAllocatedUnfairLock(initialState: State())
    private let now: @Sendable () -> Date

    public init(cards: [LiveCard] = [], now: @escaping @Sendable () -> Date = { Date() }) {
        self.now = now
        lock.withLock { $0.cards = cards }
    }

    public func cards() -> [LiveCard] {
        lock.withLock { $0.cards }
    }

    /// Every request made, in order.
    public var requests: [LiveCardRequest] { lock.withLock { $0.requests } }
    /// The ids of every card ended, in order.
    public var ended: [String] { lock.withLock { $0.ended } }

    /// The timestamp of the content a card shows; nil for a card put on the
    /// board directly.
    public func timestamp(of id: String) -> Date? { lock.withLock { $0.stamps[id] } }

    /// Puts a card on the board as an earlier launch or build left it.
    public func add(_ card: LiveCard) {
        lock.withLock { $0.cards.append(card) }
    }

    public func request(_ request: LiveCardRequest) throws -> String? {
        let id = UUID().uuidString
        lock.withLock {
            $0.requests.append(request)
            $0.cards.append(LiveCard(id: id, babyID: request.babyID, kindRaw: request.kindRaw, state: request.state, scope: request.scope))
        }
        return id
    }

    public func update(id: String, state: TimerActivityContentState, staleDate: Date?, timestamp: Date?) async {
        apply(id: id, state: state, staleDate: staleDate, at: timestamp ?? now())
    }

    public func end(id: String, timestamp: Date?) async {
        let stamp = timestamp ?? now()
        lock.withLock { board in
            guard board.cards.contains(where: { $0.id == id }) else { return }
            if let last = board.stamps[id], stamp < last { return }
            board.cards.removeAll { $0.id == id }
            board.stamps[id] = nil
            board.ended.append(id)
        }
    }

    /// An APNs update for a card on the board.
    public func receive(id: String, state: TimerActivityContentState, staleDate: Date?, timestamp: Date) {
        apply(id: id, state: state, staleDate: staleDate, at: timestamp)
    }

    /// An APNs push-to-start: a card this process did not request.
    public func receiveStart(id: String, babyID: UUID, kindRaw: String, scope: LiveActivityScope?, state: TimerActivityContentState,
                             staleDate: Date? = nil, timestamp: Date) {
        lock.withLock {
            $0.cards.append(LiveCard(id: id, babyID: babyID, kindRaw: kindRaw, state: state, staleDate: staleDate, scope: scope))
            $0.stamps[id] = timestamp
        }
    }

    private func apply(id: String, state: TimerActivityContentState, staleDate: Date?, at stamp: Date) {
        lock.withLock { board in
            guard let index = board.cards.firstIndex(where: { $0.id == id }) else { return }
            if let last = board.stamps[id], stamp < last { return }
            let card = board.cards[index]
            board.cards[index] = LiveCard(id: id, babyID: card.babyID, kindRaw: card.kindRaw, state: state, staleDate: staleDate,
                                          scope: card.scope)
            board.stamps[id] = stamp
        }
    }
}

#if os(iOS)
/// The real board. `Activity` is not `Sendable`, so it is looked up and used
/// entirely inside these nonisolated methods; only ids and states cross.
struct ActivityKitCardBoard: LiveCardBoard {
    func cards() -> [LiveCard] {
        Activity<TimerActivityAttributes>.activities
            .filter { $0.activityState == .active || $0.activityState == .stale }
            .map {
                LiveCard(
                    id: $0.id,
                    babyID: $0.attributes.babyID,
                    kindRaw: $0.attributes.kindRaw,
                    state: $0.content.state,
                    staleDate: $0.content.staleDate,
                    scope: $0.attributes.scope
                )
            }
    }

    func request(_ request: LiveCardRequest) throws -> String? {
        guard ActivityAuthorizationInfo().areActivitiesEnabled else { return nil }
        let attributes = TimerActivityAttributes(babyID: request.babyID, babyName: request.babyName, kindRaw: request.kindRaw,
                                                 scope: request.scope)
        let content = ActivityContent(state: request.state, staleDate: nil)
        // Without a push type no update token is ever issued, so the server
        // could neither update nor end the card.
        if request.isPushEnabled {
            return try Activity.request(attributes: attributes, content: content, pushType: .token).id
        }
        return try Activity.request(attributes: attributes, content: content).id
    }

    func update(id: String, state: TimerActivityContentState, staleDate: Date?, timestamp: Date?) async {
        guard let activity = Activity<TimerActivityAttributes>.activities.first(where: { $0.id == id }) else { return }
        let content = ActivityContent(state: state, staleDate: staleDate)
        if let timestamp {
            await activity.update(content, timestamp: timestamp)
        } else {
            await activity.update(content)
        }
    }

    func end(id: String, timestamp: Date?) async {
        guard let activity = Activity<TimerActivityAttributes>.activities.first(where: { $0.id == id }) else { return }
        if let timestamp {
            await activity.end(nil, dismissalPolicy: .immediate, timestamp: timestamp)
        } else {
            await activity.end(nil, dismissalPolicy: .immediate)
        }
    }
}
#endif
