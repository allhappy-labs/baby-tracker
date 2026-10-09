import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

@MainActor @Suite struct PastEntryModelTests {
    @Test func reversedFutureAndTooShortAreRefused() throws {
        let (store, baby, clock) = try fixture()
        let model = PastEntryModel(store: store, babyID: baby)
        model.kind = .left
        model.start = clock.now
        model.end = clock.now.addingTimeInterval(-60)
        #expect(model.validationMessage != nil)
        model.end = clock.now.addingTimeInterval(60)
        #expect(model.validationMessage != nil)   // in the future
        model.start = clock.now.addingTimeInterval(-20)
        model.end = clock.now
        #expect(model.validationMessage != nil)   // 20 s breast
        model.start = clock.now.addingTimeInterval(-600)
        #expect(model.validationMessage == nil)
        try model.save()
        #expect(try store.lastFeed(for: baby)?.components.first?.breastSide == .left)
    }

    @Test func bottleAndSleepLogAtTheirOwnKind() throws {
        let (store, baby, clock) = try fixture()
        let model = PastEntryModel(store: store, babyID: baby)
        model.kind = .bottle
        model.start = clock.now.addingTimeInterval(-300)
        model.volumeML = 90
        model.contents = .formula
        #expect(model.validationMessage == nil)
        try model.save()
        #expect(try store.lastFeed(for: baby)?.totalBottleVolumeML == 90)

        model.kind = .sleep
        model.start = clock.now.addingTimeInterval(-1200)
        model.end = clock.now.addingTimeInterval(-600)
        #expect(model.validationMessage == nil)
        try model.save()
        #expect(try store.lastSleep(for: baby)?.startedAt == model.start)
        #expect(try store.lastSleep(for: baby)?.endedAt == model.end)
    }
}
