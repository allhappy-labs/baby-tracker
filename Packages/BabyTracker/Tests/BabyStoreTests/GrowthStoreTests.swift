import Foundation
import SwiftData
import Testing
import BabyCore
import BabyProtocol
@testable import BabyStore

@MainActor
@Suite("Growth local store")
struct GrowthStoreTests {
    let instant = Date(timeIntervalSince1970: 1_790_000_000)

    func fixture() throws -> (BabyStore, Baby, TestDateProvider) {
        let clock = TestDateProvider(instant)
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: instant.addingTimeInterval(-30 * 86400),
                                     referenceSex: .girls, expectedDueDate: nil)
        return (store, baby, clock)
    }

    @Test func creationRequiresAndPersistsProfileInOneRevision() throws {
        let (store, baby, _) = try fixture()
        let profile = try #require(try store.growthProfile(for: baby.id))
        #expect(profile.id == GrowthProfileID.forBaby(baby.id))
        #expect(profile.referenceSex == .girls)
        #expect(profile.expectedDueDate == nil)
        #expect(profile.realmID == baby.realmID)
        #expect(store.revision == 1)
        #expect(Set(try store.changeLog().map(\.entityType)) == ["Baby", "GrowthProfile"])
        #expect(try store.pendingOutbox().isEmpty)
    }

    @Test func profileUpdateClearsDueDateAndCombinedEditPreservesLegacyAbsence() throws {
        let (store, baby, _) = try fixture()
        try store.setGrowthProfile(for: baby.id, referenceSex: .boys, expectedDueDate: LocalDate("2026-09-01"))
        try store.updateBabyAndGrowthProfile(baby, name: " Mira corrected ", birthDate: instant,
                                            referenceSex: .girls, expectedDueDate: nil)
        #expect(baby.name == "Mira corrected")
        #expect(baby.birthDate == instant)
        #expect(baby.localBirthDate == (try LocalDate(displaying: instant, in: .current)))
        #expect(try store.growthProfile(for: baby.id)?.expectedDueDate == nil)
        #expect(try store.growthProfile(for: baby.id)?.referenceSex == .girls)
        #expect(try store.context.fetch(FetchDescriptor<GrowthProfile>()).count == 1)
        let legacy = Baby(name: "Legacy", birthDate: instant)
        store.context.insert(legacy)
        try store.context.save()
        try store.updateBabyAndGrowthProfile(legacy, name: "Legacy edited", birthDate: instant,
                                            referenceSex: nil, expectedDueDate: nil)
        #expect(try store.growthProfile(for: legacy.id) == nil)
        try store.setGrowthProfile(for: legacy.id, referenceSex: .boys, expectedDueDate: nil)
        #expect(try store.growthProfile(for: legacy.id)?.id == GrowthProfileID.forBaby(legacy.id))
    }

    @Test func offlineCRUDPreservesLoggingTimeAndSortsByMeasurementThenID() throws {
        let (store, baby, clock) = try fixture()
        let measured = instant.addingTimeInterval(-3600)
        let a = try store.logMeasurement(for: baby.id, at: measured, timeZoneID: "Europe/Zurich", values: MeasurementValues(weightKG: 4))
        clock.advance(by: 10)
        let b = try store.logMeasurement(for: baby.id, at: measured, timeZoneID: "UTC", values: MeasurementValues(headCircumferenceCM: 35))
        let c = try store.logMeasurement(for: baby.id, at: instant, timeZoneID: "UTC", values: MeasurementValues(lengthCM: 55, lengthPosition: .lying))
        #expect(try store.measurements(for: baby.id).map(\.id) == [c.id] + [a.id, b.id].sorted { $0.uuidString < $1.uuidString })
        let logged = a.loggedAt
        clock.advance(by: 100)
        try store.editMeasurement(a, at: measured.addingTimeInterval(-60), timeZoneID: "America/New_York",
                                  values: MeasurementValues(lengthCM: 56, lengthPosition: .standing))
        #expect(a.loggedAt == logged)
        #expect(a.weightKG == nil)
        #expect(a.lengthCM == 56)
        #expect(a.lengthPosition == .standing)
        #expect(a.lastModifiedAt == clock.now)
        try store.softDelete(b)
        #expect(b.isTombstoned)
        #expect(try store.measurements(for: baby.id).count == 2)
        #expect(try store.context.fetch(FetchDescriptor<MeasurementCheckIn>()).count == 3)
        #expect(try store.pendingOutbox().isEmpty)
    }

    @Test func invalidDatesZonesAndMissingBabiesLeaveNoRecords() throws {
        let (store, baby, _) = try fixture()
        let count = try store.changeLog().count
        let values = try MeasurementValues(weightKG: 4)
        for date in [instant.addingTimeInterval(1), instant.addingTimeInterval(-40 * 86400),
                     Date(timeIntervalSince1970: .nan), Date(timeIntervalSince1970: .infinity)] {
            #expect(throws: (any Error).self) { try store.logMeasurement(for: baby.id, at: date, timeZoneID: "UTC", values: values) }
        }
        #expect(throws: (any Error).self) { try store.logMeasurement(for: baby.id, at: instant, timeZoneID: "Invalid/Zone", values: values) }
        #expect(throws: (any Error).self) { try store.logMeasurement(for: UUID(), at: instant, timeZoneID: "UTC", values: values) }
        #expect(throws: (any Error).self) { try store.setGrowthProfile(for: UUID(), referenceSex: .girls, expectedDueDate: nil) }
        #expect(try store.measurements(for: baby.id).isEmpty)
        #expect(try store.changeLog().count == count)
    }

    @Test func birthBoundaryUsesMeasurementZoneCalendarDay() throws {
        let (store, baby, _) = try fixture()
        let birth = try LocalDate(displaying: baby.birthDate, in: .current)
        let formatter = ISO8601DateFormatter()
        let utcMidnight = try #require(formatter.date(from: birth.iso8601 + "T00:00:00Z"))
        // It is still the previous date in New York, but birth day in Tokyo.
        #expect(throws: (any Error).self) { try store.logMeasurement(for: baby.id, at: utcMidnight, timeZoneID: "America/New_York", values: MeasurementValues(weightKG: 3)) }
        _ = try store.logMeasurement(for: baby.id, at: utcMidnight, timeZoneID: "Asia/Tokyo", values: MeasurementValues(weightKG: 3))
        // Later correction preserves existing raw history even if now pre-birth.
        try store.updateBabyAndGrowthProfile(baby, name: baby.name, birthDate: instant, referenceSex: .girls, expectedDueDate: nil)
        #expect(try store.measurements(for: baby.id).count == 1)
    }

    @Test func storedBirthDayWinsOverViewerTimeZone() throws {
        let (store, baby, _) = try fixture()
        baby.birthDateISO = try LocalDate(displaying: instant, in: TimeZone(secondsFromGMT: 0)!).iso8601
        try store.context.save()
        #expect(throws: GrowthStoreError.measurementBeforeBirth) {
            try store.logMeasurement(for: baby.id, at: instant.addingTimeInterval(-86400), timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        }
    }

    @Test func hiddenLocalBabyRetainsRowsAndRejectsNewWrites() throws {
        let (store, baby, _) = try fixture()
        let entry = try store.logMeasurement(for: baby.id, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        try store.deleteBaby(baby)
        #expect(try store.growthProfile(for: baby.id) != nil)
        #expect(try store.measurements(for: baby.id).map(\.id) == [entry.id])
        #expect(!entry.isTombstoned)
        #expect(throws: (any Error).self) { try store.logMeasurement(for: baby.id, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4)) }
        #expect(throws: (any Error).self) { try store.setGrowthProfile(for: baby.id, referenceSex: .boys, expectedDueDate: nil) }
    }

    @Test func realmIsolationRejectsStaleObjectsWithoutMutatingThem() throws {
        let (store, baby, _) = try fixture()
        let row = try store.logMeasurement(for: baby.id, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        try store.setActiveRealm(Fixture.sharedRealm)
        #expect(try store.growthProfile(for: baby.id) == nil)
        #expect(try store.measurements(for: baby.id).isEmpty)
        #expect(throws: RealmWriteError.wrongRealm) { try store.editMeasurement(row, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 5)) }
        #expect(throws: RealmWriteError.wrongRealm) { try store.softDelete(row) }
        #expect(throws: RealmWriteError.wrongRealm) { try store.updateBabyAndGrowthProfile(baby, name: "wrong", birthDate: instant, referenceSex: .boys, expectedDueDate: nil) }
        #expect(row.weightKG == 4)
        #expect(!row.isTombstoned)
        #expect(baby.name == "Mira")
        let twin = Baby(id: baby.id, name: "Shared", birthDate: baby.birthDate, realm: Fixture.sharedRealm)
        store.context.insert(twin)
        try store.context.save()
        try store.setGrowthProfile(for: twin.id, referenceSex: .boys, expectedDueDate: nil)
        #expect(try store.growthProfile(for: twin.id)?.referenceSex == .boys)
        try store.setActiveRealm(.local)
        #expect(try store.growthProfile(for: baby.id)?.referenceSex == .girls)
    }

    @Test func detachedBabyCannotPartiallyUpdateAStoredProfile() throws {
        let (store, baby, _) = try fixture()
        let detached = Baby(id: baby.id, name: baby.name, birthDate: baby.birthDate)
        #expect(throws: GrowthStoreError.babyUnavailable) {
            try store.updateBabyAndGrowthProfile(detached, name: "Detached", birthDate: instant,
                                                referenceSex: .boys, expectedDueDate: nil)
        }
        #expect(baby.name == "Mira")
        #expect(try store.growthProfile(for: baby.id)?.referenceSex == .girls)
    }

    @Test func viewerFailuresKeepEditAndDeletionStateUntouched() throws {
        let (store, _, _) = try fixture()
        try store.setActiveRealm(Fixture.sharedRealm)
        let baby = try store.addBaby(name: "Shared", birthDate: instant, referenceSex: .girls, expectedDueDate: nil)
        let row = try store.logMeasurement(for: baby.id, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        try store.setWritePermission(.viewer, for: Fixture.sharedRealm)
        let count = try store.changeLog().count
        #expect(throws: WritePermissionError.readOnlyCaregiver) { try store.editMeasurement(row, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 5)) }
        #expect(throws: WritePermissionError.readOnlyCaregiver) { try store.softDelete(row) }
        #expect(throws: WritePermissionError.readOnlyCaregiver) { try store.setGrowthProfile(for: baby.id, referenceSex: .boys, expectedDueDate: nil) }
        #expect(throws: WritePermissionError.readOnlyCaregiver) { try store.updateBabyAndGrowthProfile(baby, name: "wrong", birthDate: instant, referenceSex: .boys, expectedDueDate: nil) }
        #expect(throws: WritePermissionError.readOnlyCaregiver) { try store.logMeasurement(for: baby.id, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 5)) }
        #expect(row.weightKG == 4)
        #expect(!row.isTombstoned)
        #expect(baby.name == "Shared")
        #expect(try store.growthProfile(for: baby.id)?.referenceSex == .girls)
        #expect(try store.changeLog().count == count)
    }

    @Test func diskSaveFailureRollsBackBabyAndProfileCreationTogether() throws {
        let harness = try LeaseTestHarness()
        defer { try? FileManager.default.removeItem(at: harness.directory) }
        _ = try harness.store.captureWriteContext()
        let store = try readOnlyStore(at: harness.storeURL, lock: harness.lockURL)
        #expect(throws: (any Error).self) {
            try store.addBaby(name: "Mira", birthDate: instant, referenceSex: .girls, expectedDueDate: nil)
        }
        let persisted = ModelContext(store.container)
        #expect(try persisted.fetch(FetchDescriptor<Baby>()).isEmpty)
        #expect(try persisted.fetch(FetchDescriptor<GrowthProfile>()).isEmpty)
        #expect(try persisted.fetch(FetchDescriptor<ChangeLogEntry>()).isEmpty)
        #expect(try store.babies().isEmpty)
        #expect(try store.context.fetch(FetchDescriptor<GrowthProfile>()).isEmpty)
        try expectCleanFailedWrite(store, logs: 0, outbox: 0)
    }

    @Test func failedCombinedEditRestoresHeldBabyAndProfileAndAllowsRetry() throws {
        let (harness, store, baby, _) = try failedSaveFixture()
        defer { try? FileManager.default.removeItem(at: harness.directory) }
        let profile = try #require(try store.growthProfile(for: baby.id))
        let birth = baby.birthDate
        let birthISO = baby.birthDateISO
        let modified = baby.lastModifiedAt
        let profileModified = profile.lastModifiedAt
        let logCount = try store.changeLog().count
        let outboxCount = try store.pendingOutbox().count
        for _ in 0..<2 {
            expectPhysicalSaveFailure {
                try store.updateBabyAndGrowthProfile(baby, name: "Changed", birthDate: instant,
                                                    referenceSex: .boys, expectedDueDate: LocalDate("2026-10-01"))
            }
            // These must be the first reads after failure, before any query.
            #expect(baby.name == "Mira")
            #expect(baby.birthDate == birth)
            #expect(baby.birthDateISO == birthISO)
            #expect(baby.lastModifiedAt == modified)
            #expect(profile.referenceSex == .girls)
            #expect(profile.expectedDueDate == nil)
            #expect(profile.lastModifiedAt == profileModified)
            #expect(try store.baby(baby.id)?.name == "Mira")
            #expect(try store.babies().first?.name == "Mira")
            #expect(baby.name == "Mira")
            try expectCleanFailedWrite(store, logs: logCount, outbox: outboxCount)
        }
        #expect(try ModelContext(store.container).fetch(FetchDescriptor<Baby>()).first?.name == "Mira")
    }

    @Test func failedMeasurementEditRestoresHeldValuesAndAllowsRetry() throws {
        let (harness, store, _, row) = try failedSaveFixture()
        defer { try? FileManager.default.removeItem(at: harness.directory) }
        let measured = row.measuredAt
        let logged = row.loggedAt
        let modified = row.lastModifiedAt
        let logCount = try store.changeLog().count
        let outboxCount = try store.pendingOutbox().count
        for _ in 0..<2 {
            expectPhysicalSaveFailure {
                try store.editMeasurement(row, at: instant.addingTimeInterval(-60), timeZoneID: "Asia/Tokyo",
                                          values: MeasurementValues(lengthCM: 56, lengthPosition: .standing))
            }
            #expect(row.weightKG == 4)
            #expect(row.lengthCM == nil)
            #expect(row.lengthPosition == nil)
            #expect(row.measuredAt == measured)
            #expect(row.timeZoneID == "UTC")
            #expect(row.loggedAt == logged)
            #expect(row.lastModifiedAt == modified)
            try expectCleanFailedWrite(store, logs: logCount, outbox: outboxCount)
        }
        #expect(try ModelContext(store.container).fetch(FetchDescriptor<MeasurementCheckIn>()).first?.weightKG == 4)
    }

    @Test func failedMeasurementDeletionRestoresHeldTombstoneAndAllowsRetry() throws {
        let (harness, store, baby, row) = try failedSaveFixture()
        defer { try? FileManager.default.removeItem(at: harness.directory) }
        let modified = row.lastModifiedAt
        let logCount = try store.changeLog().count
        let outboxCount = try store.pendingOutbox().count
        for _ in 0..<2 {
            // A retry must reach the physical save, never measurementUnavailable
            // due to a stale tombstone from the previous failed attempt.
            expectPhysicalSaveFailure { try store.softDelete(row) }
            #expect(!row.isTombstoned)
            #expect(row.lastModifiedAt == modified)
            #expect(try store.measurements(for: baby.id).map(\.id) == [row.id])
            try expectCleanFailedWrite(store, logs: logCount, outbox: outboxCount)
        }
        #expect(try ModelContext(store.container).fetch(FetchDescriptor<MeasurementCheckIn>()).first?.isTombstoned == false)
    }

    private func failedSaveFixture() throws -> (LeaseTestHarness, BabyStore, Baby, MeasurementCheckIn) {
        let harness = try LeaseTestHarness()
        try harness.store.setActiveRealm(Fixture.sharedRealm)
        let seeded = try harness.store.addBaby(name: "Mira", birthDate: instant.addingTimeInterval(-86400),
                                              referenceSex: .girls, expectedDueDate: nil)
        _ = try harness.store.logMeasurement(for: seeded.id, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        let store = try readOnlyStore(at: harness.storeURL, lock: harness.lockURL)
        let baby = try #require(try store.baby(seeded.id))
        let row = try #require(try store.measurements(for: baby.id).first)
        return (harness, store, baby, row)
    }

    private func expectPhysicalSaveFailure(_ body: () throws -> Void) {
        do {
            try body()
            Issue.record("Expected physical save rejection")
        } catch {
            #expect((error as NSError).domain == NSCocoaErrorDomain)
            #expect((error as NSError).code == NSFileWriteNoPermissionError)
        }
    }

    private func expectCleanFailedWrite(_ store: BabyStore, logs: Int, outbox: Int) throws {
        #expect(try store.changeLog().count == logs)
        #expect(try store.pendingOutbox().count == outbox)
        #expect(!store.context.hasChanges)
        #expect(store.context.insertedModelsArray.isEmpty)
        #expect(store.context.changedModelsArray.isEmpty)
        let persisted = ModelContext(store.container)
        #expect(try persisted.fetch(FetchDescriptor<ChangeLogEntry>()).count == logs)
        #expect(try persisted.fetch(FetchDescriptor<SyncOutboxEntry>()).count == outbox)
    }

    // SwiftData's read-only configuration rejects the actual save, after the
    // API has mutated rows and recorded its change log inside the boundary.
    private func readOnlyStore(at url: URL, lock: URL) throws -> BabyStore {
        let schema = Schema(versionedSchema: SchemaV12.self)
        let container = try BabySchema.makeContainer(schema: schema,
            configurations: [ModelConfiguration(schema: schema, url: url, allowsSave: false)])
        return BabyStore(container: container, dateProvider: TestDateProvider(instant), deviceID: "readonly-test", coordinationLockURL: lock)
    }

    @Test func invalidCombinedEditAndMeasurementEditLeaveSavedStateUnchanged() throws {
        let (store, baby, _) = try fixture()
        let row = try store.logMeasurement(for: baby.id, at: instant, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4))
        #expect(throws: (any Error).self) { try store.updateBabyAndGrowthProfile(baby, name: "  ", birthDate: instant, referenceSex: .boys, expectedDueDate: nil) }
        #expect(throws: (any Error).self) { try store.editMeasurement(row, at: instant.addingTimeInterval(5), timeZoneID: "UTC", values: MeasurementValues(weightKG: 5)) }
        #expect(baby.name == "Mira")
        #expect(try store.growthProfile(for: baby.id)?.referenceSex == .girls)
        #expect(row.weightKG == 4)
        #expect(row.measuredAt == instant)
    }
}
