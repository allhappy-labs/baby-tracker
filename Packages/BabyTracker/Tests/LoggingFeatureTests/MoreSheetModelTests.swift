import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

@MainActor @Suite struct MoreSheetModelTests {
    @Test func savedMedsAreChipsAndATapLogsNow() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.logSupplement(name: "Vitamin D", for: baby)
        clock.advance(by: 60)
        _ = try store.logSupplement(name: "Iron", for: baby)
        let model = MoreSheetModel(store: store, babyID: baby)
        #expect(model.meds == ["Iron", "Vitamin D"])
        try model.logMed("Vitamin D")
        #expect(try store.supplementNames(for: baby).first == "Vitamin D")
    }

    @Test func withNoMedsLoggedTheRowIsEmpty() throws {
        let (store, baby, _) = try fixture()
        let model = MoreSheetModel(store: store, babyID: baby)
        #expect(model.meds.isEmpty)
    }

    @Test func theDefaultOrderShowsSolidsGrowthAndTheMedChips() throws {
        let (store, baby, _) = try fixture()
        let model = MoreSheetModel(store: store, babyID: baby)
        #expect(model.tiles == [.solids, .growth, .vaccine, .pump])
        #expect(model.showsMeds)
    }

    @Test func whatTheDockHasNoRoomForIsATile() throws {
        let (store, baby, _) = try fixture()
        let model = MoreSheetModel(store: store, babyID: baby,
                                   order: [.solids, .meds, .growth, .diaper, .breast, .bottle, .sleep])
        #expect(model.tiles == [.breast, .bottle, .sleep, .vaccine, .pump])
        // Meds is on the dock, where its own panel carries the chips.
        #expect(!model.showsMeds)
    }

    @Test func duringASleepTheSheetKeepsFeedActions() throws {
        let (store, baby, _) = try fixture()
        _ = try store.startSleep(for: baby)
        let model = MoreSheetModel(store: store, babyID: baby,
                                   order: [.solids, .meds, .growth, .diaper, .breast, .bottle, .sleep])
        // Diaper lost its tile to the three-tile cap, so the sheet carries it.
        #expect(model.tiles == [.diaper, .breast, .bottle, .vaccine, .pump])
    }

    @Test func duringAFeedTheSheetLeavesOutEveryTimedAction() throws {
        let (store, baby, _) = try fixture()
        _ = try store.startPart(.breast(.left), for: baby)
        let model = MoreSheetModel(store: store, babyID: baby,
                                   order: [.solids, .meds, .growth, .diaper, .breast, .bottle, .sleep])
        #expect(model.tiles == [.diaper, .vaccine, .pump])
    }

    @Test func aBreastSideStartsFromTheSheet() async throws {
        let (store, baby, _) = try fixture()
        let model = MoreSheetModel(store: store, babyID: baby,
                                   order: [.solids, .meds, .growth, .diaper, .breast, .bottle, .sleep])
        try await model.startBreast(.right)
        #expect(try store.activeTimer(kind: .feed, for: baby)?.currentSide == .right)
        try await model.startSleep()
        #expect(try store.activeTimer(kind: .sleep, for: baby) != nil)
    }

    /// Meds docked fourth loses its tile while a sleep runs with Feed on the
    /// dock: its chips move into the sheet rather than vanish.
    @Test func medsCutFromARunningDockShowTheirChipsInTheSheet() throws {
        let (store, baby, _) = try fixture()
        _ = try store.startSleep(for: baby)
        let model = MoreSheetModel(store: store, babyID: baby,
                                   order: [.bottle, .diaper, .solids, .meds, .breast, .sleep, .growth])
        #expect(model.showsMeds)
        #expect(model.tiles == [.breast, .growth, .vaccine, .pump])
    }
}
