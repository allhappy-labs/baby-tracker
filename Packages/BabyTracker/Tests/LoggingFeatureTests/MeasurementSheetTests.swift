import Testing
import Foundation
import BabyCore
import BabyStore
@testable import LoggingFeature

@Suite @MainActor struct MeasurementSheetTests {
    func setup() throws -> (BabyStore, UUID, TestDateProvider) {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: clock.now.addingTimeInterval(-864000), referenceSex: .girls, expectedDueDate: nil)
        return (store, baby.id, clock)
    }
    @Test func validatesValuesAndMeasurementMethod() throws {
        let (store, id, _) = try setup()
        let model = MeasurementSheetModel(store: store, mode: .create(babyID: id))
        #expect(!model.canSave)
        model.head = "36"
        #expect(model.canSave)
        model.head = ""
        model.length = "55"
        model.lengthPosition = .lying
        #expect(model.canSave)
        model.length = ""
        model.lengthPosition = nil
        model.weight = "4.2"
        #expect(model.canSave)
        model.length = "55"
        #expect(!model.canSave)
        model.lengthPosition = .lying
        #expect(model.canSave)
        model.lengthPosition = .standing
        model.head = "36"
        #expect(model.canSave)
        for text in ["oops", "0", "-2", "nan", "1.2.3"] {
            model.weight = text
            #expect(!model.canSave)
        }
    }
    @Test func validatesDatesAndSavesBackdatedValues() throws {
        let (store, id, clock) = try setup()
        let model = MeasurementSheetModel(store: store, mode: .create(babyID: id))
        model.weight = "4,2"
        model.when = clock.now.addingTimeInterval(1)
        #expect(!model.canSave)
        model.when = clock.now.addingTimeInterval(-1728000)
        #expect(!model.canSave)
        model.when = clock.now.addingTimeInterval(-86400)
        #expect(model.save())
        let entry = try #require(store.measurements(for: id).first)
        #expect(entry.weightKG == 4.2)
        #expect(entry.measuredAt == model.when)
        let edit = MeasurementSheetModel(store: store, mode: .edit(entry: entry))
        edit.weight = "4.5"
        #expect(edit.save())
        #expect(entry.weightKG == 4.5)
        #expect(entry.loggedAt == clock.now)
        #expect(try store.measurements(for: id).count == 1)
        let small = try store.logMeasurement(for: id, at: clock.now, timeZoneID: "UTC", values: MeasurementValues(weightKG: 0.000001))
        let smallEdit = MeasurementSheetModel(store: store, mode: .edit(entry: small))
        #expect(smallEdit.canSave)
        #expect(smallEdit.save())
        #expect(small.weightKG == 0.000001)
    }
    @Test func creationUsesOneOpeningTimeWithLiveClock() throws {
        let store = try BabyStore.inMemory(dateProvider: SystemDateProvider())
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let model = MeasurementSheetModel(store: store, mode: .create(babyID: baby.id))
        model.weight = "4.2"
        #expect(model.when == model.latestAllowedDate)
        #expect(model.canSave)
    }
    @Test func failedSaveRetainsInput() throws {
        let (store, id, _) = try setup()
        let model = MeasurementSheetModel(store: store, mode: .create(babyID: id))
        model.weight = "4.2"
        try store.setActiveRealm(.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID()))
        #expect(!model.save())
        #expect(model.errorMessage != nil)
        #expect(model.weight == "4.2")
    }
    @Test func defaultsPositionBySelectedCalendarDayAndPreservesOverride() throws {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = .current
        let birth = calendar.date(from: DateComponents(year: 2022, month: 1, day: 1, hour: 12))!
        let day730 = calendar.date(from: DateComponents(year: 2023, month: 12, day: 31, hour: 12))!
        let day731 = calendar.date(from: DateComponents(year: 2024, month: 1, day: 1, hour: 12))!
        let clock = TestDateProvider(day730)
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: birth, referenceSex: .girls, expectedDueDate: nil)
        let young = MeasurementSheetModel(store: store, mode: .create(babyID: baby.id))
        #expect(young.lengthPosition == .lying)
        clock.advance(by: day731.timeIntervalSince(day730))
        let older = MeasurementSheetModel(store: store, mode: .create(babyID: baby.id))
        older.length = "80.5"
        #expect(older.lengthPosition == .standing)
        older.when = day730
        #expect(older.lengthPosition == .lying)
        older.when = day731
        #expect(older.lengthPosition == .standing)
        older.lengthPosition = .lying
        older.when = day730
        older.when = day731
        #expect(older.lengthPosition == .lying)
        #expect(older.length == "80.5")
        #expect(older.save())
        let entry = try #require(store.measurements(for: baby.id).first)
        let edit = MeasurementSheetModel(store: store, mode: .edit(entry: entry))
        edit.when = day730
        edit.when = day731
        #expect(edit.lengthPosition == .lying)
    }

    @Test func leapDayBirthDefaultsAtSecondCalendarBirthdayAndIgnoresDueDate() throws {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = .current
        let birth = calendar.date(from: DateComponents(year: 2024, month: 2, day: 29, hour: 12))!
        let before = calendar.date(from: DateComponents(year: 2026, month: 2, day: 27, hour: 12))!
        let birthday = calendar.date(from: DateComponents(year: 2026, month: 2, day: 28, hour: 12))!
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(birthday))
        let baby = try store.addBaby(name: "Mira", birthDate: birth, referenceSex: .girls,
                                    expectedDueDate: LocalDate("2024-04-01"))
        let model = MeasurementSheetModel(store: store, mode: .create(babyID: baby.id))
        #expect(model.lengthPosition == .standing)
        model.when = before
        #expect(model.lengthPosition == .lying)
        model.when = birthday
        #expect(model.lengthPosition == .standing)
    }

    @Test func explainsInvalidFieldsWithoutDiscardingInput() throws {
        let (store, id, clock) = try setup()
        let model = MeasurementSheetModel(store: store, mode: .create(babyID: id))
        model.weight = "4.2"
        model.length = "oops"
        #expect(model.validationMessage(for: .length) == LoggingL10n.text("Enter a positive number for length or height."))
        #expect(model.validationMessage(for: .weight) == nil)
        #expect(!model.canSave)
        #expect(model.length == "oops")
        model.length = "55"
        model.lengthPosition = nil
        #expect(model.validationMessage(for: .length) == nil)
        #expect(model.validationMessage(for: .position) != nil)
        model.weight = "0"
        model.head = "nan"
        #expect(model.validationMessage(for: .weight) != nil)
        #expect(model.validationMessage(for: .head) != nil)
        model.when = clock.now.addingTimeInterval(-1728000)
        #expect(model.validationMessage(for: .date) == LoggingL10n.text("Choose a date on or after the baby’s birth date."))
        model.when = clock.now.addingTimeInterval(1)
        #expect(model.validationMessage(for: .date) == LoggingL10n.text("Choose a date and time that is not in the future."))
        model.when = clock.now
        model.weight = "4.2"
        model.head = ""
        model.lengthPosition = .lying
        #expect(model.validationMessage(for: .date) == nil)
        #expect(model.canSave)
    }

    @Test func editModeDeleteTombstonesTheCheckIn() throws {
        let (store, id, clock) = try setup()
        let entry = try store.logMeasurement(for: id, at: clock.now.addingTimeInterval(-3600), timeZoneID: "UTC",
                                             values: MeasurementValues(weightKG: 4.2))
        let model = MeasurementSheetModel(store: store, mode: .edit(entry: entry))
        let logged = try store.changeLog().count
        #expect(model.isEditing)

        try model.delete()

        #expect(entry.isTombstoned)
        #expect(try store.measurements(for: id).isEmpty)
        let log = try store.changeLog()
        #expect(log.count == logged + 1)
        #expect(log.last?.operation == .delete)
    }

    @Test func createModeHasNothingToDelete() throws {
        let (store, id, _) = try setup()
        let model = MeasurementSheetModel(store: store, mode: .create(babyID: id))
        #expect(!model.isEditing)
        #expect(throws: EditModeError.invalid) { try model.delete() }
    }

}
