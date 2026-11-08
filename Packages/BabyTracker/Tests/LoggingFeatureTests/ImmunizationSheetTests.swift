import Foundation
import Testing
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

@MainActor @Suite("Vaccine panel")
struct ImmunizationSheetTests {
    static let noon = Date(timeIntervalSince1970: 1_800_014_400)

    private func setup(bornDaysAgo: Double = 200) throws -> (BabyStore, UUID, TestDateProvider) {
        let clock = TestDateProvider(Self.noon)
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: clock.now.addingTimeInterval(-bornDaysAgo * 86_400),
                                     referenceSex: .girls, expectedDueDate: nil)
        return (store, baby.id, clock)
    }

    @Test func withNoNamesItAsksForTheFirstAndCannotSave() throws {
        let (store, baby, _) = try setup()
        let model = ImmunizationSheetModel(store: store, babyID: baby)
        #expect(model.names.isEmpty)
        #expect(model.isAddingName)
        #expect(model.selectedName == nil)
        #expect(model.dose == nil)
        #expect(!model.canSave)
    }

    @Test func nothingIsSelectedOnOpenEvenWithOneName() throws {
        let (store, baby, _) = try setup()
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: baby)
        let model = ImmunizationSheetModel(store: store, babyID: baby)
        #expect(model.names == ["Prevenar 13"])
        #expect(!model.isAddingName)
        #expect(model.selectedName == nil)
        #expect(model.dose == nil)
        #expect(!model.canSave)
    }

    @Test func theDoseFollowsTheNameUntilChangedByHand() throws {
        let (store, baby, _) = try setup()
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: baby)
        try store.logImmunization(vaccine: "MMR", dose: 2, for: baby)
        let model = ImmunizationSheetModel(store: store, babyID: baby)
        model.choose("Prevenar 13")
        #expect(model.dose == 2)
        #expect(model.canSave)
        model.choose("MMR")
        #expect(model.dose == 3)
        model.stepDose(-1)
        #expect(model.dose == 2)
        model.choose("Prevenar 13")
        #expect(model.dose == 2)          // by hand now: it no longer follows
        model.choose(nil)
        #expect(!model.canSave)
    }

    @Test func aTypedNameSetsTheDoseAndTheStepperClearsIt() throws {
        let (store, baby, _) = try setup()
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: baby)
        let model = ImmunizationSheetModel(store: store, babyID: baby)
        model.addName()
        model.type("rota")
        #expect(model.dose == 1)
        model.type("prevenar 13")
        #expect(model.dose == 2)
        model.type("")
        #expect(model.dose == nil)
        #expect(!model.canSave)
        model.type("Rotarix")
        model.stepDose(-1)
        #expect(model.dose == nil)        // below 1 is no dose
        model.stepDose(-1)
        #expect(model.dose == nil)
        model.stepDose(1)
        #expect(model.dose == 1)
        for _ in 0..<20 { model.stepDose(1) }
        #expect(model.dose == 10)
        model.useSavedNames()
        #expect(model.selectedName == nil)
    }

    @Test func savingLogsOneShotInTheFamilysSpelling() throws {
        let (store, baby, clock) = try setup()
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: baby, at: clock.now.addingTimeInterval(-60 * 86_400))
        let model = ImmunizationSheetModel(store: store, babyID: baby)
        model.addName()
        model.type("  prevenar 13 ")
        model.note = "  left thigh  "
        #expect(model.log())
        let shots = try store.immunizations(for: baby)
        #expect(shots.count == 2)
        #expect(shots[0].vaccine == "Prevenar 13")
        #expect(shots[0].dose == 2)
        #expect(shots[0].note == "left thigh")
        #expect(shots[0].givenAt == clock.now)
    }

    // Review Focus 1.
    @Test func dateRulesDisableSaveWithAMessage() throws {
        let (store, baby, clock) = try setup(bornDaysAgo: 10)
        let model = ImmunizationSheetModel(store: store, babyID: baby)
        model.addName(); model.type("MMR")
        #expect(model.dateMessage == nil)
        #expect(model.canSave)
        model.when = clock.now.addingTimeInterval(-12 * 86_400)
        #expect(model.dateMessage == ImmunizationSheetCopy.beforeBirth)
        #expect(!model.canSave)
        #expect(!model.log())
        model.when = clock.now.addingTimeInterval(3600)
        #expect(model.dateMessage == ImmunizationSheetCopy.inTheFuture)
        #expect(!model.canSave)
        #expect(try store.immunizations(for: baby).isEmpty)
    }

    @Test func aFailedSaveKeepsTheInputAndShowsAMessage() throws {
        let (store, baby, _) = try setup()
        let model = ImmunizationSheetModel(store: store, babyID: baby)
        model.addName(); model.type("MMR")
        try store.setActiveRealm(.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID()))
        #expect(!model.log())
        #expect(model.errorMessage != nil)
        #expect(model.newName == "MMR")
    }

    @Test func editingShowsTheStoredDoseAndNeverFollowsTheName() throws {
        let (store, baby, clock) = try setup()
        try store.logImmunization(vaccine: "MMR", dose: 4, for: baby)
        let shot = try store.logImmunization(vaccine: "Prevenar 13", dose: nil, for: baby, note: "fussy after")
        let model = ImmunizationSheetModel(store: store, babyID: baby, editing: shot.id)
        #expect(model.isEditing)
        #expect(model.selectedName == "Prevenar 13")
        #expect(model.dose == nil)
        #expect(model.note == "fussy after")
        model.choose("MMR")
        #expect(model.dose == nil)
        model.stepDose(1)
        model.when = clock.now.addingTimeInterval(-86_400)
        try model.save()
        #expect(shot.vaccine == "MMR")
        #expect(shot.dose == 1)
        #expect(shot.givenAt == clock.now.addingTimeInterval(-86_400))
    }

    @Test func reloadTakesTheOtherPhonesChangeOnlyIntoUntouchedFields() throws {
        let (store, baby, clock) = try setup()
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby)
        let model = ImmunizationSheetModel(store: store, babyID: baby, editing: shot.id)
        model.note = "mine"
        try store.editImmunization(shot, vaccine: "MMR", dose: 2, at: clock.now.addingTimeInterval(-3600), note: "theirs")
        model.reload()
        #expect(model.dose == 2)
        #expect(model.when == clock.now.addingTimeInterval(-3600))
        #expect(model.note == "mine")
    }

    // Review Focus 5.
    @Test func aShotDeletedElsewhereClosesThePanel() throws {
        let (store, baby, _) = try setup()
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby)
        let model = ImmunizationSheetModel(store: store, babyID: baby, editing: shot.id)
        try store.softDelete(shot)
        model.reload()
        #expect(model.isGone)
        #expect(throws: EditModeError.gone) { try model.save() }
        #expect(throws: EditModeError.gone) { try model.delete() }
        let never = ImmunizationSheetModel(store: store, babyID: baby, editing: UUID())
        #expect(never.isGone)
    }

    /// A name the other phone's shot brings in while the panel is open is
    /// offered at once, without closing the panel or losing what was typed.
    @Test func namesSyncedInWhileLoggingAreOffered() throws {
        let (store, baby, _) = try setup()
        let model = ImmunizationSheetModel(store: store, babyID: baby)
        #expect(model.isAddingName)
        model.type("Rota")
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: baby)
        model.refreshNames()
        #expect(model.names == ["Prevenar 13"])
        #expect(model.isAddingName)
        #expect(model.newName == "Rota")
        model.useSavedNames()
        model.choose("Prevenar 13")
        #expect(model.dose == 2)
        // A name that is gone again leaves the selection empty, not dangling.
        for shot in try store.immunizations(for: baby) { try store.softDelete(shot) }
        model.refreshNames()
        #expect(model.names.isEmpty)
        #expect(model.selectedName == nil)
        #expect(model.isAddingName)
    }

    /// A shot already in this phone's future (the other phone's clock ran
    /// fast) opens editable: Save is on, and saving a note works.
    @Test func aShotAlreadyInTheFutureIsStillEditable() throws {
        let (store, baby, clock) = try setup()
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby)
        shot.givenAt = clock.now.addingTimeInterval(120)
        try store.context.save()
        let model = ImmunizationSheetModel(store: store, babyID: baby, editing: shot.id)
        #expect(model.dateMessage == nil)
        #expect(model.canSave)
        model.note = "sore arm"
        try model.save()
        #expect(shot.note == "sore arm")
    }

    @Test func deleteTombstonesTheShot() throws {
        let (store, baby, _) = try setup()
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby)
        let model = ImmunizationSheetModel(store: store, babyID: baby, editing: shot.id)
        try model.delete()
        #expect(try store.immunizations(for: baby).isEmpty)
    }
}
