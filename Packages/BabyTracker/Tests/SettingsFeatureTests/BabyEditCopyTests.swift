import Testing
@testable import SettingsFeature

@Test func theDeleteConfirmationNamesTheBaby() {
    #expect(BabyEditCopy.deleteConfirmTitle(name: "Mira") == "Delete Mira?")
}

@Test func theDeleteFooterSaysTheEntriesSurvive() {
    #expect(
        BabyEditCopy.deleteFooter
            == "Removes them from the app. Nothing already logged is deleted."
    )
}
