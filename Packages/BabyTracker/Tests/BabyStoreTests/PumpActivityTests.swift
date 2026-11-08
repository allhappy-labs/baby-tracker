import Foundation
import Testing
import BabyCore
import BabyProtocol
@testable import BabyStore

@MainActor
private func onPumpBoard(_ board: MemoryPumpCardBoard, _ body: () async -> Void) async {
    await PumpCards.$board.withValue(board) { await body() }
}

@MainActor @Suite("Pump card")
struct PumpActivityTests {
    @Test func startingRequestsOneCardForThePump() async throws {
        let (store, _, _) = try fixture()
        let board = MemoryPumpCardBoard()
        let pump = try store.startPump(side: .left)
        await onPumpBoard(board) {
            await PumpActivityController.start(for: pump)
            await PumpActivityController.start(for: pump)
        }
        #expect(board.cards().map(\.pumpID) == [pump.id])
        #expect(board.cards().first?.state.side == .left)
        #expect(board.cards().first?.state.staleDate == pump.startedAt.addingTimeInterval(PumpRules.maxSeconds))
    }

    // Review Focus 5. The other phone stopped it; this phone learns of it at
    // the next store change, and the card goes.
    @Test func aCardWhosePumpEndedElsewhereEnds() async throws {
        let (store, _, clock) = try fixture()
        let board = MemoryPumpCardBoard()
        let pump = try store.startPump(side: .both)
        await onPumpBoard(board) { await PumpActivityController.start(for: pump) }
        clock.advance(by: 600)
        try store.stopPump(pump)
        await onPumpBoard(board) { await PumpActivityController.reconcile(in: store) }
        #expect(board.cards().isEmpty)
        #expect(board.ended.count == 1)
    }

    // Review Focus 5. The other phone deleted the session.
    @Test func aCardWhosePumpWasDeletedEnds() async throws {
        let (store, _, _) = try fixture()
        let board = MemoryPumpCardBoard()
        let pump = try store.startPump(side: .both)
        await onPumpBoard(board) { await PumpActivityController.start(for: pump) }
        try store.softDelete(pump)
        await onPumpBoard(board) { await PumpActivityController.reconcile(in: store) }
        #expect(board.cards().isEmpty)
    }

    // Review Focus 5. This phone's start lost the race: the session it shows
    // is not the family's, and the other phone's gets no card here.
    @Test func aCardWhoseStartLostTheRaceEnds() async throws {
        let store = try BabyStore.sharedFixture()
        let board = MemoryPumpCardBoard()
        let mine = try store.startPump(side: .both)
        await onPumpBoard(board) { await PumpActivityController.start(for: mine) }
        let start = try #require(try store.liveOutboxRows().first { $0.entityID == mine.id })
        try store.applySyncPage(SyncFixture.page(outcomes: [PumpSyncTests.supersededByRunningPump(start.id, winner: UUID())]))
        #expect(try store.runningPump() != nil)
        await onPumpBoard(board) { await PumpActivityController.reconcile(in: store) }
        #expect(board.cards().isEmpty)
    }

    // Review Focus 5.
    @Test func aCardPastTheSixHourCapEnds() async throws {
        let (store, _, clock) = try fixture()
        let board = MemoryPumpCardBoard()
        let pump = try store.startPump(side: .both)
        await onPumpBoard(board) { await PumpActivityController.start(for: pump) }
        clock.advance(by: PumpRules.maxSeconds - 1)
        await onPumpBoard(board) { await PumpActivityController.reconcile(in: store) }
        #expect(board.cards().count == 1)
        clock.advance(by: 2)
        await onPumpBoard(board) { await PumpActivityController.reconcile(in: store) }
        #expect(board.cards().isEmpty)
    }

    @Test func aSideOrStartEditedElsewhereUpdatesTheCard() async throws {
        let (store, _, _) = try fixture()
        let board = MemoryPumpCardBoard()
        let pump = try store.startPump(side: .both)
        await onPumpBoard(board) { await PumpActivityController.start(for: pump) }
        try store.updatePump(pump, side: .right, startedAt: pump.startedAt.addingTimeInterval(-120), endedAt: nil, volumeML: nil, note: nil)
        await onPumpBoard(board) { await PumpActivityController.reconcile(in: store) }
        #expect(board.cards().first?.state.side == .right)
        #expect(board.cards().first?.state.startedAt == pump.startedAt)
        #expect(board.ended.isEmpty)
    }

    /// Part 1: the phone that did not start the session gets no card.
    @Test func reconcileNeverStartsACard() async throws {
        let (store, _, _) = try fixture()
        let board = MemoryPumpCardBoard()
        _ = try store.startPump(side: .both)
        await onPumpBoard(board) { await PumpActivityController.reconcile(in: store) }
        #expect(board.cards().isEmpty)
    }
}
