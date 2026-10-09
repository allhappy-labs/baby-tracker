import Testing
import Foundation
import BabyCore
import BabyStore
@testable import LoggingFeature

@Suite @MainActor struct SupplementSheetTests {
    func setup() throws -> (BabyStore, UUID, TestDateProvider) {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
        let store = try BabyStore.inMemory(dateProvider: clock)
        return (store, try store.addBaby(name: "Mira", birthDate: clock.now, referenceSex: .girls, expectedDueDate: nil).id, clock)
    }
    @Test func emptyInvitesNameAndDoesNotLogOnOpening() throws {
        let (store, id, _) = try setup()
        let model = SupplementSheetModel(store: store, babyID: id)
        #expect(model.isAddingName)
        #expect(!model.showsPicker)
        #expect(!model.canLog)
        #expect(try store.lastSupplement(for: id) == nil)
        #expect(SupplementSheetCopy.addName == "Add med")
        #expect(LogSheet.supplement.id == "supplement")
    }
    @Test func oneNameIsPlainAndPlusStartsNewName() throws {
        let (store, id, _) = try setup()
        try store.logSupplement(name: "Vitamin D", for: id)
        let model = SupplementSheetModel(store: store, babyID: id)
        #expect(model.selectedName == "Vitamin D")
        #expect(!model.showsPicker)
        #expect(model.canLog)
        model.addName()
        #expect(model.isAddingName)
        #expect(!model.canLog)
    }
    @Test func multipleSelectsMostRecentActionAndCanonicalSpelling() throws {
        let (store, id, clock) = try setup()
        try store.logSupplement(name: "Vitamin D", for: id)
        clock.advance(by: 10)
        try store.logSupplement(name: "Iron", for: id)
        clock.advance(by: 10)
        try store.logSupplement(name: "vitamin d", for: id, at: clock.now.addingTimeInterval(-86400))
        let model = SupplementSheetModel(store: store, babyID: id)
        #expect(model.showsPicker)
        #expect(model.selectedName == "Vitamin D")
        model.selectedName = "Iron"
        model.when = clock.now.addingTimeInterval(-172800)
        clock.advance(by: 10)
        #expect(model.log())
        #expect(try store.lastSupplement(for: id)?.name == "Iron")
        #expect(try store.lastSupplement(for: id)?.startedAt == model.when)
    }
    @Test func validatesAndNormalizesFreeText() throws {
        let (store, id, _) = try setup()
        let model = SupplementSheetModel(store: store, babyID: id)
        for invalid in ["  ", "a\nb", String(repeating: "a", count: 65)] {
            model.newName = invalid
            #expect(!model.canLog)
        }
        model.newName = "  Vitamin D  "
        #expect(model.canLog)
        #expect(model.log())
        #expect(try store.supplementNames(for: id) == ["Vitamin D"])
    }
    @Test func failedSaveRetainsInputAndDoesNotDismissOrDuplicate() throws {
        let (store, id, _) = try setup()
        let model = SupplementSheetModel(store: store, babyID: id)
        model.newName = "Iron"
        try store.setActiveRealm(.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID()))
        #expect(!model.log())
        #expect(model.errorMessage != nil)
        #expect(model.newName == "Iron")
        #expect(try store.lastSupplement(for: id) == nil)
    }
}
