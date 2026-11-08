import Testing
import BabyCore
@testable import BabyUI

@Suite struct HomeActionDisplayTests {
    @Test func everyActionHasItsOwnName() {
        let titles = HomeActionKind.allCases.map(\.title)
        #expect(titles.allSatisfy { !$0.isEmpty })
        #expect(Set(titles).count == HomeActionKind.allCases.count)
    }

    @Test func actionsUseTheirTrackersLook() {
        #expect(HomeActionKind.breast.icon == .feed)
        #expect(HomeActionKind.bottle.icon == .bottle)
        #expect(HomeActionKind.meds.icon == Tracker.supplement.icon)
        #expect(HomeActionKind.meds.title == Tracker.supplement.title)
        #expect(HomeActionKind.growth.title == Tracker.growth.title)
    }
}
