import BabyCore
import BabyProtocol
import BabyStore
import BabySync
import Foundation
import Testing
@testable import SettingsFeature

@MainActor
@Suite("Import summary and conflict presentation")
struct ImportConflictViewModelTests {
    private func frozenManifest() throws -> (FrozenImportManifest, Baby) {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Mira", birthDate: Date(timeIntervalSince1970: 1_700_000_000), referenceSex: .girls, expectedDueDate: nil)
        _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby.id)
        _ = try store.logDiaper(.wet, for: baby.id)
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .matchShared)
        return (try store.freezeImportManifest(checkpointID: checkpoint), baby)
    }

    /// The summary must describe the frozen bytes, not live rows: the user is
    /// approving exactly what will be sent.
    @Test func summaryComesFromTheFrozenManifest() throws {
        let (manifest, baby) = try frozenManifest()
        let summary = try ImportSummaryView.Summary.fromFrozen(manifest)

        #expect(summary.babyNames == ["Mira"])
        #expect(summary.recordCount == 3)
        #expect(!summary.isEmpty)
        _ = baby
    }

    /// The saved displayed date, as the user reads it — not a re-derived one
    /// that could land a day earlier in another time zone.
    @Test func summaryShowsTheDisplayedBirthDate() throws {
        let (manifest, _) = try frozenManifest()
        let summary = try ImportSummaryView.Summary.fromFrozen(manifest)
        let expected = try LocalDate(displaying: Date(timeIntervalSince1970: 1_700_000_000), in: .current)

        #expect(summary.displayedBirthDates == [expected.iso8601])
    }

    @Test func emptyDeviceReportsNothingToShare() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .matchShared)
        let manifest = try store.freezeImportManifest(checkpointID: checkpoint)

        #expect(try ImportSummaryView.Summary.fromFrozen(manifest).isEmpty)
    }

    /// Both encodable resolutions, and only those two.
    @Test func conflictOffersExactlyTheTwoValidAnswers() {
        let candidate = UUID()
        let useExisting = ImportConflictResolution(choice: .useExisting, canonicalBabyID: candidate)
        let keepLocal = ImportConflictResolution(choice: .keepLocal, canonicalBabyID: nil)

        #expect(useExisting.isValid)
        #expect(keepLocal.isValid)
        #expect(!ImportConflictResolution(choice: .useExisting, canonicalBabyID: nil).isValid)
        #expect(!ImportConflictResolution(choice: .keepLocal, canonicalBabyID: candidate).isValid)
    }

    /// The copy has to say the decision cannot be undone, because it cannot:
    /// choosing "same baby" merges two children's records in one irreversible
    /// transaction.
    @Test func conflictCopyWarnsThatTheChoiceIsFinal() {
        #expect(ImportConflictCopy.explanation.contains("cannot be undone"))
        #expect(ImportConflictCopy.useExisting.contains("Same baby"))
        #expect(ImportConflictCopy.keepLocal.contains("different baby"))
    }

    /// The summary says records move and nothing is deleted. A user who thinks
    /// joining wipes their device will not join.
    @Test func summaryCopyExplainsSharingWithoutDeletion() {
        #expect(ImportSummaryCopy.footer.contains("Nothing is deleted"))
        #expect(ImportSummaryCopy.footer.contains("every caregiver"))
        #expect(ImportSummaryCopy.emptyFooter.contains("nothing on this device"))
    }
}
