import Foundation
import BabyCore
@testable import BabyStore

/// A copy of `BabyStoreTests/PartTestHelpers.swift`: test targets cannot
/// import each other's support files. A baby on a deterministic clock, with
/// the standard break limits pinned so a change to the real default can't
/// silently shift these sheet-model tests.
@MainActor
func fixture() throws -> (BabyStore, UUID, TestDateProvider) {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    store.breakLimits = { .standard }
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    return (store, baby.id, clock)
}

@MainActor
extension ActiveTimer {
    /// Advances the shared clock, then returns `self` so a timer's creation
    /// and the elapsed time before it stops can be written on one line.
    func advancing(_ clock: TestDateProvider, by seconds: TimeInterval) -> ActiveTimer {
        clock.advance(by: seconds)
        return self
    }
}

/// An id that names two sessions' worth of things (final review I2): Z, then
/// A, then B joined into A while parts that continued B kept B's id as their
/// session key — the other phone's running part, stopped before the Join
/// reached it. `b.id` is then both the live member B of session {A, B} and
/// the key of session {x, bottle}. Z is an earlier session, so a Join row
/// shows which of the two it took `b.id` for.
@MainActor
func collidingOrphan(_ store: BabyStore, _ baby: UUID, _ clock: TestDateProvider) throws
    -> (z: FeedEntry, a: FeedEntry, b: FeedEntry, x: FeedEntry, bottle: FeedEntry) {
    let z = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-8 * 3_600),
                                to: clock.now.addingTimeInterval(-8 * 3_600 + 600), for: baby)
    let a = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-4 * 3_600),
                                to: clock.now.addingTimeInterval(-4 * 3_600 + 600), for: baby)
    let b = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-900),
                                to: clock.now.addingTimeInterval(-300), for: baby)
    try store.combineFeeds([a.id, b.id], for: baby)
    let x = try store.logBreast(side: .right, from: clock.now.addingTimeInterval(-240),
                                to: clock.now.addingTimeInterval(-120), for: baby)
    x.feedingSessionID = b.id
    try store.context.save()
    let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: clock.now.addingTimeInterval(-60))
    precondition(bottle.feedingSessionID == b.id)
    return (z, a, b, x, bottle)
}
