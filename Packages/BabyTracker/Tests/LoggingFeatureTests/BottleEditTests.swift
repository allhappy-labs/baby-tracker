import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

@Suite @MainActor struct BottleEditTests {
    @Test func opensWithTheComponentsVolumeContentsAndTime() throws {
        let (store, baby, clock) = try fixture()
        let at = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: at)
        let componentID = try #require(feed.components.first?.id)

        let model = PartEditModel(store: store, babyID: baby, target: .bottle(feedID: feed.id, componentID: componentID),
                                  unit: .millilitres)

        #expect(model.volumeML == 90)
        #expect(model.contents == .formula)
        #expect(model.start == at)
        #expect(model.chips == [30, 60, 90, 120])
        #expect(model.canSave)
    }

    @Test func saveChangesOnlyThatComponent() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let breast = try #require(feed.components.first)
        let bottleID = UUID()
        let bottleAt = start.addingTimeInterval(900)
        feed.components.append(.bottle(id: bottleID, volumeML: 60, contents: .formula, startedAt: bottleAt))
        feed.endedAt = bottleAt
        try store.update(feed)
        let model = PartEditModel(store: store, babyID: baby, target: .bottle(feedID: feed.id, componentID: bottleID),
                                  unit: .millilitres)
        let logged = try store.changeLog().count

        model.volumeML = 120
        model.contents = .breastMilk

        #expect(try model.save())
        #expect(feed.components == [breast, .bottle(id: bottleID, volumeML: 120, contents: .breastMilk, startedAt: bottleAt)])
        #expect(feed.startedAt == start)
        let log = try store.changeLog()
        #expect(log.count == logged + 1)
        #expect(log.last?.operation == .update)
    }

    /// A single-bottle feed's time is the feed's time.
    @Test func movingASingleBottlesTimeMovesTheFeed() throws {
        let (store, baby, clock) = try fixture()
        let at = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: at)
        let componentID = try #require(feed.components.first?.id)
        let model = PartEditModel(store: store, babyID: baby, target: .bottle(feedID: feed.id, componentID: componentID),
                                  unit: .millilitres)
        let moved = at.addingTimeInterval(-600)

        model.start = moved

        #expect(try model.save())
        #expect(feed.startedAt == moved)
        #expect(feed.endedAt == moved)
        #expect(feed.components == [.bottle(id: componentID, volumeML: 90, contents: .formula, startedAt: moved)])
    }

    /// Start and End are independent rows: moving a timed bottle's start
    /// leaves its end where it was, and the length follows.
    @Test func aTimedBottleKeepsItsEndWhenItsStartMoves() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: start)
        let componentID = try #require(feed.components.first?.id)
        feed.components = [.bottle(id: componentID, volumeML: 90, contents: .formula, seconds: 600,
                                   startedAt: start, endedAt: start.addingTimeInterval(600))]
        feed.endedAt = start.addingTimeInterval(600)
        try store.update(feed)
        let model = PartEditModel(store: store, babyID: baby, target: .bottle(feedID: feed.id, componentID: componentID),
                                  unit: .millilitres)
        let moved = start.addingTimeInterval(-300)

        model.start = moved

        #expect(try model.save())
        #expect(feed.components == [.bottle(id: componentID, volumeML: 90, contents: .formula, seconds: 900,
                                            startedAt: moved, endedAt: start.addingTimeInterval(600))])
        #expect(feed.startedAt == moved)
        #expect(feed.endedAt == start.addingTimeInterval(600))
    }

    @Test func aBottleGainsAnEndAndKeepsItsStart() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-1800)
        let feed = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: start)
        let componentID = try #require(feed.components.first?.id)
        let model = PartEditModel(store: store, babyID: baby, target: .bottle(feedID: feed.id, componentID: componentID))
        #expect(model.end == nil)

        model.end = start.addingTimeInterval(900)

        #expect(try model.save())
        #expect(feed.components.first?.bottleSeconds == 900)
        #expect(feed.components.first?.startedAt == start)
        #expect(feed.components.first?.endedAt == start.addingTimeInterval(900))
        #expect(feed.endedAt == start.addingTimeInterval(900))
    }

    /// A bottle that recorded only its length (as the running-timer editor
    /// builds) opens with an end, so moving only its start keeps that end
    /// and a length, rather than losing it.
    @Test func aSecondsOnlyBottleKeepsItsLengthWhenItsStartMoves() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: start)
        let componentID = try #require(feed.components.first?.id)
        feed.components = [.bottle(id: componentID, volumeML: 90, contents: .formula, seconds: 600)]
        try store.update(feed)
        let model = PartEditModel(store: store, babyID: baby, target: .bottle(feedID: feed.id, componentID: componentID))
        #expect(model.end == start.addingTimeInterval(600))
        let moved = start.addingTimeInterval(-300)

        model.start = moved

        #expect(try model.save())
        #expect(feed.components.first?.bottleSeconds == 900)
        #expect(feed.components.first?.startedAt == moved)
        #expect(feed.components.first?.endedAt == start.addingTimeInterval(600))
        #expect(feed.startedAt == moved)
        #expect(feed.endedAt == start.addingTimeInterval(600))
    }

    /// An End-only edit of an old bottle that never recorded its time, in a
    /// two-part entry, changes its length but invents no start.
    @Test func anEndOnlyEditKeepsAnUntimedBottlesStartUnknown() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let breast = try #require(feed.components.first)
        let bottleID = UUID()
        feed.components = [breast, .bottle(id: bottleID, volumeML: 60, contents: .formula, seconds: 300)]
        feed.endedAt = start.addingTimeInterval(1500)
        try store.update(feed)
        let model = PartEditModel(store: store, babyID: baby, target: .bottle(feedID: feed.id, componentID: bottleID))
        #expect(model.end == model.start.addingTimeInterval(300))

        model.end = model.start.addingTimeInterval(400)

        #expect(try model.save())
        #expect(feed.components == [breast, .bottle(id: bottleID, volumeML: 60, contents: .formula, seconds: 400)])
        #expect(feed.startedAt == start)
        #expect(feed.endedAt == start.addingTimeInterval(1500))
    }

    @Test func removingABottleEndBeforeSavingKeepsItAMoment() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-1800)
        let feed = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: start)
        let componentID = try #require(feed.components.first?.id)
        let model = PartEditModel(store: store, babyID: baby, target: .bottle(feedID: feed.id, componentID: componentID))

        model.end = start.addingTimeInterval(900)
        model.end = nil
        model.volumeML = 120

        #expect(try model.save())
        #expect(feed.components.first?.bottleSeconds == nil)
        #expect(feed.components.first?.endedAt == nil)
        #expect(feed.endedAt == start)
    }

    @Test func zeroVolumeCannotBeSaved() throws {
        let (store, baby, clock) = try fixture()
        let feed = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: clock.now.addingTimeInterval(-60))
        let model = PartEditModel(store: store, babyID: baby,
                                  target: .bottle(feedID: feed.id, componentID: try #require(feed.components.first?.id)),
                                  unit: .millilitres)

        model.volumeML = 0

        #expect(!model.canSave)
        #expect(try model.save() == false)
        #expect(feed.components.first?.volumeML == 90)
    }

    /// H-R12: a volume-only edit on a part that already starts before the
    /// previous one ended stays saveable.
    @Test func aVolumeOnlyEditOnAnOverlappingPartSaves() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-7200)
        let left = try store.logBreast(side: .left, from: base, to: base.addingTimeInterval(720), for: baby)
        let bottle = try store.addFeedPart(.bottle(volumeML: 60, contents: .formula, at: base.addingTimeInterval(600)),
                                           toSession: left.id, for: baby)
        let model = PartEditModel(store: store, babyID: baby,
                                  target: .bottle(feedID: bottle.id, componentID: try #require(bottle.components.first?.id)),
                                  unit: .millilitres)

        model.volumeML = 80

        #expect(try model.save())
        #expect(bottle.components.first?.volumeML == 80)
    }

    @Test func movingABottleBeforeThePreviousPartEndsIsRefused() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-7200)
        let left = try store.logBreast(side: .left, from: base, to: base.addingTimeInterval(720), for: baby)
        let bottle = try store.addFeedPart(.bottle(volumeML: 60, contents: .formula, at: base.addingTimeInterval(1200)),
                                           toSession: left.id, for: baby)
        let model = PartEditModel(store: store, babyID: baby,
                                  target: .bottle(feedID: bottle.id, componentID: try #require(bottle.components.first?.id)),
                                  unit: .millilitres)

        model.start = base.addingTimeInterval(300)

        #expect(model.validationMessage?.hasPrefix("The start can't be before the previous part ended") == true)
        #expect(try model.save() == false)
        #expect(bottle.startedAt == base.addingTimeInterval(1200))
    }

    /// An old bottle that never recorded its time keeps it unknown, and the
    /// entry's span stays put, when only its volume is edited.
    @Test func aVolumeOnlyEditKeepsAnUntimedBottlesTimes() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let breast = try #require(feed.components.first)
        let bottleID = UUID()
        feed.components = [breast, .bottle(id: bottleID, volumeML: 60, contents: .formula)]
        feed.endedAt = start.addingTimeInterval(1500)
        try store.update(feed)
        let model = PartEditModel(store: store, babyID: baby, target: .bottle(feedID: feed.id, componentID: bottleID),
                                  unit: .millilitres)

        model.volumeML = 90

        #expect(try model.save())
        #expect(feed.components == [breast, .bottle(id: bottleID, volumeML: 90, contents: .formula)])
        #expect(feed.startedAt == start)
        #expect(feed.endedAt == start.addingTimeInterval(1500))
    }

    @Test func ouncesChipsAreWholeOunces() throws {
        let (store, baby, clock) = try fixture()
        let feed = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: clock.now.addingTimeInterval(-60))
        let model = PartEditModel(store: store, babyID: baby,
                                  target: .bottle(feedID: feed.id, componentID: try #require(feed.components.first?.id)),
                                  unit: .ounces)

        #expect(model.chips == [1, 2, 3, 4].map { Volume(ounces: $0).millilitres })
        #expect(model.label(for: Volume(ounces: 2).millilitres) == "2")
    }

    @Test func theEditModeIsABottleSheetMode() {
        let feedID = UUID(), componentID = UUID()
        let mode = BottleSheetMode.edit(feedID: feedID, componentID: componentID)
        #expect(mode == .edit(feedID: feedID, componentID: componentID))
        #expect(mode.editTarget == .bottle(feedID: feedID, componentID: componentID))
        #expect(BottleSheetMode.start.editTarget == nil)
    }

    // MARK: - Ported from the old generic editor's tests

    /// The step follows the display unit, as on the Bottle sheet: half an
    /// ounce, so a parent reading ounces lands on round ones.
    @Test func theStepFollowsTheDisplayUnit() throws {
        let (store, baby, clock) = try fixture()
        let feed = try store.logBottle(volumeML: 120, contents: .formula, for: baby, at: clock.now.addingTimeInterval(-60))
        let target = PartEditTarget.bottle(feedID: feed.id, componentID: try #require(feed.components.first?.id))

        let millilitres = PartEditModel(store: store, babyID: baby, target: target, unit: .millilitres)
        #expect(millilitres.stepAmount == BottleSheetModel(store: store, babyID: baby, unit: .millilitres, mode: .start).stepAmount)

        let ounces = PartEditModel(store: store, babyID: baby, target: target, unit: .ounces)
        #expect(ounces.stepAmount == Volume(ounces: 0.5).millilitres)
        #expect(Volume(millilitres: ounces.stepAmount * 8).formatted(as: .ounces) == "4 oz")
    }

    /// A time-only edit never clamps a volume outside the chips' range, and
    /// a typed volume is written as typed, not capped.
    @Test func aTimeOnlyEditLeavesALargeVolumeAndATypedOneIsNotCapped() throws {
        let (store, baby, clock) = try fixture()
        let at = clock.now.addingTimeInterval(-3600)
        let huge = try store.logBottle(volumeML: 900, contents: .breastMilk, for: baby, at: at)
        let target = PartEditTarget.bottle(feedID: huge.id, componentID: try #require(huge.components.first?.id))
        let model = PartEditModel(store: store, babyID: baby, target: target, unit: .millilitres)

        model.start = at.addingTimeInterval(-600)
        #expect(try model.save())
        #expect(huge.components.first?.volumeML == 900)

        model.volumeML = 437
        #expect(try model.save())
        #expect(huge.components.first?.volumeML == 437)
    }

    /// Ruling H-R24, the old editor's assertion restored: a legacy bottle
    /// stored at 0 ml takes a note or a new time and stays at 0 — it is not
    /// quietly raised to a floor — while a change to 0 is still refused.
    @Test func aLegacyZeroBottleStaysEditableWhileItsVolumeIsLeftAlone() throws {
        let (store, baby, clock) = try fixture()
        let at = clock.now.addingTimeInterval(-3600)
        let zero = try store.logBottle(volumeML: 0, contents: .formula, for: baby, at: at)
        let target = PartEditTarget.bottle(feedID: zero.id, componentID: try #require(zero.components.first?.id))
        let model = PartEditModel(store: store, babyID: baby, target: target, unit: .millilitres)

        model.note = "Refused it"
        #expect(model.canSave)
        #expect(try model.save())
        #expect(zero.components.first?.volumeML == 0, "a 0 ml bottle is not quietly raised to a floor")
        #expect(zero.note == "Refused it")

        model.start = at.addingTimeInterval(-600)
        #expect(try model.save())
        #expect(zero.startedAt == at.addingTimeInterval(-600))
        #expect(zero.components.first?.volumeML == 0)

        // A volume the parent moves is written; moving it back to 0 is not.
        model.volumeML = 60
        #expect(try model.save())
        #expect(zero.components.first?.volumeML == 60)
        model.volumeML = 0
        #expect(!model.canSave)
        #expect(try model.save() == false)
        #expect(zero.components.first?.volumeML == 60)
    }

    @Test func editingTheFirstOfTwoBottlesLeavesTheOther() throws {
        let (store, baby, _) = try fixture()
        let feed = try store.logBottle(volumeML: 1, contents: .formula, for: baby)
        let milkID = UUID(), formulaID = UUID()
        feed.components = [
            .bottle(id: milkID, volumeML: 50, contents: .breastMilk),
            .bottle(id: formulaID, volumeML: 30, contents: .formula),
        ]
        try store.update(feed)
        let model = PartEditModel(store: store, babyID: baby, target: .bottle(feedID: feed.id, componentID: milkID),
                                  unit: .millilitres)

        model.volumeML = 70
        model.contents = .mixed

        #expect(try model.save())
        #expect(feed.components == [
            .bottle(id: milkID, volumeML: 70, contents: .mixed),
            .bottle(id: formulaID, volumeML: 30, contents: .formula),
        ])
    }

    /// Inside an older phone's combined entry, a bottle may not move before
    /// the side before it ended, nor into the future; nothing is written.
    @Test func aBottleMovedBeforeAnEarlierPartOfItsEntryEndsIsRefused() throws {
        let (store, baby, clock) = try fixture()
        let feed = try store.logBottle(volumeML: 1, contents: .formula, for: baby)
        let start = clock.now.addingTimeInterval(-600)
        let bottleID = UUID()
        feed.components = [
            .breast(id: UUID(), side: .left, seconds: 120, startedAt: start, endedAt: start.addingTimeInterval(120)),
            .bottle(id: bottleID, volumeML: 50, contents: .formula, seconds: 90,
                    startedAt: start.addingTimeInterval(300), endedAt: start.addingTimeInterval(390)),
        ]
        try store.update(feed)
        let model = PartEditModel(store: store, babyID: baby, target: .bottle(feedID: feed.id, componentID: bottleID),
                                  unit: .millilitres)

        model.start = start.addingTimeInterval(100)
        #expect(model.validationMessage?.hasPrefix("The start can't be before the previous part ended") == true)
        #expect(try model.save() == false)

        model.start = clock.now.addingTimeInterval(60)
        #expect(!model.canSave)
        #expect(try model.save() == false)
        #expect(feed.components[1].startedAt == start.addingTimeInterval(300))
    }
}
