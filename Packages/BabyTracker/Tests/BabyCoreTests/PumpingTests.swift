import Testing
import Foundation
@testable import BabyCore

@Suite struct PumpingTests {
    @Test func sidesHaveNamesAndRawValuesMatchTheWire() {
        #expect(PumpSide.allCases.map(\.rawValue) == ["left", "right", "both"])
        #expect(PumpSide.both.displayName == "Both")
        #expect(PumpCopy.caption(.both) == "Pumping · Both")
    }

    /// The server takes at most two decimals; ounces never land on whole ml.
    @Test func volumesRoundToTwoDecimals() {
        #expect(PumpRules.roundedVolume(Volume(ounces: 3).millilitres) == 88.72)
        #expect(PumpRules.roundedVolume(90.5) == 90.5)
        #expect(PumpRules.roundedVolume(0.004) == 0)
    }

    @Test func volumeBoundsMatchTheServer() {
        #expect(PumpRules.isValidVolume(nil))
        #expect(PumpRules.isValidVolume(0))
        #expect(PumpRules.isValidVolume(1000))
        #expect(!PumpRules.isValidVolume(-0.01))
        #expect(!PumpRules.isValidVolume(1000.01))
        #expect(!PumpRules.isValidVolume(.nan))
    }
}
