import Testing
import Foundation
import BabyCore
import BabyStore
@testable import InsightsFeature

/// Noon local on 15 June 2026; see HistoryFeatureTests for why not an epoch.
@MainActor private func noonLocal() -> Date {
    Calendar.current.date(from: DateComponents(year: 2026, month: 6, day: 15, hour: 12))!
}

@MainActor private func fixture() throws -> (BabyStore, UUID, Date) {
    let now = noonLocal()
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(now))
    store.breakLimits = { .standard }
    let baby = try store.addBaby(name: "Mira", birthDate: now.addingTimeInterval(-60 * 86_400), referenceSex: .girls, expectedDueDate: nil)
    return (store, baby.id, now)
}

@MainActor @Suite struct InsightsViewModelTests {
    @Test func joinedFeedCountsOnceWithEveryBottleCounted() throws {
        let (store, baby, now) = try fixture()
        store.breakLimits = { BreakLimits(feed: 600, sleep: 900) }
        let day = now.addingTimeInterval(-86_400)
        _ = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: day)
        _ = try store.logBottle(volumeML: 40, contents: .formula, for: baby, at: day.addingTimeInterval(300))   // joins
        let model = InsightsViewModel(store: store, babyID: baby)
        try model.load()
        #expect(model.summary.feeding.perDay?.value == 1)
        #expect(model.summary.feeding.bottleOnly == 1)
        #expect(model.summary.feeding.bottlePerDayML == 100)
    }

    @Test func breastSidesComeFromComponents() throws {
        let (store, baby, now) = try fixture()
        let start = now.addingTimeInterval(-86_400)
        let feed = FeedEntry(babyID: baby, kind: .breast, startedAt: start, timeZoneID: TimeZone.current.identifier)
        feed.endedAt = start.addingTimeInterval(900)
        feed.components = [.breast(id: UUID(), side: .left, seconds: 600), .breast(id: UUID(), side: .right, seconds: 300)]
        store.context.insert(feed)
        try store.context.save()
        let model = InsightsViewModel(store: store, babyID: baby)
        try model.load()
        #expect((model.summary.feeding.leftSeconds, model.summary.feeding.rightSeconds) == (600, 300))
        #expect(model.summary.feeding.breastOnly == 1)
    }

    @Test func sleepKindAndPartsComeFromTheSessionMembers() throws {
        let (store, baby, now) = try fixture()
        store.breakLimits = { BreakLimits(feed: 1_800, sleep: 1_200) }
        let yesterday = Calendar.current.startOfDay(for: now).addingTimeInterval(-86_400)
        _ = try store.logSleep(from: yesterday.addingTimeInterval(20 * 3600), to: yesterday.addingTimeInterval(22 * 3600), for: baby)
        _ = try store.logSleep(from: yesterday.addingTimeInterval(22.25 * 3600), to: yesterday.addingTimeInterval(23.25 * 3600), for: baby) // joins
        let model = InsightsViewModel(store: store, babyID: baby)
        try model.load()
        // `3.0 * 3600`, not `3 * 3600`: Swift Testing's #expect macro misreads
        // an `Optional<Double> == <Int-literal-expression>` comparison as
        // false even though both sides print as equal (10800.0 vs 10800) —
        // typing the literal as a Double sidesteps the macro, not the value.
        #expect(model.summary.sleep.perDay?.value == 3.0 * 3600)
        #expect(model.summary.sleep.nightPerDay == 3.0 * 3600)
        #expect(model.summary.sleep.longestStretch == 2 * 3600)
    }

    @Test func openSleepIsLeftOut() throws {
        let (store, baby, now) = try fixture()
        _ = try store.logSleep(from: now.addingTimeInterval(-86_400), to: nil, for: baby)
        let model = InsightsViewModel(store: store, babyID: baby)
        try model.load()
        #expect(model.summary.sleep.perDay == nil)
    }

    @Test func rangeChangeIsReadOnTheNextLoad() throws {
        let (store, baby, now) = try fixture()
        _ = try store.logDiaper(.wet, for: baby, at: now.addingTimeInterval(-20 * 86_400))
        let model = InsightsViewModel(store: store, babyID: baby)
        try model.load()
        #expect(model.summary.diapers.perDay == nil)
        model.range = .month
        try model.load()
        #expect(model.summary.range == .month)
        #expect(model.summary.diapers.perDay?.value == 1)
        model.range = .all
        try model.load()
        #expect(model.summary.diapers.bars.count == 3)
    }

    @Test func failedLoadClearsTheSummary() throws {
        let (store, baby, now) = try fixture()
        _ = try store.logDiaper(.wet, for: baby, at: now.addingTimeInterval(-86_400))
        let model = InsightsViewModel(store: store, babyID: baby)
        try model.load()
        #expect(model.summary.diapers.perDay != nil)
        try store.setActiveRealm(.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID()))
        #expect(throws: (any Error).self) { try model.load() }
        #expect(model.summary == .empty)
    }
}
