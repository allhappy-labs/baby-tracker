import Testing
import BabyCore

@Test func sentenceCaseRaisesOnlyTheFirstCharacter() {
    #expect("pear, oat".sentenceCased == "Pear, oat")
    #expect("sweet potato, apple".sentenceCased == "Sweet potato, apple")
    // `.capitalized` would have title-cased every word; this must not.
    #expect("sweet potato".sentenceCased != "sweet potato".capitalized)
}

@Test func sentenceCasePreservesWhatIsAlreadyCapitalisedAndSurvivesEmpty() {
    #expect("Pear".sentenceCased == "Pear")
    #expect("".sentenceCased == "")
    // Multi-scalar graphemes must not be split.
    #expect("éclair".sentenceCased == "Éclair")
}
