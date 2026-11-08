import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

@MainActor @Suite struct LogLinkPanelTests {
    @Test func aLinkOpensItsActionsPanelForALivingBaby() throws {
        let (store, baby, _) = try fixture()
        let babies = try store.babies().map(\.id)
        func panel(_ kind: HomeActionKind) -> String? {
            LogLink(kind: kind, babyID: baby).panel(babies: babies, canLog: true, isBusy: false)?.id
        }
        #expect(panel(.solids) == LogSheet.solids.id)
        #expect(panel(.meds) == LogSheet.meds.id)
        #expect(panel(.growth) == LogSheet.measurement.id)
        #expect(panel(.vaccine) == LogSheet.immunization.id)
        #expect(panel(.pump) == LogSheet.pump.id)
        #expect(panel(.bottle) == LogSheet.bottle.id)
        #expect(panel(.diaper) == LogSheet.diaper.id)
        // Both log from the widget itself; a link for them only opens Home.
        #expect(panel(.breast) == nil)
        #expect(panel(.sleep) == nil)
    }

    /// A widget can go on drawing a baby deleted since: its link must not
    /// open a panel that would then log for another baby.
    @Test func aDeletedBabysLinkOpensNothing() throws {
        let (store, baby, _) = try fixture()
        _ = try store.addBaby(name: "Tomas", birthDate: .now, referenceSex: .boys, expectedDueDate: nil)
        let mira = try #require(try store.baby(baby))
        try store.deleteBaby(mira)
        // The row is still there, tombstoned; `babies()` is who is alive.
        let babies = try store.babies().map(\.id)
        #expect(LogLink(kind: .solids, babyID: baby).panel(babies: babies, canLog: true, isBusy: false) == nil)
    }

    @Test func aLinkNeverReplacesWhatIsOnScreenOrOpensForAViewer() throws {
        let (store, baby, _) = try fixture()
        let babies = try store.babies().map(\.id)
        let link = LogLink(kind: .solids, babyID: baby)
        #expect(link.panel(babies: babies, canLog: true, isBusy: true) == nil)
        #expect(link.panel(babies: babies, canLog: false, isBusy: false) == nil)
    }
}
