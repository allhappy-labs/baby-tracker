import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

@Suite @MainActor struct DiaperEditTests {
    @Test func opensWithTheEntrysContentsTimeAndDetail() throws {
        let (store, baby, clock) = try fixture()
        let at = clock.now.addingTimeInterval(-3600)
        let diaper = try store.logDiaper(.dirty, for: baby, at: at, consistency: "seedy", note: "x")

        let model = DiaperEditModel(store: store, babyID: baby, diaperID: diaper.id)

        #expect(model.contents == .dirty)
        #expect(model.note == "x")
        #expect(model.consistency == "seedy")
        #expect(model.color == "")
        #expect(model.when == at)
        #expect(model.hasDetail)
        #expect(!model.isGone)
        #expect(model.canSave)
    }

    @Test func saveWritesTheNewContentsAndTimeAsOneUpdate() throws {
        let (store, baby, clock) = try fixture()
        let diaper = try store.logDiaper(.dirty, for: baby, at: clock.now.addingTimeInterval(-3600), note: "x")
        let model = DiaperEditModel(store: store, babyID: baby, diaperID: diaper.id)
        let logged = try store.changeLog().count
        let revision = store.revision
        let moved = clock.now.addingTimeInterval(-7200)

        model.contents = .wet
        model.when = moved
        model.color = " green "
        model.note = "   "
        clock.advance(by: 60)
        try model.save()

        #expect(diaper.contents == .wet)
        #expect(diaper.startedAt == moved)
        #expect(diaper.color == "green")
        #expect(diaper.note == nil)
        #expect(diaper.lastModifiedAt == clock.now)
        let log = try store.changeLog()
        #expect(log.count == logged + 1)
        #expect(log.last?.operation == .update)
        #expect(log.last?.entityID == diaper.id)
        #expect(log.filter { $0.entityID == diaper.id && $0.operation == .create }.count == 1)
        #expect(store.revision > revision)
        #expect(try store.lastDiaper(for: baby)?.id == diaper.id)
    }

    /// Ported from the old editor's detail tests: consistency, colour and
    /// note are written trimmed, and text that is only whitespace is no
    /// detail at all.
    @Test func detailIsWrittenTrimmedAndBlankDetailIsNil() throws {
        let (store, baby, clock) = try fixture()
        let diaper = try store.logDiaper(.dirty, for: baby)
        let model = DiaperEditModel(store: store, babyID: baby, diaperID: diaper.id)

        model.consistency = "Seedy"
        model.color = "Mustard"
        model.note = "  Blowout, changed the sheets  "
        clock.advance(by: 30)
        try model.save()

        #expect(diaper.consistency == "Seedy")
        #expect(diaper.color == "Mustard")
        #expect(diaper.note == "Blowout, changed the sheets")
        #expect(try store.changeLog().last?.operation == .update)

        let clearing = DiaperEditModel(store: store, babyID: baby, diaperID: diaper.id)
        clearing.consistency = "   "
        clearing.color = ""
        clearing.note = "   \n\t "
        try clearing.save()

        #expect(diaper.consistency == nil)
        #expect(diaper.color == nil)
        #expect(diaper.note == nil)
    }

    @Test func dryIsOfferedOnlyInEditMode() throws {
        #expect(DiaperSheet.choices(editing: true).contains(.dry))
        #expect(DiaperSheet.choices(editing: true) == [.wet, .dirty, .mixed, .dry])
        #expect(!DiaperSheet.choices(editing: false).contains(.dry))
        #expect(DiaperSheet.choices(editing: false) == DiaperContents.fastPath)

        let (store, baby, _) = try fixture()
        let diaper = try store.logDiaper(.wet, for: baby)
        let model = DiaperEditModel(store: store, babyID: baby, diaperID: diaper.id)
        model.contents = .dry
        try model.save()
        #expect(diaper.contents == .dry)
    }

    @Test func deleteTombstonesTheEntry() throws {
        let (store, baby, _) = try fixture()
        let diaper = try store.logDiaper(.mixed, for: baby)
        let day = DayBucket.key(for: diaper.startedAt, timeZoneID: TimeZone.current.identifier)
        #expect(try store.entries(for: baby, on: day).count == 1)
        let model = DiaperEditModel(store: store, babyID: baby, diaperID: diaper.id)

        try model.delete()

        #expect(diaper.isTombstoned)
        #expect(try store.lastDiaper(for: baby) == nil)
        #expect(try store.changeLog().last?.operation == .delete)
        // Re-fetched from the context rather than read off the tombstoned
        // object: `isDeleted` would silently reset across the save.
        #expect(try store.entries(for: baby, on: day).isEmpty)
    }

    @Test func becomesGoneWhenTheEntryIsDeletedElsewhere() throws {
        let (store, baby, _) = try fixture()
        let diaper = try store.logDiaper(.wet, for: baby)
        let model = DiaperEditModel(store: store, babyID: baby, diaperID: diaper.id)
        let revision = store.revision

        try store.softDelete(diaper)
        #expect(store.revision != revision)
        model.reload()

        #expect(model.isGone)
        #expect(!model.canSave)
        #expect(throws: EditModeError.gone) { try model.save() }
        #expect(throws: EditModeError.gone) { try model.delete() }
    }

    @Test func aMissingEntryOpensGone() throws {
        let (store, baby, _) = try fixture()
        #expect(DiaperEditModel(store: store, babyID: baby, diaperID: UUID()).isGone)
    }

    /// The other phone's change lands in every field the user has not
    /// touched; the field they are typing in keeps what they typed.
    @Test func reloadTakesAnotherPhonesChangeButKeepsTheUsersEdits() throws {
        let (store, baby, clock) = try fixture()
        let diaper = try store.logDiaper(.dirty, for: baby, note: "x")
        let model = DiaperEditModel(store: store, babyID: baby, diaperID: diaper.id)

        model.note = "mine"
        diaper.contents = .wet
        diaper.note = "theirs"
        diaper.startedAt = clock.now.addingTimeInterval(-600)
        try store.update(diaper)
        model.reload()

        #expect(model.contents == .wet)
        #expect(model.when == clock.now.addingTimeInterval(-600))
        #expect(model.note == "mine")
        #expect(!model.isGone)
    }

    @Test func aFutureTimeIsRefused() throws {
        let (store, baby, clock) = try fixture()
        let diaper = try store.logDiaper(.wet, for: baby)
        let model = DiaperEditModel(store: store, babyID: baby, diaperID: diaper.id)

        model.when = clock.now.addingTimeInterval(60)

        #expect(!model.canSave)
        #expect(throws: EditModeError.invalid) { try model.save() }
        #expect(diaper.startedAt != model.when)
    }

    /// A refused write must not leave the edit sitting unsaved in the
    /// context, where the next unrelated save would commit it without a
    /// change-log entry.
    @Test func aRefusedSaveLeavesTheEntryAsItWas() throws {
        let (store, baby, _) = try fixture()
        let family = RealmID.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID())
        try store.setActiveRealm(family)
        let diaper = try store.logDiaper(.dirty, for: baby, note: "x")
        let model = DiaperEditModel(store: store, babyID: baby, diaperID: diaper.id)
        let modifiedAt = diaper.lastModifiedAt
        try store.setWritePermission(.viewer, for: family)

        model.contents = .wet
        model.note = "changed"
        #expect(throws: WritePermissionError.readOnlyCaregiver) { try model.save() }

        #expect(diaper.contents == .dirty)
        #expect(diaper.note == "x")
        #expect(diaper.lastModifiedAt == modifiedAt)
        #expect(model.contents == .wet)
        #expect(model.note == "changed")

        #expect(throws: WritePermissionError.readOnlyCaregiver) { try model.delete() }
        #expect(!diaper.isTombstoned)
    }

    /// An entry of another realm — left behind by a switch to or from a
    /// family — is not this sheet's to edit.
    @Test func anEntryOfAnotherRealmIsGone() throws {
        let (store, baby, _) = try fixture()
        let diaper = try store.logDiaper(.wet, for: baby)
        let model = DiaperEditModel(store: store, babyID: baby, diaperID: diaper.id)

        try store.setActiveRealm(.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID()))
        model.reload()

        #expect(model.isGone)
    }

    /// The restore puts back the stored text byte for byte, not as a draft
    /// would write it: trimming legacy spaces would itself be an unlogged change.
    @Test func aRefusedSaveRestoresStoredTextUntrimmed() throws {
        let (store, baby, _) = try fixture()
        let family = RealmID.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID())
        try store.setActiveRealm(family)
        let diaper = try store.logDiaper(.dirty, for: baby, note: " legacy ")
        let model = DiaperEditModel(store: store, babyID: baby, diaperID: diaper.id)
        try store.setWritePermission(.viewer, for: family)

        model.contents = .wet
        #expect(throws: WritePermissionError.readOnlyCaregiver) { try model.save() }

        #expect(diaper.note == " legacy ")
        #expect(diaper.contents == .dirty)
    }

    @Test func aNewDiaperKeepsTheChosenDay() throws {
        let (store, baby, clock) = try fixture()
        let yesterday = clock.now.addingTimeInterval(-26 * 3600)
        try DiaperSheet.log(.wet, at: yesterday, store: store, babyID: baby)
        #expect(try store.lastDiaper(for: baby)?.startedAt == yesterday)
    }
}
