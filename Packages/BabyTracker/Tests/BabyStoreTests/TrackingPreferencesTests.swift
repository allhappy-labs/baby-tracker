import Testing
import Foundation
import BabyCore
@testable import BabyStore

@Suite struct TrackingPreferencesTests {
    private func defaults() -> UserDefaults {
        let name = "tracking-\(UUID().uuidString)"
        let d = UserDefaults(suiteName: name)!
        d.removePersistentDomain(forName: name)
        return d
    }

    @Test func defaultsAreThirtyAndFifteenMinutes() {
        let prefs = TrackingPreferences(defaults: defaults())
        #expect(prefs.limits == BreakLimits(feed: 1800, sleep: 900))
    }

    @Test func limitsAreClamped() {
        let prefs = TrackingPreferences(defaults: defaults())
        prefs.feedMinutes = 2
        prefs.sleepMinutes = 500
        #expect(prefs.feedMinutes == 5)
        #expect(prefs.sleepMinutes == 120)
        prefs.feedMinutes = 22
        #expect(prefs.feedMinutes == 20)
    }

    @MainActor @Test func changingTheLimitAppliesToTheNextDecision() throws {
        let d = defaults()
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        store.breakLimits = { TrackingPreferences(defaults: d).limits }
        TrackingPreferences(defaults: d).feedMinutes = 10
        #expect(store.breakLimits().feed == 600)
    }

    @Test func withNothingStoredTheOrderIsTheDefault() {
        #expect(TrackingPreferences(defaults: defaults()).homeOrder(for: UUID()) == HomeActionKind.defaultOrder)
    }

    @Test func anOrderIsKeptPerBaby() {
        let prefs = TrackingPreferences(defaults: defaults())
        let mira = UUID(), tomas = UUID()
        prefs.setHomeOrder([.bottle, .breast, .sleep, .diaper, .solids, .meds, .growth], for: mira)
        #expect(prefs.homeOrder(for: mira).first == .bottle)
        #expect(prefs.homeOrder(for: tomas) == HomeActionKind.defaultOrder)
    }

    @Test func aStoredOrderWithUnknownKindsReadsComplete() {
        let d = defaults()
        let baby = UUID()
        d.set(["solids", "bath", "solids"], forKey: TrackingPreferences.homeOrderKey(for: baby))
        #expect(TrackingPreferences(defaults: d).homeOrder(for: baby)
                == [.solids, .breast, .bottle, .sleep, .diaper, .meds, .growth, .vaccine, .pump])
    }

    @Test func resetRemovesTheStoredOrder() {
        let d = defaults()
        let prefs = TrackingPreferences(defaults: d)
        let baby = UUID()
        prefs.setHomeOrder([.solids, .breast, .bottle, .sleep, .diaper, .meds, .growth], for: baby)
        prefs.resetHomeOrder(for: baby)
        #expect(d.object(forKey: TrackingPreferences.homeOrderKey(for: baby)) == nil)
        #expect(prefs.homeOrder(for: baby) == HomeActionKind.defaultOrder)
    }

    /// The default is "nothing stored", so a kind a later version adds to the
    /// default order is not pinned to the end for a baby left at the default.
    @Test func storingTheDefaultOrderStoresNothing() {
        let d = defaults()
        let baby = UUID()
        TrackingPreferences(defaults: d).setHomeOrder(HomeActionKind.defaultOrder, for: baby)
        #expect(d.object(forKey: TrackingPreferences.homeOrderKey(for: baby)) == nil)
    }
}
