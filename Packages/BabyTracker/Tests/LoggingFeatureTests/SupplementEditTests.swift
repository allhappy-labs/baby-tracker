import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

@Suite @MainActor struct SupplementEditTests {
    @Test func opensWithTheEntrysNameTimeAndNote() throws {
        let (store, baby, clock) = try fixture()
        let at = clock.now.addingTimeInterval(-3600)
        try store.logSupplement(name: "Vitamin D", for: baby)
        clock.advance(by: 10)
        let iron = try store.logSupplement(name: "Iron", for: baby, at: at, note: "with food")

        let model = SupplementSheetModel(store: store, babyID: baby, editing: iron.id)

        #expect(model.isEditing)
        #expect(!model.isAddingName)
        #expect(model.showsPicker)
        #expect(model.selectedName == "Iron")
        #expect(model.when == at)
        #expect(model.note == "with food")
        #expect(!model.isGone)
        #expect(model.canLog)
    }

    @Test func renameAndMoveTheTime() throws {
        let (store, baby, clock) = try fixture()
        try store.logSupplement(name: "Vitamin D", for: baby)
        clock.advance(by: 10)
        let iron = try store.logSupplement(name: "Iron", for: baby)
        let model = SupplementSheetModel(store: store, babyID: baby, editing: iron.id)
        let logged = try store.changeLog().count
        let moved = clock.now.addingTimeInterval(-5400)

        model.selectedName = "Vitamin D"
        model.when = moved
        try model.save()

        #expect(iron.name == "Vitamin D")
        #expect(iron.startedAt == moved)
        #expect(iron.note == nil)
        let log = try store.changeLog()
        #expect(log.count == logged + 1)
        #expect(log.last?.operation == .update)
        #expect(log.last?.entityID == iron.id)
        #expect(try store.supplementRows(for: baby).count == 2)
    }

    @Test func renameToANewTypedName() throws {
        let (store, baby, _) = try fixture()
        let entry = try store.logSupplement(name: "Vitamin D", for: baby, note: "drops")
        let model = SupplementSheetModel(store: store, babyID: baby, editing: entry.id)

        model.addName()
        model.newName = "  Zinc "
        model.note = " "
        try model.save()

        #expect(entry.name == "Zinc")
        #expect(entry.note == nil)
    }

    @Test func deleteTombstonesTheEntry() throws {
        let (store, baby, _) = try fixture()
        let entry = try store.logSupplement(name: "Iron", for: baby)
        let model = SupplementSheetModel(store: store, babyID: baby, editing: entry.id)

        try model.delete()

        #expect(entry.isTombstoned)
        #expect(try store.lastSupplement(for: baby) == nil)
        #expect(try store.changeLog().last?.operation == .delete)
    }

    @Test func becomesGoneWhenTheEntryIsDeletedElsewhere() throws {
        let (store, baby, _) = try fixture()
        let entry = try store.logSupplement(name: "Iron", for: baby)
        let model = SupplementSheetModel(store: store, babyID: baby, editing: entry.id)

        try store.softDelete(entry)
        model.reload()

        #expect(model.isGone)
        #expect(!model.canLog)
        #expect(throws: EditModeError.gone) { try model.save() }
    }

    @Test func createModeIsNotEditing() throws {
        let (store, baby, _) = try fixture()
        let model = SupplementSheetModel(store: store, babyID: baby)
        #expect(!model.isEditing)
        #expect(!model.isGone)
    }

    @Test func aFutureTimeIsRefused() throws {
        let (store, baby, clock) = try fixture()
        let entry = try store.logSupplement(name: "Iron", for: baby)
        let model = SupplementSheetModel(store: store, babyID: baby, editing: entry.id)

        model.when = clock.now.addingTimeInterval(60)

        #expect(!model.canLog)
        #expect(throws: EditModeError.invalid) { try model.save() }
    }

    @Test func aRefusedDeleteLeavesTheEntryLive() throws {
        let (store, baby, _) = try fixture()
        let family = RealmID.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID())
        try store.setActiveRealm(family)
        let entry = try store.logSupplement(name: "Iron", for: baby)
        let model = SupplementSheetModel(store: store, babyID: baby, editing: entry.id)
        let modifiedAt = entry.lastModifiedAt
        try store.setWritePermission(.viewer, for: family)

        #expect(throws: WritePermissionError.readOnlyCaregiver) { try model.delete() }

        #expect(!entry.isTombstoned)
        #expect(entry.lastModifiedAt == modifiedAt)
        #expect(try store.lastSupplement(for: baby)?.id == entry.id)
    }

    @Test func anEntryOfAnotherRealmIsGone() throws {
        let (store, baby, _) = try fixture()
        let entry = try store.logSupplement(name: "Iron", for: baby)
        let model = SupplementSheetModel(store: store, babyID: baby, editing: entry.id)

        try store.setActiveRealm(.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID()))
        model.reload()

        #expect(model.isGone)
    }

    // MARK: - Ported from the old generic editor (SupplementHistoryTests)

    @Test func aBlankNameIsRefusedAndTheEntryKeepsItsName() throws {
        let (store, baby, _) = try fixture()
        let entry = try store.logSupplement(name: "Iron", for: baby)
        let model = SupplementSheetModel(store: store, babyID: baby, editing: entry.id)

        model.addName()
        model.newName = " "

        #expect(throws: EditModeError.invalid) { try model.save() }
        #expect(try store.lastSupplement(for: baby)?.name == "Iron")
    }

    @Test func saveIsOffUntilATypedNameIsValid() throws {
        let (store, baby, _) = try fixture()
        let entry = try store.logSupplement(name: "Iron", for: baby)
        let model = SupplementSheetModel(store: store, babyID: baby, editing: entry.id)
        #expect(model.canLog)

        model.addName()
        for invalid in ["", " \u{2003} ", String(repeating: "a", count: 65), "Vitamin\nD", "Iron\u{0000}"] {
            model.newName = invalid
            #expect(!model.canLog, "\(invalid.debugDescription) was accepted")
        }
        for valid in ["  Vitamin D  ", String(repeating: "a", count: 64), "Cafe\u{0301}"] {
            model.newName = valid
            #expect(model.canLog, "\(valid.debugDescription) was refused")
        }
    }
}
