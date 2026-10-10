import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

@Suite @MainActor struct SolidsEditTests {
    @Test func opensWithTheEntrysFoodsAmountTimeAndNote() throws {
        let (store, baby, clock) = try fixture()
        let at = clock.now.addingTimeInterval(-3600)
        let feed = try store.logSolids(foods: ["Pear", "oat"], amount: .taste, for: baby, at: at)
        feed.note = "loved it"
        try store.update(feed)

        let model = SolidsEditModel(store: store, babyID: baby, feedID: feed.id)

        #expect(model.foodsText == "Pear, oat")
        #expect(model.amount == .taste)
        #expect(model.when == at)
        #expect(model.note == "loved it")
        #expect(!model.isGone)
        #expect(model.canSave)
    }

    @Test func changeFoodsAndAmountKeepingTheComponentID() throws {
        let (store, baby, clock) = try fixture()
        let at = clock.now.addingTimeInterval(-3600)
        let feed = try store.logSolids(foods: ["Pear"], amount: .some, for: baby, at: at)
        let componentID = try #require(feed.components.first?.id)
        let model = SolidsEditModel(store: store, babyID: baby, feedID: feed.id)
        let logged = try store.changeLog().count
        let moved = at.addingTimeInterval(-600)

        model.foodsText = "banana, , rice "
        model.amount = .lots
        model.when = moved
        try model.save()

        #expect(feed.components.count == 1)
        #expect(feed.components.first?.id == componentID)
        #expect(feed.components.first?.solidFoods == ["banana", "rice"])
        #expect(feed.components.first?.solidsAmount == .lots)
        #expect(feed.startedAt == moved)
        #expect(feed.endedAt == moved)
        let log = try store.changeLog()
        #expect(log.count == logged + 1)
        #expect(log.last?.operation == .update)
        #expect(log.last?.entityID == feed.id)
    }

    @Test func noFoodsCannotBeSaved() throws {
        let (store, baby, _) = try fixture()
        let feed = try store.logSolids(foods: ["Pear"], amount: .some, for: baby)
        let model = SolidsEditModel(store: store, babyID: baby, feedID: feed.id)

        model.foodsText = " , "

        #expect(!model.canSave)
        #expect(throws: EditModeError.invalid) { try model.save() }
        #expect(feed.components.first?.solidFoods == ["Pear"])
    }

    @Test func deleteTombstonesTheEntry() throws {
        let (store, baby, _) = try fixture()
        let feed = try store.logSolids(foods: ["Pear"], amount: .some, for: baby)
        let model = SolidsEditModel(store: store, babyID: baby, feedID: feed.id)

        try model.delete()

        #expect(feed.isTombstoned)
        #expect(try store.entry(id: feed.id, for: baby) == nil)
    }

    @Test func becomesGoneWhenTheEntryIsDeletedElsewhere() throws {
        let (store, baby, _) = try fixture()
        let feed = try store.logSolids(foods: ["Pear"], amount: .some, for: baby)
        let model = SolidsEditModel(store: store, babyID: baby, feedID: feed.id)

        try store.deleteFeedPart(feed.id, for: baby)
        model.reload()

        #expect(model.isGone)
        #expect(!model.canSave)
        #expect(throws: EditModeError.gone) { try model.save() }
    }

    @Test func aFeedWithoutSolidsOpensGone() throws {
        let (store, baby, clock) = try fixture()
        let breast = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-600), to: clock.now, for: baby)
        #expect(SolidsEditModel(store: store, babyID: baby, feedID: breast.id).isGone)
    }

    @Test func aRefusedSaveLeavesTheFeedAsItWas() throws {
        let (store, baby, clock) = try fixture()
        let family = RealmID.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID())
        try store.setActiveRealm(family)
        let at = clock.now.addingTimeInterval(-3600)
        let feed = try store.logSolids(foods: ["Pear"], amount: .some, for: baby, at: at)
        let components = feed.components
        let modifiedAt = feed.lastModifiedAt
        let model = SolidsEditModel(store: store, babyID: baby, feedID: feed.id)
        try store.setWritePermission(.viewer, for: family)

        model.foodsText = "banana"
        model.amount = .lots
        model.when = at.addingTimeInterval(-600)
        model.note = "new"
        #expect(throws: WritePermissionError.readOnlyCaregiver) { try model.save() }

        #expect(feed.components == components)
        #expect(feed.startedAt == at)
        #expect(feed.endedAt == at)
        #expect(feed.note == nil)
        #expect(feed.lastModifiedAt == modifiedAt)
        #expect(model.foodsText == "banana")
    }

    @Test func anEntryOfAnotherRealmIsGone() throws {
        let (store, baby, _) = try fixture()
        let feed = try store.logSolids(foods: ["Pear"], amount: .some, for: baby)
        let model = SolidsEditModel(store: store, babyID: baby, feedID: feed.id)

        try store.setActiveRealm(.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID()))
        model.reload()

        #expect(model.isGone)
    }

    @Test func newSolidsKeepTheChosenDay() throws {
        let (store, baby, clock) = try fixture()
        let yesterday = clock.now.addingTimeInterval(-26 * 3600)
        let model = SolidsLogModel(store: store, babyID: baby)
        model.foodsText = "pear"
        model.when = yesterday
        try model.log()
        #expect(try store.lastFeed(for: baby)?.startedAt == yesterday)
    }
}
