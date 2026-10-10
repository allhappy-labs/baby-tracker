import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

@MainActor @Suite struct NewPartModelTests {
    @Test func reversedFutureAndTooShortAreRefused() throws {
        let (store, baby, clock) = try fixture()
        let model = NewPartModel(store: store, babyID: baby)
        model.kind = .breast
        model.side = .left
        model.start = clock.now
        model.end = clock.now.addingTimeInterval(-60)
        #expect(model.validationMessage == TimeSpanCopy.endBeforeStart)
        model.end = clock.now.addingTimeInterval(60)
        #expect(model.validationMessage == TimeSpanCopy.inTheFuture)
        model.start = clock.now.addingTimeInterval(-20)
        model.end = clock.now
        #expect(model.validationMessage == TimeSpanCopy.breastTooShort)
        model.start = clock.now.addingTimeInterval(-600)
        #expect(model.validationMessage == nil)
        try model.save()
        #expect(try store.lastFeed(for: baby)?.components.first?.breastSide == .left)
    }

    @Test func bottleAndSleepLogAtTheirOwnKind() throws {
        let (store, baby, clock) = try fixture()
        let model = NewPartModel(store: store, babyID: baby)
        model.kind = .bottle
        model.start = clock.now.addingTimeInterval(-300)
        model.end = nil
        model.volumeML = 90
        model.contents = .formula
        #expect(model.validationMessage == nil)
        try model.save()
        #expect(try store.lastFeed(for: baby)?.totalBottleVolumeML == 90)
        #expect(try store.lastFeed(for: baby)?.components.first?.bottleSeconds == nil)

        model.kind = .sleep
        model.start = clock.now.addingTimeInterval(-1200)
        model.end = clock.now.addingTimeInterval(-600)
        model.sleepKind = .nap
        #expect(model.validationMessage == nil)
        try model.save()
        #expect(try store.lastSleep(for: baby)?.endedAt == model.end)
        #expect(try store.lastSleep(for: baby)?.kindOverride == .nap)
    }

    @Test func aTimedBottleAndANoteAreKept() throws {
        let (store, baby, clock) = try fixture()
        let model = NewPartModel(store: store, babyID: baby)
        model.kind = .bottle
        model.start = clock.now.addingTimeInterval(-900)
        model.end = clock.now
        model.note = "  slow  "
        try model.save()
        let feed = try #require(try store.lastFeed(for: baby))
        #expect(feed.components.first?.bottleSeconds == 900)
        #expect(feed.note == "slow")
    }

    @Test func switchingToSleepNeedsAnEnd() throws {
        let (store, baby, _) = try fixture()
        let model = NewPartModel(store: store, babyID: baby)
        model.kind = .bottle
        model.end = nil
        model.kind = .sleep
        #expect(model.end != nil)
    }

    /// Past entry opens with an End (ten minutes ago to now); switching to
    /// Bottle must clear it, since a bottle logged after the fact is a
    /// moment unless the user adds an end time (final review finding 1).
    @Test func switchingToBottleClearsTheEndItOpenedWith() throws {
        let (store, baby, _) = try fixture()
        let model = NewPartModel(store: store, babyID: baby)
        #expect(model.end != nil)
        model.kind = .bottle
        #expect(model.end == nil)
        model.volumeML = 90
        try model.save()
        let feed = try #require(try store.lastFeed(for: baby))
        #expect(feed.components.first?.bottleSeconds == nil)
        #expect(feed.components.first?.endedAt == nil)
    }

    /// Switching back to breast or sleep restores a non-nil End.
    @Test func switchingFromBottleToBreastRestoresAnEnd() throws {
        let (store, baby, _) = try fixture()
        let model = NewPartModel(store: store, babyID: baby)
        model.kind = .bottle
        #expect(model.end == nil)
        model.kind = .breast
        #expect(model.end != nil)
    }

    /// A bottle given an End by the user, after switching away and back to
    /// bottle, keeps the End the user set (the didSet only clears it on the
    /// transition *into* bottle, not every time the kind is touched).
    @Test func aBottleEndAddedByTheUserIsKeptOnSave() throws {
        let (store, baby, clock) = try fixture()
        let model = NewPartModel(store: store, babyID: baby)
        model.kind = .bottle
        #expect(model.end == nil)
        model.start = clock.now.addingTimeInterval(-300)
        model.end = clock.now
        model.volumeML = 90
        try model.save()
        let feed = try #require(try store.lastFeed(for: baby))
        #expect(feed.components.first?.bottleSeconds == 300)
    }
}
