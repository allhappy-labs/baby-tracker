import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

@MainActor @Suite struct PastEntrySheetTests {
    /// Every kind that can be logged after the fact, Diaper and Solids
    /// included, in the order of the tiles.
    @Test func offersAllSixKinds() {
        #expect(PastEntrySheet.kinds == [.breast, .bottle, .sleep, .pump, .diaper, .solids])
    }

    /// The date pill reads as a day ("Today"), not as the plain date the
    /// system formatter would give, and yesterday reads differently again.
    @Test func datePillNamesTheDayRelatively() {
        let now = Date()
        let plain = DateFormatter()
        plain.dateStyle = .medium
        plain.timeStyle = .none
        #expect(EditTimeText.dayText(now) != plain.string(from: now))
        #expect(EditTimeText.dayText(now) != EditTimeText.dayText(now.addingTimeInterval(-86_400)))
        let lastWeek = now.addingTimeInterval(-7 * 86_400)
        #expect(EditTimeText.dayText(lastWeek) == plain.string(from: lastWeek))
    }

    /// Bottle is picked on the list now, not switched to, so it must open
    /// with no End of its own; breast and sleep open with one.
    @Test func bottleOpensAsAMoment() throws {
        let (store, baby, _) = try fixture()
        #expect(NewPartModel(store: store, babyID: baby, kind: .bottle).end == nil)
        #expect(NewPartModel(store: store, babyID: baby, kind: .breast).end != nil)
        #expect(NewPartModel(store: store, babyID: baby, kind: .sleep).end != nil)
    }
}
