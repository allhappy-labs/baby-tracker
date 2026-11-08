import Foundation
import Testing
import BabyCore
@testable import BabyStore
@testable import BabyFeature

// Review Focus 2. Forgotten: Stop on the card seven hours later saves six.
@MainActor
@Test func stoppingFromTheCardAsksForTheAmountNextOpen() async throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let pending = PendingQuickLogStore(defaults: emptyQuickLogDefaults(), center: NotificationCenter())
    let board = MemoryPumpCardBoard()
    let pump = try store.startPump(side: .both)
    try await PumpCards.$board.withValue(board) {
        await PumpActivityController.start(for: pump)
        clock.advance(by: 7 * 3600)
        try await OutOfAppPumpActions.stop(in: store, pending: pending)
    }
    #expect(try store.runningPump() == nil)
    #expect(pump.endedAt == pump.startedAt.addingTimeInterval(PumpRules.maxSeconds))
    #expect(board.cards().isEmpty)
    #expect(pending.consume() == PendingQuickLogRequest(.pumpAmount(pumpID: pump.id)))
}

@MainActor
@Test func stoppingACardWhoseSessionAlreadyEndedAsksNothing() async throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let pending = PendingQuickLogStore(defaults: emptyQuickLogDefaults(), center: NotificationCenter())
    try await PumpCards.$board.withValue(MemoryPumpCardBoard()) {
        try await OutOfAppPumpActions.stop(in: store, pending: pending)
    }
    #expect(pending.consume() == nil)
}

/// A card left over from an earlier session: its Stop must not end the
/// session the other parent has since started. Only the stale card goes.
@MainActor
@Test func aStaleCardsStopLeavesAnotherSessionRunning() async throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let pending = PendingQuickLogStore(defaults: emptyQuickLogDefaults(), center: NotificationCenter())
    let board = MemoryPumpCardBoard()
    let old = try store.startPump(side: .left)
    try await PumpCards.$board.withValue(board) {
        await PumpActivityController.start(for: old)
        // Stopped elsewhere, and a new session started, without this phone's
        // card being reconciled yet.
        try store.stopPump(old)
        let current = try store.startPump(side: .both)
        try await OutOfAppPumpActions.stop(pumpID: old.id, in: store, pending: pending)
        #expect(try store.runningPump()?.id == current.id)
    }
    #expect(board.cards().isEmpty)
    #expect(pending.consume() == nil)
}
