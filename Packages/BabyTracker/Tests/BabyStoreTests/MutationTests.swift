import Testing
import Foundation
import SwiftData
import BabyCore
@testable import BabyStore

@MainActor
@Test func loggingADiaperStampsNowAndTheDeviceTimezone() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    let entry = try store.logDiaper(.wet, for: baby.id)

    #expect(entry.startedAt == clock.now)
    #expect(entry.contents == .wet)
    #expect(entry.timeZoneID == TimeZone.current.identifier)
}

@MainActor
@Test func loggingAcceptsABackdatedTime() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    let fortyMinutesAgo = clock.now.addingTimeInterval(-2_400)
    let entry = try store.logDiaper(.dirty, for: baby.id, at: fortyMinutesAgo)

    #expect(entry.startedAt == fortyMinutesAgo)
}

@MainActor
@Test func softDeleteKeepsTheRowAndLogsTheDeletion() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let entry = try store.logDiaper(.wet, for: baby.id)

    try store.softDelete(entry)

    // The row survives — a sync node cannot replicate a deletion it never saw.
    let all = try store.context.fetch(FetchDescriptor<DiaperEntry>())
    #expect(all.count == 1)
    #expect(all[0].isTombstoned)

    let log = try store.changeLog()
    #expect(log.last?.operation == .delete)
}

@MainActor
@Test func everyMutationTypeProducesALogEntry() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))

    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)      // baby + profile creates
    let entry = try store.logDiaper(.wet, for: baby.id)              // 3 create
    entry.contents = .mixed
    try store.update(entry)                                          // 4 update
    try store.softDelete(entry)                                      // 5 delete

    #expect(try store.changeLog().count == 5)
}

@MainActor
@Test func updateRefreshesLastModified() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let entry = try store.logDiaper(.wet, for: baby.id)
    let originalModified = entry.lastModifiedAt

    clock.advance(by: 300)
    entry.contents = .dirty
    try store.update(entry)

    #expect(entry.lastModifiedAt == clock.now)
    #expect(entry.lastModifiedAt > originalModified)
}
