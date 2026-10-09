import Foundation
import Testing
import BabyCore
import BabyStore
@testable import SettingsFeature

struct GrowthEditTests {
    private enum ReadFailure: Error { case unavailable }

    @MainActor @Test func failedProfileReadReportsErrorAndUnrelatedSavePreservesProfile() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(Date()))
        let day = try LocalDate("2026-10-01")
        let baby = try store.addBaby(name: "Mira", birthDate: Date(), referenceSex: .girls, expectedDueDate: day)
        var editor = BabyGrowthProfileEditor(fallbackDate: baby.birthDate)
        var reported = 0
        editor.load(fetch: { throw ReadFailure.unavailable }) { _ in reported += 1 }
        #expect(reported == 1)
        #expect(editor.failed)
        #expect(!editor.loaded)
        // Defensive boundary also blocks programmatic changes while the read failed.
        editor.referenceSex = .boys
        editor.hasDueDate = false
        #expect(editor.referenceForSave == nil)
        try BabyGrowthEditValidation.save(store: store, baby: baby, name: "Edited", birthDate: baby.birthDate,
                                         referenceSex: editor.referenceForSave, expectedDueDate: editor.dueDateForSave()) {}
        #expect(baby.name == "Edited")
        #expect(try store.growthProfile(for: baby.id)?.referenceSex == .girls)
        #expect(try store.growthProfile(for: baby.id)?.expectedDueDate == day)
        editor.load(fetch: { try store.growthProfile(for: baby.id) }) { _ in reported += 1 }
        #expect(editor.loaded)
        #expect(!editor.failed)
        #expect(editor.referenceSex == .girls)
        #expect(try editor.dueDateForSave() == day)
        #expect(reported == 1)
    }

    @MainActor @Test func successfulMissingProfileLoadIsExplicitlyLegacy() {
        var editor = BabyGrowthProfileEditor(fallbackDate: Date())
        editor.load(fetch: { nil }) { _ in Issue.record("Unexpected load error") }
        #expect(editor.loaded)
        #expect(!editor.failed)
        #expect(editor.referenceSex == nil)
        editor.referenceSex = .boys
        #expect(editor.referenceForSave == .boys)
    }

    @MainActor @Test func legacyBabyCanSaveUnrelatedEditsAndThenChooseReference() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(Date()))
        let baby = Baby(name: "Legacy", birthDate: Date())
        store.context.insert(baby)
        try store.context.save()
        var completed = 0
        try BabyGrowthEditValidation.save(store: store, baby: baby, name: "Renamed", birthDate: baby.birthDate,
                                         referenceSex: nil, expectedDueDate: nil) { completed += 1 }
        #expect(baby.name == "Renamed")
        #expect(try store.growthProfile(for: baby.id) == nil)
        try BabyGrowthEditValidation.save(store: store, baby: baby, name: baby.name, birthDate: baby.birthDate,
                                         referenceSex: .boys, expectedDueDate: nil) { completed += 1 }
        #expect(try store.growthProfile(for: baby.id)?.referenceSex == .boys)
        #expect(completed == 2)
    }

    @MainActor @Test func clearingDueDatePersistsNil() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(Date()))
        let baby = try store.addBaby(name: "Mira", birthDate: Date(), referenceSex: .girls,
                                     expectedDueDate: LocalDate("2026-10-01"))
        try BabyGrowthEditValidation.save(store: store, baby: baby, name: baby.name, birthDate: baby.birthDate,
                                         referenceSex: .girls, expectedDueDate: nil) {}
        #expect(try store.growthProfile(for: baby.id)?.expectedDueDate == nil)
    }

    @MainActor @Test func failedEditDoesNotDismissOrApplyProfile() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(Date()))
        let baby = try store.addBaby(name: "Mira", birthDate: Date(), referenceSex: .girls, expectedDueDate: nil)
        var completed = false
        #expect(throws: (any Error).self) {
            try BabyGrowthEditValidation.save(store: store, baby: baby, name: " ", birthDate: baby.birthDate,
                                             referenceSex: .boys, expectedDueDate: nil) { completed = true }
        }
        #expect(!completed)
        #expect(baby.name == "Mira")
        #expect(try store.growthProfile(for: baby.id)?.referenceSex == .girls)
    }

    @Test func absentLegacyDueDateStaysAbsent() {
        #expect(BabyGrowthEditValidation.pickerDate(nil) == nil)
    }

    @Test func dateOnlyDueDateRoundTripsInDisplayedTimeZone() throws {
        let day = try LocalDate("2026-03-29")
        for offset in [-43200, 0, 50400] {
            let zone = TimeZone(secondsFromGMT: offset)!
            let date = try #require(BabyGrowthEditValidation.pickerDate(day, timeZone: zone))
            #expect(try LocalDate(displaying: date, in: zone) == day)
        }
    }
}
