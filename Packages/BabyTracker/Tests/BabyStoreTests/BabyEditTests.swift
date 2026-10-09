import Testing
import Foundation
import SwiftData
import BabyCore
@testable import BabyStore

@MainActor
@Test func renamingLogsAnUpdateNotACreate() throws {
    let clock = TestDateProvider(.now)
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mria", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    clock.advance(by: 60)
    try store.renameBaby(baby, to: "Mira", birthDate: baby.birthDate)

    #expect(baby.name == "Mira")
    #expect(baby.lastModifiedAt == clock.now)
    #expect(try store.changeLog().last?.operation == .update)
}

@MainActor
@Test func deletingABabyTombstonesItAndKeepsTheRow() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    try store.deleteBaby(baby)

    #expect(try store.babies().isEmpty)
    // The row survives — a sync server cannot replicate a deletion it never saw.
    let all = try store.context.fetch(FetchDescriptor<Baby>())
    #expect(all.count == 1)
    #expect(all[0].isTombstoned)
    #expect(try store.changeLog().last?.operation == .delete)
}

@MainActor
@Test func aDeletedBabysEntriesAreNotReturnedForIt() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let mira = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let theo = try store.addBaby(name: "Theo", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    _ = try store.logDiaper(.wet, for: mira.id)
    _ = try store.logDiaper(.wet, for: theo.id)

    try store.deleteBaby(mira)

    // Entries are untouched — only the baby is hidden. Deleting a child must
    // not silently destroy their log.
    #expect(try store.lastDiaper(for: mira.id) != nil)
    #expect(try store.babies().map(\.id) == [theo.id])
}

@MainActor
@Test func renamingRejectsAnEmptyName() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    #expect(throws: (any Error).self) {
        try store.renameBaby(baby, to: "   ", birthDate: baby.birthDate)
    }
    #expect(baby.name == "Mira")
}

@MainActor
@Test func loggingADiaperWithDetailIsOneWriteNotTwo() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    let entry = try store.logDiaper(
        .dirty, for: baby.id, consistency: "Seedy", color: "Mustard"
    )

    #expect(entry.consistency == "Seedy")
    #expect(entry.color == "Mustard")
    // One `.create`, never a `.create` followed by an `.update`: a failure
    // between the two left a saved entry behind a "couldn't save" banner, and
    // the obvious retry logged a second diaper.
    let log = try store.changeLog().filter { $0.entityID == entry.id }
    #expect(log.count == 1)
    #expect(log.first?.operation == .create)
}
