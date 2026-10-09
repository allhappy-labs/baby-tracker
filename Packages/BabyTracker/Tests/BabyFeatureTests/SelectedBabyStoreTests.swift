import Testing
import Foundation
import BabyCore
import BabyStore
@testable import BabyFeature

private func emptyDefaults() -> UserDefaults {
    let name = "test-\(UUID().uuidString)"
    let defaults = UserDefaults(suiteName: name)!
    defaults.removePersistentDomain(forName: name)
    return defaults
}

@MainActor
@Test func selectionPersistsAcrossInstances() throws {
    let defaults = emptyDefaults()
    let id = UUID()

    SelectedBabyStore(defaults: defaults).select(id)

    #expect(SelectedBabyStore(defaults: defaults).selectedID == id)
}

@MainActor
@Test func resolveFallsBackToTheFirstBabyWhenSelectionIsStale() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    let mira = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let babies = try store.babies()

    let selection = SelectedBabyStore(defaults: emptyDefaults())
    selection.select(UUID())   // points at a baby that no longer exists

    #expect(selection.resolve(from: babies)?.id == mira.id)
}

@MainActor
@Test func resolveReturnsNilWhenThereAreNoBabies() {
    let selection = SelectedBabyStore(defaults: emptyDefaults())
    #expect(selection.resolve(from: []) == nil)
}

@MainActor
@Test func resolveHonoursAValidSelection() throws {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    _ = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let theo = try store.addBaby(name: "Theo", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let babies = try store.babies()

    let selection = SelectedBabyStore(defaults: emptyDefaults())
    selection.select(theo.id)

    #expect(selection.resolve(from: babies)?.name == "Theo")
}
