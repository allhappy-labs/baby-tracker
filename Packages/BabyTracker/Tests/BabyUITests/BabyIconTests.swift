import Testing
import SwiftUI
@testable import BabyUI

@Suite struct BabyIconTests {
    @Test func everyIconHasAnAssetInTheBundle() {
        for icon in BabyIcon.allCases {
            #expect(Bundle.module.url(forResource: "Assets", withExtension: "car") != nil
                    || Bundle.module.path(forResource: icon.assetName, ofType: "svg") != nil,
                    "missing asset for \(icon)")
        }
    }

    @Test func trackersMapToTheNewIcons() {
        #expect(Tracker.diaper.icon == .diaper)
        #expect(Tracker.supplement.icon == .meds)
        #expect(Tracker.supplement.title == "Meds")
    }

    @Test func breastSidesShareTheFeedDrop() {
        #expect(BabyIcon.breastLeft.assetName == "icon-feed")
        #expect(BabyIcon.breastRight.assetName == "icon-feed")
    }

    @Test func everyOtherIconNamesItsOwnAsset() {
        for icon in BabyIcon.allCases where icon != .breastLeft && icon != .breastRight {
            #expect(icon.assetName == "icon-" + icon.rawValue)
        }
    }
}
