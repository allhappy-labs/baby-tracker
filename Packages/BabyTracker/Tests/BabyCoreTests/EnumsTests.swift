import Testing
import Foundation
@testable import BabyCore

@Test func rawValuesAreStableContract() {
    // These strings are written to the store and will be written to the wire
    // by the future sync node. Changing one is a migration, not a rename.
    #expect(FeedKind.breast.rawValue == "breast")
    #expect(FeedKind.bottle.rawValue == "bottle")
    #expect(FeedKind.solids.rawValue == "solids")
    #expect(BreastSide.left.rawValue == "left")
    #expect(DiaperContents.mixed.rawValue == "mixed")
    #expect(TimerKind.feed.rawValue == "feed")
}

@Test func unknownRawValueDecodesToNil() {
    #expect(FeedKind(rawValue: "telepathy") == nil)
}

// MARK: - Display names
//
// Raw values are lower-case storage keys. Rendering them straight put "left"
// and "mixed" beside properly capitalised siblings like "Dirty" in the same
// dot-separated line, on Home and in History.

@Test func displayNamesAreCapitalisedForTheScreen() {
    #expect(BreastSide.left.displayName == "Left")
    #expect(BreastSide.right.displayName == "Right")
    #expect(DiaperContents.dirty.displayName == "Dirty")
    #expect(DiaperContents.mixed.displayName == "Mixed")
    #expect(BottleContents.formula.displayName == "Formula")
    #expect(BottleContents.mixed.displayName == "Mixed")
    // `.capitalized` on the raw value would render this "Breastmilk".
    #expect(BottleContents.breastMilk.displayName == "Breast milk")
}
