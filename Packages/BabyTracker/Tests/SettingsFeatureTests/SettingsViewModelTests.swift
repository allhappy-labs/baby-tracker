import Testing
import Foundation
import BabyCore
import BabyStore
@testable import SettingsFeature

private func emptyDefaults() -> UserDefaults {
    let name = "test-\(UUID().uuidString)"
    let defaults = UserDefaults(suiteName: name)!
    defaults.removePersistentDomain(forName: name)
    return defaults
}

@MainActor
@Test func theUnitDefaultsToMillilitres() {
    #expect(SettingsViewModel(defaults: emptyDefaults()).volumeUnit == .millilitres)
}

@MainActor
@Test func theUnitPersistsAcrossInstances() {
    let defaults = emptyDefaults()
    SettingsViewModel(defaults: defaults).volumeUnit = .ounces

    #expect(SettingsViewModel(defaults: defaults).volumeUnit == .ounces)
}

@MainActor
@Test func anUnknownStoredUnitFallsBackRatherThanCrashing() {
    let defaults = emptyDefaults()
    defaults.set("furlongs", forKey: "volumeUnit")

    #expect(SettingsViewModel(defaults: defaults).volumeUnit == .millilitres)
}

@MainActor
@Test func theFooterExplainsWhatTheToggleAffects() {
    let model = SettingsViewModel(defaults: emptyDefaults())
    #expect(model.unitFooter == "Changes how volumes are shown. Nothing already logged is altered.")
}

@MainActor
@Test func settingTheFeedBreakMinutesStoresItForTheExtensionToRead() {
    let defaults = emptyDefaults()
    let model = SettingsViewModel(defaults: defaults)

    model.feedBreakMinutes = 45

    #expect(defaults.integer(forKey: TrackingPreferences.feedKey) == 45)
}

@MainActor
@Test func aHomeOrderIsWrittenThroughAndReadBack() {
    let d = emptyDefaults()
    let model = SettingsViewModel(defaults: d)
    let baby = UUID()
    #expect(model.homeOrder(for: baby) == HomeActionKind.defaultOrder)
    model.setHomeOrder([.bottle, .breast, .sleep, .diaper, .solids, .meds, .growth], for: baby)
    #expect(model.homeOrder(for: baby).first == .bottle)
    // Another model on the same defaults — the widget's view of it.
    #expect(TrackingPreferences(defaults: d).homeOrder(for: baby).first == .bottle)
    model.resetHomeOrder(for: baby)
    #expect(model.homeOrder(for: baby) == HomeActionKind.defaultOrder)
}

@MainActor
@Test func movingAnActionByOneStepSwapsItWithItsNeighbour() {
    let model = SettingsViewModel(defaults: emptyDefaults())
    let baby = UUID()
    model.moveHomeAction(at: 1, by: -1, for: baby)
    #expect(model.homeOrder(for: baby).prefix(2) == [.bottle, .breast])
    // Off either end: nothing moves.
    model.moveHomeAction(at: 0, by: -1, for: baby)
    model.moveHomeAction(at: HomeActionKind.allCases.count - 1, by: 1, for: baby)
    #expect(model.homeOrder(for: baby).prefix(2) == [.bottle, .breast])
}

@MainActor
@Test func theListsHeadingsSitAtFixedPositions() {
    #expect(HomeButtonsCopy.heading(at: 0) == HomeButtonsCopy.bigButton)
    #expect(HomeButtonsCopy.heading(at: 1) == HomeButtonsCopy.tiles)
    #expect(HomeButtonsCopy.heading(at: 2) == nil)
    #expect(HomeButtonsCopy.heading(at: 4) == HomeButtonsCopy.inMore)
    #expect(HomeButtonsCopy.heading(at: 5) == nil)
}
