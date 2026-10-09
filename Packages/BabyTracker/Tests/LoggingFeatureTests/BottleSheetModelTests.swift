import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

@MainActor @Suite struct BottleSheetModelTests {
    @Test func aChipLogsAFinishedBottleThatJoinsTheOpenFeed() throws {
        let (store, baby, clock) = try fixture()
        let first = try #require(try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300)))
        let model = BottleSheetModel(store: store, babyID: baby, unit: .millilitres, mode: .start)
        model.contents = .formula
        try model.logChip(90)
        let last = try #require(try store.lastFeed(for: baby))
        #expect(last.totalBottleVolumeML == 90)
        #expect(last.feedingSessionID == first.id)
    }

    @Test func doneAfterATimedBottleSavesTheChosenAmount() async throws {
        let (store, baby, clock) = try fixture()
        let timer = try store.startPart(.bottle(.breastMilk), for: baby)
        clock.advance(by: 400)
        let model = BottleSheetModel(store: store, babyID: baby, unit: .millilitres, mode: .stopped(timerID: timer.id))
        model.volumeML = 80
        model.step(1)
        try await model.done()
        #expect(try store.lastFeed(for: baby)?.totalBottleVolumeML == 90)
        #expect(try store.activeTimers(for: baby).isEmpty)
    }

    @Test func ounceChipsAreWholeOunces() throws {
        let (store, baby, _) = try fixture()
        let model = BottleSheetModel(store: store, babyID: baby, unit: .ounces, mode: .start)
        #expect(model.chips.count == 4)
    }

    /// Setting the exact amount on a feed already logged does not touch any
    /// timer — there is none left to touch.
    @Test func amountModeSetsTheVolumeOnAnAlreadyLoggedBottle() async throws {
        let (store, baby, _) = try fixture()
        let feed = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        let model = BottleSheetModel(store: store, babyID: baby, unit: .millilitres, mode: .amount(feedID: feed.id))
        model.volumeML = 120
        try await model.done()
        #expect(try store.lastFeed(for: baby)?.totalBottleVolumeML == 120)
    }

    /// The time row sits with the amounts and says so: it is when the logged
    /// bottle was drunk. Start bottle always starts now.
    @Test func theTimeRowTimesTheLoggedAmountAndSaysSo() throws {
        let (store, baby, clock) = try fixture()
        let model = BottleSheetModel(store: store, babyID: baby, unit: .millilitres, mode: .start)
        model.when = clock.now.addingTimeInterval(-600)
        try model.logChip(60)
        #expect(try store.lastFeed(for: baby)?.startedAt == clock.now.addingTimeInterval(-600))
        #expect(BottleSheetCopy.drunkAt == "Drunk at")
    }

    /// The amounts are bare numbers, so the line above them names the unit.
    @Test func theAmountsCaptionNamesTheUnit() throws {
        let (store, baby, _) = try fixture()
        #expect(BottleSheetModel(store: store, babyID: baby, unit: .millilitres, mode: .start).amountsCaption
            == "or log what was drunk, in ml")
        #expect(BottleSheetModel(store: store, babyID: baby, unit: .ounces, mode: .start).amountsCaption
            == "or log what was drunk, in oz")
    }

    @Test func stepNeverGoesNegative() throws {
        let (store, baby, _) = try fixture()
        let model = BottleSheetModel(store: store, babyID: baby, unit: .millilitres, mode: .start)
        model.volumeML = 5
        model.step(-1)
        #expect(model.volumeML == 0)
        #expect(!model.canDone)
    }
}
