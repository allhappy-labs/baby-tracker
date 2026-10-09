import Testing
import Foundation
import BabyCore
import BabyStore
@testable import HistoryFeature

@MainActor
private func makeStore(bornDaysAgo: Double) throws -> (BabyStore, UUID, TestDateProvider) {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: clock.now.addingTimeInterval(-bornDaysAgo * 86_400),
                                 referenceSex: .girls, expectedDueDate: nil)
    return (store, baby.id, clock)
}

private func daysAgo(_ days: Double, from clock: TestDateProvider) -> Date {
    clock.now.addingTimeInterval(-days * 86_400)
}

/// One endless scroll (ruling H-R23): two weeks at a time, as far back as
/// the baby's first day.
@Suite @MainActor struct HistoryPagingTests {
    @Test func theFirstPageIsTwoWeeksAndTheNextBringsOlderDays() throws {
        let (store, baby, clock) = try makeStore(bornDaysAgo: 90)
        try store.logDiaper(.wet, for: baby)
        let older = try store.logDiaper(.dirty, for: baby, at: daysAgo(20, from: clock))
        let model = HistoryViewModel(store: store, babyID: baby)

        try model.reload()
        #expect(HistoryViewModel.pageSize == 14)
        #expect(model.dayCount == 14)
        #expect(model.days.count == 1)
        #expect(model.hasMore)

        try model.loadMore()
        #expect(model.dayCount == 28)
        #expect(model.days.count == 2)
        #expect(model.days.last?.rows.map(\.id) == [older.id])
        #expect(model.hasMore)
    }

    @Test func pagingStopsAtTheBirthDay() throws {
        let (store, baby, _) = try makeStore(bornDaysAgo: 20)
        try store.logDiaper(.wet, for: baby)
        let model = HistoryViewModel(store: store, babyID: baby)

        try model.reload()
        #expect(model.hasMore)
        try model.loadMore()
        #expect(model.dayCount == 28)
        #expect(!model.hasMore)

        try model.loadMore()
        #expect(model.dayCount == 28, "nothing older to page in")
    }

    /// A birth date set later than the first entries must not hide them.
    @Test func anEntryFromBeforeTheBirthDateIsStillReached() throws {
        let (store, baby, clock) = try makeStore(bornDaysAgo: 5)
        let early = try store.logDiaper(.wet, for: baby, at: daysAgo(40, from: clock))
        let model = HistoryViewModel(store: store, babyID: baby)

        try model.reload()
        while model.hasMore { try model.loadMore() }

        #expect(model.dayCount == 42)
        #expect(model.days.flatMap(\.rows).map(\.id) == [early.id])
    }

    /// A write anywhere reloads the list; the pages already scrolled in stay.
    @Test func aReloadKeepsThePagesAlreadyLoaded() throws {
        let (store, baby, clock) = try makeStore(bornDaysAgo: 90)
        let older = try store.logDiaper(.dirty, for: baby, at: daysAgo(20, from: clock))
        let model = HistoryViewModel(store: store, babyID: baby)
        try model.reload()
        try model.loadMore()

        try store.logDiaper(.wet, for: baby)
        try model.reload()

        #expect(model.dayCount == 28)
        #expect(model.days.flatMap(\.rows).contains { $0.id == older.id })
        #expect(model.days.count == 2)
    }
}
