import Testing
import BabyCore
@testable import LoggingFeature

@Test func theDetailDisclosureIsCalledDetails() {
    // The sheet calls its accordion "Details", matching the approved mockup
    // (sheets-v2.html), in edit mode as when logging.
    #expect(DiaperSheetCopy.detailHeader == "Details")
    #expect(DiaperSheetCopy.consistencyLabel == "Consistency")
    #expect(DiaperSheetCopy.colorLabel == "Colour")
    #expect(DiaperSheetCopy.noteLabel == "Note")
}

@Test func thePromptsGiveExamplesRatherThanRepeatTheLabel() {
    #expect(DiaperSheetCopy.consistencyPrompt == "Seedy, runny, firm")
    #expect(DiaperSheetCopy.colorPrompt == "Mustard, green, dark")
    #expect(DiaperSheetCopy.notePrompt == "Optional")
}

@Test func theFastPathListsWetDirtyMixedAndTheMixedLabelReadsMixed() {
    #expect(DiaperContents.fastPath == [.wet, .dirty, .mixed])
    #expect(DiaperContents.mixed.displayName == "Mixed")
}
