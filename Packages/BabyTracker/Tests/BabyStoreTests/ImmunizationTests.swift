import Foundation
import SwiftData
import Testing
import BabyCore
import BabyProtocol
@testable import BabyStore

@MainActor @Suite("Immunizations")
struct ImmunizationTests {
    /// Noon UTC, so a day's shift never crosses midnight in the test zone.
    static let noon = Date(timeIntervalSince1970: 1_800_014_400)

    private func setup(bornDaysAgo: Double = 200) throws -> (BabyStore, Baby, TestDateProvider) {
        let clock = TestDateProvider(Self.noon)
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: clock.now.addingTimeInterval(-bornDaysAgo * 86_400),
                                     referenceSex: .girls, expectedDueDate: nil)
        return (store, baby, clock)
    }

    @Test func aShotIsLoggedEditedAndSoftDeleted() throws {
        let (store, baby, clock) = try setup()
        let shot = try store.logImmunization(vaccine: " Prevenar 13 ", dose: 1, for: baby.id, note: "left thigh")
        #expect(shot.vaccine == "Prevenar 13")
        #expect(shot.dose == 1)
        #expect(shot.givenAt == clock.now)
        #expect(shot.timeZoneID == TimeZone.current.identifier)
        #expect(try store.immunizations(for: baby.id).map(\.id) == [shot.id])
        let earlier = clock.now.addingTimeInterval(-86_400)
        try store.editImmunization(shot, vaccine: "Prevenar 20", dose: nil, at: earlier, note: nil)
        #expect(shot.vaccine == "Prevenar 20")
        #expect(shot.dose == nil)
        #expect(shot.givenAt == earlier)
        #expect(shot.note == nil)
        try store.softDelete(shot)
        #expect(try store.immunizations(for: baby.id).isEmpty)
        #expect(try store.vaccineNames().isEmpty)
    }

    @Test func invalidNamesAndDosesWriteNothing() throws {
        let (store, baby, _) = try setup()
        #expect(throws: SupplementName.ValidationError.self) { try store.logImmunization(vaccine: "   ", dose: nil, for: baby.id) }
        #expect(throws: SupplementName.ValidationError.self) {
            try store.logImmunization(vaccine: String(repeating: "a", count: 65), dose: nil, for: baby.id)
        }
        for dose in [0, 11, -1] {
            #expect(throws: ImmunizationStoreError.invalidDose) { try store.logImmunization(vaccine: "MMR", dose: dose, for: baby.id) }
        }
        #expect(try store.logImmunization(vaccine: String(repeating: "a", count: 64), dose: 10, for: baby.id).dose == 10)
        #expect(try store.context.fetch(FetchDescriptor<ImmunizationEntry>()).count == 1)
    }

    // Review Focus 1.
    @Test func aFutureOrPreBirthTimeIsRefusedAndWritesNothing() throws {
        let (store, baby, clock) = try setup(bornDaysAgo: 10)
        #expect(throws: ImmunizationStoreError.futureDate) {
            try store.logImmunization(vaccine: "MMR", dose: nil, for: baby.id, at: clock.now.addingTimeInterval(1))
        }
        #expect(throws: ImmunizationStoreError.beforeBirth) {
            try store.logImmunization(vaccine: "MMR", dose: nil, for: baby.id, at: clock.now.addingTimeInterval(-12 * 86_400))
        }
        #expect(throws: ImmunizationStoreError.babyUnavailable) {
            try store.logImmunization(vaccine: "MMR", dose: nil, for: UUID())
        }
        #expect(try store.context.fetch(FetchDescriptor<ImmunizationEntry>()).isEmpty)
        // The birth day itself is allowed, and so is exactly now.
        let shot = try store.logImmunization(vaccine: "Hep B", dose: 1, for: baby.id, at: baby.birthDate)
        #expect(throws: ImmunizationStoreError.futureDate) {
            try store.editImmunization(shot, vaccine: "Hep B", dose: 1, at: clock.now.addingTimeInterval(60), note: nil)
        }
        #expect(shot.givenAt == baby.birthDate)
    }

    // Review Focus 3.
    @Test func namesAreRealmWideMergedAndOrderedByLatestShot() throws {
        let (store, mira, clock) = try setup()
        let noah = try store.addBaby(name: "Noah", birthDate: clock.now.addingTimeInterval(-400 * 86_400),
                                     referenceSex: .boys, expectedDueDate: nil)
        let day = 86_400.0
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: noah.id, at: clock.now.addingTimeInterval(-90 * day))
        try store.logImmunization(vaccine: "Infanrix hexa", dose: 1, for: mira.id, at: clock.now.addingTimeInterval(-30 * day))
        let respelled = try store.logImmunization(vaccine: "  prevenar 13 ", dose: 1, for: mira.id, at: clock.now.addingTimeInterval(-5 * day))
        #expect(respelled.vaccine == "Prevenar 13")
        #expect(try store.vaccineNames() == ["Prevenar 13", "Infanrix hexa"])
        let last = try store.immunizations(for: mira.id)
        try store.softDelete(last[0])
        #expect(try store.vaccineNames() == ["Infanrix hexa", "Prevenar 13"])
    }

    @Test func nextDoseCountsThisBabyOnly() throws {
        let (store, mira, clock) = try setup()
        let noah = try store.addBaby(name: "Noah", birthDate: clock.now.addingTimeInterval(-400 * 86_400),
                                     referenceSex: .boys, expectedDueDate: nil)
        #expect(try store.nextDose(of: "Prevenar 13", for: mira.id) == 1)
        try store.logImmunization(vaccine: "Prevenar 13", dose: 3, for: noah.id)
        #expect(try store.nextDose(of: "Prevenar 13", for: mira.id) == 1)
        try store.logImmunization(vaccine: "Prevenar 13", dose: nil, for: mira.id)
        #expect(try store.nextDose(of: "Prevenar 13", for: mira.id) == 1)
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: mira.id)
        let second = try store.logImmunization(vaccine: "prevenar 13", dose: 2, for: mira.id)
        #expect(try store.nextDose(of: " PREVENAR 13 ", for: mira.id) == 3)
        try store.softDelete(second)
        #expect(try store.nextDose(of: "Prevenar 13", for: mira.id) == 2)
        try store.logImmunization(vaccine: "Prevenar 13", dose: 10, for: mira.id)
        #expect(try store.nextDose(of: "Prevenar 13", for: mira.id) == 10)
        #expect(try store.nextDose(of: "   ", for: mira.id) == 1)
    }

    @Test func shotsJoinTheBabysEntriesAndNoOtherBabys() throws {
        let (store, mira, clock) = try setup()
        let noah = try store.addBaby(name: "Noah", birthDate: clock.now.addingTimeInterval(-400 * 86_400),
                                     referenceSex: .boys, expectedDueDate: nil)
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: mira.id)
        let items = try store.entries(for: mira.id, from: .distantPast, to: .distantFuture)
        #expect(items.map(\.id) == [shot.id])
        #expect(items.first?.startedAt == shot.givenAt)
        let today = DayBucket.key(for: clock.now, timeZoneID: TimeZone.current.identifier)
        #expect(try store.entries(for: mira.id, on: today).map(\.id) == [shot.id])
        #expect(try store.entries(for: noah.id, from: .distantPast, to: .distantFuture).isEmpty)
        guard case .immunization(let found)? = try store.entry(id: shot.id, for: mira.id) else {
            Issue.record("entry(id:for:) did not return the shot"); return
        }
        #expect(found.id == shot.id)
        if try store.entry(id: shot.id, for: noah.id) != nil { Issue.record("another baby's lookup returned the shot") }
        #expect(try store.lastImmunization(for: mira.id)?.id == shot.id)
    }

    @Test func aGenericUpdateNormalisesTheNameAndRollsBackABadOne() throws {
        let (store, baby, _) = try setup()
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: baby.id)
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby.id)
        shot.vaccine = " prevenar 13 "
        try store.update(shot)
        #expect(shot.vaccine == "Prevenar 13")
        shot.vaccine = "   "
        #expect(throws: (any Error).self) { try store.update(shot) }
        let saved = try ModelContext(store.container).fetch(FetchDescriptor<ImmunizationEntry>()).first { $0.id == shot.id }
        #expect(saved?.vaccine == "Prevenar 13")
    }

    @Test func aStaleEntryCannotBeEditedInAnotherRealm() throws {
        let (store, baby, clock) = try setup()
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby.id)
        try store.setActiveRealm(Fixture.sharedRealm)
        #expect(throws: RealmWriteError.wrongRealm) {
            try store.editImmunization(shot, vaccine: "MMR", dose: 2, at: clock.now, note: nil)
        }
        #expect(shot.dose == 1)
    }

    @Test func deletingABabyKeepsItsShotsOnThisPhone() throws {
        let (store, baby, _) = try setup()
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby.id)
        try store.deleteBaby(baby)
        #expect(try store.context.fetch(FetchDescriptor<ImmunizationEntry>()).map(\.id) == [shot.id])
        #expect(!shot.isTombstoned)
    }

    /// The names are the family's, but only of babies it still has: a vaccine
    /// only a deleted baby had is not offered to a sibling.
    @Test func aDeletedBabysVaccinesAreNotOffered() throws {
        let (store, mira, clock) = try setup()
        let noah = try store.addBaby(name: "Noah", birthDate: clock.now.addingTimeInterval(-400 * 86_400),
                                     referenceSex: .boys, expectedDueDate: nil)
        try store.logImmunization(vaccine: "MMR", dose: 1, for: noah.id)
        try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: noah.id)
        try store.logImmunization(vaccine: "prevenar 13", dose: 1, for: mira.id)
        try store.deleteBaby(noah)
        #expect(try store.vaccineNames() == ["Prevenar 13"])
    }

    /// A shot the other phone logged "now" with a clock running fast is in
    /// this phone's future. Its note or dose can still be edited; only moving
    /// its time to another future moment is refused.
    @Test func aShotAlreadyInTheFutureCanBeEditedWithoutMovingIt() throws {
        let (store, baby, clock) = try setup()
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby.id)
        let ahead = clock.now.addingTimeInterval(120)
        shot.givenAt = ahead
        try store.context.save()
        try store.editImmunization(shot, vaccine: "MMR", dose: 2, at: ahead, note: "sore arm")
        #expect(shot.dose == 2)
        #expect(shot.note == "sore arm")
        #expect(throws: ImmunizationStoreError.futureDate) {
            try store.editImmunization(shot, vaccine: "MMR", dose: 2, at: ahead.addingTimeInterval(60), note: nil)
        }
        try store.editImmunization(shot, vaccine: "MMR", dose: 2, at: clock.now, note: nil)
        #expect(shot.givenAt == clock.now)
    }

    @Test func theNameKeyIsOneRuleForEveryCaller() {
        #expect(BabyStore.vaccineNameKey("Prevenar 13") == BabyStore.vaccineNameKey("PREVENAR 13"))
        #expect(BabyStore.vaccineNameKey("Prevenar 13") != BabyStore.vaccineNameKey("Prevenar 20"))
    }
}
