import Foundation
import BabyCore
@testable import BabyStore

/// Shared by the one-part-timer tests (Task 9) and the Home snapshot tests
/// (Task 10): a baby on a deterministic clock, with the standard break limits
/// pinned so a change to the real default can't silently shift either suite.
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
