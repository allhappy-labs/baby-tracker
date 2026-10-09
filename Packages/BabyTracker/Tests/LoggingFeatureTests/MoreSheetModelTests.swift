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
}
