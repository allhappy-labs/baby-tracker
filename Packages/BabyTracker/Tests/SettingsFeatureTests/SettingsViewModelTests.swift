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
