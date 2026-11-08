import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

/// Where History's rows lead (`LogSheet.editEntry` / `.session`): every entry
/// opens the sheet it was logged with, in edit mode, and solids never open
/// the session sheet, which has nothing to show for them.
@Suite @MainActor struct LogSheetRoutingTests {
    @Test func theSheetIdentityNamesTheKindAndTheEntry() {
        let id = UUID()
        #expect(LogSheet.editEntry(.diaper(id)).id == "edit-diaper-\(id)")
        #expect(LogSheet.editEntry(.feed(id)).id == "edit-feed-\(id)")
        #expect(LogSheet.editEntry(.measurement(id)).id == "edit-measurement-\(id)")
        #expect(LogSheet.immunization.id == "immunization")
        #expect(LogSheet.editEntry(.immunization(id)).id == "edit-immunization-\(id)")
        #expect(LogSheet.session(.sleep, rootID: id).id == "session-sleep-\(id)")
        #expect(LogSheet.session(.feed, rootID: id).id == "session-feed-\(id)")
    }

    @Test func diaperMedAndGrowthOpenTheirOwnSheets() throws {
        let (store, baby, clock) = try fixture()
        let diaper = try store.logDiaper(.wet, for: baby)
        let med = try store.logSupplement(name: "Vitamin D", for: baby)
        let check = try store.logMeasurement(for: baby, at: clock.now, timeZoneID: "UTC",
                                             values: MeasurementValues(weightKG: 4.2))

        #expect(EditDestination.resolve(.diaper(diaper.id), store: store, babyID: baby) == .diaper(diaper.id))
        #expect(EditDestination.resolve(.supplement(med.id), store: store, babyID: baby) == .supplement(med.id))
        #expect(EditDestination.resolve(.measurement(check.id), store: store, babyID: baby) == .measurement(check.id))
    }

    @Test func aShotResolvesToItsPanelAndAWrongKindToGone() throws {
        let (store, baby, _) = try fixture()
        let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby)
        let med = try store.logSupplement(name: "Iron", for: baby)
        #expect(EditDestination.resolve(.immunization(shot.id), store: store, babyID: baby) == .immunization(shot.id))
        #expect(EditDestination.resolve(.immunization(med.id), store: store, babyID: baby) == .gone)
        #expect(EditDestination.resolve(.supplement(shot.id), store: store, babyID: baby) == .gone)
        try store.softDelete(shot)
        #expect(EditDestination.resolve(.immunization(shot.id), store: store, babyID: baby) == .gone)
    }

    @Test func aSolidsFeedOpensTheSolidsSheetNeverTheSession() throws {
        let (store, baby, _) = try fixture()
        let solids = try store.logSolids(foods: ["Pear"], amount: .some, for: baby)

        #expect(EditDestination.resolve(.feed(solids.id), store: store, babyID: baby) == .solids(feedID: solids.id))
    }

    @Test func aLoneBreastPartOpensThePartEditorAndABottleTheBottleSheet() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let breast = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(600), for: baby)
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        let breastComponent = try #require(breast.components.first?.id)
        let bottleComponent = try #require(bottle.components.first?.id)

        #expect(EditDestination.resolve(.feed(breast.id), store: store, babyID: baby)
                == .part(.breast(feedID: breast.id, componentID: breastComponent)))
        #expect(EditDestination.resolve(.feed(bottle.id), store: store, babyID: baby)
                == .bottle(feedID: bottle.id, componentID: bottleComponent))
    }

    @Test func aSleepOpensThePartEditor() throws {
        let (store, baby, clock) = try fixture()
        let sleep = try store.logSleep(from: clock.now.addingTimeInterval(-7200),
                                       to: clock.now.addingTimeInterval(-3600), for: baby)

        #expect(EditDestination.resolve(.sleep(sleep.id), store: store, babyID: baby) == .part(.sleep(sleepID: sleep.id)))
    }

    /// An older phone's one-entry combo (L, R and a bottle in one feed) has
    /// several parts: only the session sheet lists them all.
    @Test func anOldCombinedFeedOpensItsSession() throws {
        let (store, baby, _) = try fixture()
        let feed = try store.logBottle(volumeML: 1, contents: .formula, for: baby)
        feed.components = [
            .breast(id: UUID(), side: .left, seconds: 300),
            .bottle(id: UUID(), volumeML: 60, contents: .formula),
        ]
        try store.update(feed)

        #expect(EditDestination.resolve(.feed(feed.id), store: store, babyID: baby)
                == .session(.feed, rootID: feed.effectiveFeedingSessionID))
    }

    @Test func anEntryGoneSinceTheTapIsGone() throws {
        let (store, baby, _) = try fixture()
        let diaper = try store.logDiaper(.wet, for: baby)
        try store.softDelete(diaper)

        #expect(EditDestination.resolve(.diaper(diaper.id), store: store, babyID: baby) == .gone)
        #expect(EditDestination.resolve(.feed(UUID()), store: store, babyID: baby) == .gone)
        #expect(EditDestination.resolve(.sleep(UUID()), store: store, babyID: baby) == .gone)
        #expect(EditDestination.resolve(.measurement(UUID()), store: store, babyID: baby) == .gone)
    }

    /// A ref of the wrong kind (a diaper id passed as a med) is not opened
    /// in a sheet that would show it as empty.
    @Test func aRefOfTheWrongKindIsGone() throws {
        let (store, baby, _) = try fixture()
        let diaper = try store.logDiaper(.wet, for: baby)

        #expect(EditDestination.resolve(.supplement(diaper.id), store: store, babyID: baby) == .gone)
        #expect(EditDestination.resolve(.feed(diaper.id), store: store, babyID: baby) == .gone)
    }

    @Test func aPumpOpensThePumpPanelForEveryBabyAndADeletedOneIsGone() throws {
        let (store, baby, clock) = try fixture()
        let theo = try store.addBaby(name: "Theo", birthDate: .now, referenceSex: .boys, expectedDueDate: nil).id
        let pump = try store.logPump(side: .both, from: clock.now.addingTimeInterval(-900), to: clock.now, volumeML: nil, note: nil)
        #expect(LogSheet.pump.id == "pump")
        #expect(LogSheet.pumpAmount(pumpID: pump.id).id == "pumpAmount-\(pump.id)")
        #expect(LogSheet.editEntry(.pump(pump.id)).id == "edit-pump-\(pump.id)")
        #expect(EditDestination.resolve(.pump(pump.id), store: store, babyID: baby) == .pump(pump.id))
        #expect(EditDestination.resolve(.pump(pump.id), store: store, babyID: theo) == .pump(pump.id))
        #expect(EditDestination.resolve(.sleep(pump.id), store: store, babyID: baby) == .gone)
        #expect(!LogSheet.editEntry(.pump(pump.id)).opensSessionList(store: store, babyID: baby))
        try store.softDelete(pump)
        #expect(EditDestination.resolve(.pump(pump.id), store: store, babyID: baby) == .gone)
    }
}
