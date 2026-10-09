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
}
