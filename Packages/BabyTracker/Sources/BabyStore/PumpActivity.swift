import Foundation
import BabyCore
#if os(iOS)
import ActivityKit
#endif

/// What the pump card shows. Platform-neutral so it is testable on macOS.
public struct PumpActivityContentState: Codable, Hashable, Sendable {
    public let startedAt: Date
    public let sideRaw: String

    public init(startedAt: Date, side: PumpSide) {
        self.startedAt = startedAt
        self.sideRaw = side.rawValue
    }

    public var side: PumpSide { PumpSide(rawValue: sideRaw) ?? .both }
    /// The card goes stale at the six-hour cap: no session runs past it.
    public var staleDate: Date { startedAt.addingTimeInterval(PumpRules.maxSeconds) }
}

public struct PumpCard: Equatable, Sendable {
    public let id: String
    public let pumpID: UUID
    public let state: PumpActivityContentState
}

/// The pump cards on this phone: ActivityKit on iOS, memory in tests.
public protocol PumpCardBoard: Sendable {
    func cards() -> [PumpCard]
    func request(pumpID: UUID, state: PumpActivityContentState) throws -> String?
    func update(id: String, state: PumpActivityContentState) async
    func end(id: String) async
}

public final class MemoryPumpCardBoard: PumpCardBoard, @unchecked Sendable {
    private let lock = NSLock()
    private var stored: [PumpCard] = []
    private var endedIDs: [String] = []

    public init() {}

    public var ended: [String] { lock.withLock { endedIDs } }
    public func cards() -> [PumpCard] { lock.withLock { stored } }

    public func request(pumpID: UUID, state: PumpActivityContentState) throws -> String? {
        lock.withLock {
            let id = UUID().uuidString
            stored.append(PumpCard(id: id, pumpID: pumpID, state: state))
            return id
        }
    }

    public func update(id: String, state: PumpActivityContentState) async {
        lock.withLock {
            guard let index = stored.firstIndex(where: { $0.id == id }) else { return }
            stored[index] = PumpCard(id: id, pumpID: stored[index].pumpID, state: state)
        }
    }

    public func end(id: String) async {
        lock.withLock {
            stored.removeAll { $0.id == id }
            endedIDs.append(id)
        }
    }
}

struct NoPumpCardBoard: PumpCardBoard {
    func cards() -> [PumpCard] { [] }
    func request(pumpID: UUID, state: PumpActivityContentState) throws -> String? { nil }
    func update(id: String, state: PumpActivityContentState) async {}
    func end(id: String) async {}
}

public enum PumpCards {
    @TaskLocal public static var board: any PumpCardBoard = defaultBoard

    private static var defaultBoard: any PumpCardBoard {
        #if os(iOS)
        ActivityKitPumpCardBoard()
        #else
        NoPumpCardBoard()
        #endif
    }
}

/// The pumping card on the phone that started the session, separate from the
/// feed and sleep cards so a bottle card and a pump card sit side by side.
/// The other phone gets none: that needs server push, which Part 1 leaves
/// out.
@MainActor
public enum PumpActivityController {
    public enum Decision: Equatable, Sendable { case keep, update(PumpActivityContentState), end }

    /// Starts the card for a pump this phone just started. Never called for
    /// one a sync brought in.
    public static func start(for pump: PumpEntry) async {
        let board = PumpCards.board
        for card in board.cards() where card.pumpID != pump.id {
            await board.end(id: card.id)
        }
        guard !board.cards().contains(where: { $0.pumpID == pump.id }) else { return }
        _ = try? board.request(pumpID: pump.id, state: PumpActivityContentState(startedAt: pump.startedAt, side: pump.side))
    }

    /// A card stays only while its own session runs and is under six hours;
    /// an edit of that session (side, start) is carried over.
    public nonisolated static func decision(for card: PumpCard, runningID: UUID?,
                                            runningState: PumpActivityContentState?, now: Date) -> Decision {
        guard let runningID, let runningState, runningID == card.pumpID, now < runningState.staleDate else { return .end }
        return runningState == card.state ? .keep : .update(runningState)
    }

    /// Ends every card whose session is over, here or on the other phone, was
    /// superseded, or is past six hours. Runs at launch, on activation and on
    /// every store change, so a stop that arrives by sync takes the card
    /// down. Never starts one.
    public static func reconcile(in store: BabyStore) async {
        let board = PumpCards.board
        let running = try? store.runningPump()
        let state = running.map { PumpActivityContentState(startedAt: $0.startedAt, side: $0.side) }
        for card in board.cards() {
            switch decision(for: card, runningID: running?.id, runningState: state, now: store.now) {
            case .keep: break
            case .update(let next): await board.update(id: card.id, state: next)
            case .end: await board.end(id: card.id)
            }
        }
    }
}

#if os(iOS)
public struct PumpActivityAttributes: ActivityAttributes {
    public typealias ContentState = PumpActivityContentState
    public let pumpID: UUID

    public init(pumpID: UUID) { self.pumpID = pumpID }
}

struct ActivityKitPumpCardBoard: PumpCardBoard {
    func cards() -> [PumpCard] {
        Activity<PumpActivityAttributes>.activities
            .filter { $0.activityState == .active || $0.activityState == .stale }
            .map { PumpCard(id: $0.id, pumpID: $0.attributes.pumpID, state: $0.content.state) }
    }

    func request(pumpID: UUID, state: PumpActivityContentState) throws -> String? {
        guard ActivityAuthorizationInfo().areActivitiesEnabled else { return nil }
        return try Activity.request(attributes: PumpActivityAttributes(pumpID: pumpID),
                                    content: ActivityContent(state: state, staleDate: state.staleDate)).id
    }

    func update(id: String, state: PumpActivityContentState) async {
        guard let activity = Activity<PumpActivityAttributes>.activities.first(where: { $0.id == id }) else { return }
        await activity.update(ActivityContent(state: state, staleDate: state.staleDate))
    }

    func end(id: String) async {
        guard let activity = Activity<PumpActivityAttributes>.activities.first(where: { $0.id == id }) else { return }
        await activity.end(nil, dismissalPolicy: .immediate)
    }
}
#endif
