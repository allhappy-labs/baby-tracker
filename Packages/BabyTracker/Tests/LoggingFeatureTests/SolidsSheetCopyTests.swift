import Testing
import BabyCore
@testable import LoggingFeature

/// `foods(from:)` is the only thing standing between free text and what gets
/// stored, and it shipped with no test at all.

@Test func foodsSplitsOnCommasAndTrimsEachItem() {
    #expect(SolidsSheetCopy.foods(from: "pear, oat") == ["pear", "oat"])
    #expect(SolidsSheetCopy.foods(from: "pear,oat") == ["pear", "oat"])
    #expect(SolidsSheetCopy.foods(from: "  pear ,  oat  ") == ["pear", "oat"])
    // Inner spaces are part of the food, not padding.
    #expect(SolidsSheetCopy.foods(from: "sweet potato, apple") == ["sweet potato", "apple"])
}

@Test func foodsDropsTheBlanksATrailingOrDoubledCommaWouldCreate() {
    #expect(SolidsSheetCopy.foods(from: "pear,") == ["pear"])
    #expect(SolidsSheetCopy.foods(from: "pear,,oat") == ["pear", "oat"])
    #expect(SolidsSheetCopy.foods(from: ",pear") == ["pear"])
    // Whitespace-only items are blanks too, after the trim.
    #expect(SolidsSheetCopy.foods(from: "pear,   ,oat") == ["pear", "oat"])
    #expect(SolidsSheetCopy.foods(from: "pear\n") == ["pear"])
}

@Test func foodsYieldsNothingForTextThatNamesNoFood() {
    #expect(SolidsSheetCopy.foods(from: "") == [])
    #expect(SolidsSheetCopy.foods(from: "   ") == [])
    #expect(SolidsSheetCopy.foods(from: ",,,") == [])
}

/// A solids entry IS its foods and amount, so a `foods == []` entry would
/// say nothing wherever it is shown. Save is gated rather than left to log
/// one.
@Test func saveIsOffUntilTheTextNamesAtLeastOneFood() {
    #expect(!SolidsSheetCopy.canSave(foodsText: ""))
    #expect(!SolidsSheetCopy.canSave(foodsText: "   "))
    #expect(!SolidsSheetCopy.canSave(foodsText: " , , "))
    #expect(SolidsSheetCopy.canSave(foodsText: "pear"))
    #expect(SolidsSheetCopy.canSave(foodsText: " pear ,"))
}

/// The disabled button needs a stated reason; a footer that still explains
/// commas while Save is dead explains the wrong thing.
@Test func theFooterSaysWhySaveIsOff() {
    #expect(SolidsSheetCopy.foodsFooter == "Separate foods with commas")
    #expect(SolidsSheetCopy.foodsRequiredFooter == "Name at least one food to save")
    #expect(SolidsSheetCopy.foodsRequiredFooter != SolidsSheetCopy.foodsFooter)
}
