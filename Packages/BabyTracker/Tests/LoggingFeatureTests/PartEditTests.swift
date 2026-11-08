import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

/// The clock text the H-R4 message quotes, in the zone the entry was logged in.
private func clockText(_ date: Date, zone: String = TimeZone.current.identifier) -> String {
    var style = Date.FormatStyle(date: .omitted, time: .shortened)
    style.timeZone = TimeZone(identifier: zone) ?? .gmt
    return date.formatted(style)
}

@Suite @MainActor struct PartEditTests {
    // MARK: - Breast

    @Test func breastOpensWithTheSideStartAndDuration() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let component = try #require(feed.components.first)

        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: component.id),
                                  unit: .millilitres)

        #expect(model.side == .left)
        #expect(model.start == start)
        #expect(model.durationSeconds == 720)
        #expect(model.end == start.addingTimeInterval(720))
        #expect(model.note == "")
        #expect(!model.isGone)
        #expect(model.validationMessage == nil)
        #expect(model.canSave)
    }

    @Test func breastSavesANewSideAndDurationKeepingTheComponentID() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let componentID = try #require(feed.components.first?.id)
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: componentID),
                                  unit: .millilitres)
        let logged = try store.changeLog().count

        model.side = .right
        model.end = model.start.addingTimeInterval(900)

        #expect(try model.save())
        #expect(feed.components == [.breast(id: componentID, side: .right, seconds: 900,
                                            startedAt: start, endedAt: start.addingTimeInterval(900))])
        #expect(feed.startedAt == start)
        #expect(feed.endedAt == start.addingTimeInterval(900))
        let log = try store.changeLog()
        #expect(log.count == logged + 1)
        #expect(log.last?.operation == .update)
        #expect(log.last?.entityID == feed.id)
    }

    @Test func movingTheStartKeepsTheEnd() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let componentID = try #require(feed.components.first?.id)
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: componentID),
                                  unit: .millilitres)
        let moved = start.addingTimeInterval(-600)

        model.start = moved

        #expect(model.end == start.addingTimeInterval(720))
        #expect(model.durationSeconds == 1320)
        #expect(try model.save())
        #expect(feed.startedAt == moved)
        #expect(feed.endedAt == start.addingTimeInterval(720))
        #expect(feed.components.first?.durationSeconds == 1320)
    }

    /// Both rows moved into the future together, the length unchanged: the
    /// future is judged whenever a time moves, not only once the length does.
    @Test func startAndEndMovedTogetherIntoTheFutureAreRefused() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let componentID = try #require(feed.components.first?.id)
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: componentID),
                                  unit: .millilitres)

        model.start = clock.now.addingTimeInterval(600)
        model.end = clock.now.addingTimeInterval(600 + 720)

        #expect(model.durationSeconds == 720)
        #expect(model.validationMessage == TimeSpanCopy.inTheFuture)
        #expect(try model.save() == false)
        #expect(feed.startedAt == start)
        #expect(feed.components.first?.durationSeconds == 720)
    }

    @Test func aPartCrossingMidnightSavesItsLength() throws {
        let (store, baby, clock) = try fixture()
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = .gmt
        let midnight = calendar.startOfDay(for: clock.now)
        let start = midnight.addingTimeInterval(-20 * 60)
        let sleep = try store.logSleep(from: start, to: start.addingTimeInterval(600), for: baby)
        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: sleep.id))

        model.end = midnight.addingTimeInterval(20 * 60)

        #expect(model.validationMessage == nil)
        #expect(try model.save())
        #expect(sleep.durationSeconds == 40 * 60)
    }

    /// Review Focus 4: the clear message shows, and nothing is saved.
    @Test func aStartBeforeThePreviousPartEndsIsRefusedAndNothingIsSaved() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-7200)
        let left = try store.logBreast(side: .left, from: base, to: base.addingTimeInterval(720), for: baby)
        let right = try store.addFeedPart(.breast(side: .right, from: base.addingTimeInterval(900), to: base.addingTimeInterval(1800)),
                                          toSession: left.id, for: baby)
        let componentID = try #require(right.components.first?.id)
        let before = right.components
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: right.id, componentID: componentID),
                                  unit: .millilitres)
        let logged = try store.changeLog().count

        model.start = base.addingTimeInterval(600)

        let expected = "The start can't be before the previous part ended, at \(clockText(base.addingTimeInterval(720), zone: right.timeZoneID))."
        #expect(model.validationMessage == expected)
        #expect(!model.canSave)
        #expect(try model.save() == false)
        #expect(model.validationMessage == expected)
        #expect(right.components == before)
        #expect(right.startedAt == base.addingTimeInterval(900))
        #expect(try store.changeLog().count == logged)

        model.start = base.addingTimeInterval(720)
        #expect(model.validationMessage == nil)
        #expect(try model.save())
        #expect(right.startedAt == base.addingTimeInterval(720))
    }

    /// The editor's own check can be out of date; the store's is not. A
    /// previous part lengthened after the editor opened still refuses the
    /// save, and the message then shows.
    @Test func theStoreCheckRefusesWhenThePreviousPartChangedAfterOpening() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-7200)
        let left = try store.logBreast(side: .left, from: base, to: base.addingTimeInterval(720), for: baby)
        let right = try store.addFeedPart(.breast(side: .right, from: base.addingTimeInterval(900), to: base.addingTimeInterval(1800)),
                                          toSession: left.id, for: baby)
        let componentID = try #require(right.components.first?.id)
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: right.id, componentID: componentID),
                                  unit: .millilitres)

        let leftComponentID = try #require(left.components.first?.id)
        left.components = [.breast(id: leftComponentID, side: .left, seconds: 850, startedAt: base,
                                   endedAt: base.addingTimeInterval(850))]
        left.endedAt = base.addingTimeInterval(850)
        try store.update(left)
        let logged = try store.changeLog().count

        model.start = base.addingTimeInterval(800)
        #expect(try model.save() == false)
        #expect(model.validationMessage
            == "The start can't be before the previous part ended, at \(clockText(base.addingTimeInterval(850), zone: right.timeZoneID)).")
        #expect(right.startedAt == base.addingTimeInterval(900))
        #expect(try store.changeLog().count == logged)
    }

    /// H-R12: a start left alone is not checked, so a part that already
    /// overlaps the one before stays editable.
    @Test func anUnchangedStartIsNotCheckedAgainstThePreviousPart() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-7200)
        let left = try store.logBreast(side: .left, from: base, to: base.addingTimeInterval(720), for: baby)
        let right = try store.addFeedPart(.breast(side: .right, from: base.addingTimeInterval(600), to: base.addingTimeInterval(1200)),
                                          toSession: left.id, for: baby)
        let componentID = try #require(right.components.first?.id)
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: right.id, componentID: componentID),
                                  unit: .millilitres)

        model.side = .left

        #expect(model.validationMessage == nil)
        #expect(try model.save())
        #expect(right.components.first?.breastSide == .left)
    }

    /// An old part that never recorded its own times keeps them unknown, and
    /// the entry's span stays put, when only its side or note is edited.
    @Test func aSideOnlyEditLeavesAnOldPartsTimesAsStored() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let breastID = try #require(feed.components.first?.id)
        let bottleID = UUID()
        feed.components = [.breast(id: breastID, side: .left, seconds: 720),
                           .bottle(id: bottleID, volumeML: 60, contents: .formula)]
        feed.endedAt = start.addingTimeInterval(1500)
        try store.update(feed)
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: breastID),
                                  unit: .millilitres)

        model.side = .right
        model.note = "fussy"

        #expect(try model.save())
        #expect(feed.components == [.breast(id: breastID, side: .right, seconds: 720),
                                    .bottle(id: bottleID, volumeML: 60, contents: .formula)])
        #expect(feed.startedAt == start)
        #expect(feed.endedAt == start.addingTimeInterval(1500))
        #expect(feed.note == "fussy")
    }

    /// A new duration alone does not invent a start for an old part that
    /// never recorded one, nor move the entry's span.
    @Test func aDurationOnlyEditKeepsAnOldPartsUnknownStart() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let breastID = try #require(feed.components.first?.id)
        let bottleID = UUID()
        feed.components = [.breast(id: breastID, side: .left, seconds: 720),
                           .bottle(id: bottleID, volumeML: 60, contents: .formula)]
        feed.endedAt = start.addingTimeInterval(1500)
        try store.update(feed)
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: breastID),
                                  unit: .millilitres)

        model.end = model.start.addingTimeInterval(900)

        #expect(try model.save())
        #expect(feed.components == [.breast(id: breastID, side: .left, seconds: 900),
                                    .bottle(id: bottleID, volumeML: 60, contents: .formula)])
        #expect(feed.startedAt == start)
        #expect(feed.endedAt == start.addingTimeInterval(1500))
    }

    /// A lone part's time is its feed's time, so a new duration on one that
    /// never recorded its own times takes the feed's start and refits the
    /// span: 10:00–10:12 at 20m ends 10:20.
    @Test func aDurationOnlyEditOfALonePartRefitsTheFeed() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let breastID = try #require(feed.components.first?.id)
        feed.components = [.breast(id: breastID, side: .left, seconds: 720)]
        try store.update(feed)
        #expect(feed.endedAt == start.addingTimeInterval(720))
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: breastID),
                                  unit: .millilitres)
        #expect(model.start == start)

        model.end = model.start.addingTimeInterval(1200)

        #expect(try model.save())
        #expect(feed.components == [.breast(id: breastID, side: .left, seconds: 1200, startedAt: start,
                                            endedAt: start.addingTimeInterval(1200))])
        #expect(feed.startedAt == start)
        #expect(feed.endedAt == start.addingTimeInterval(1200))
        #expect(feed.totalBreastSeconds == 1200)
    }

    /// Final review finding 4: a legacy breast part stored with 0 seconds
    /// (so its clamped `durationSeconds` reads 0 whichever way Start moves)
    /// must still refuse a Start dragged past its End, not just a Start that
    /// grows or shrinks the length.
    @Test func aZeroSecondLegacyPartRefusesAStartMovedPastItsEnd() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let breastID = try #require(feed.components.first?.id)
        feed.components = [.breast(id: breastID, side: .left, seconds: 0)]
        feed.endedAt = start
        try store.update(feed)
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: breastID),
                                  unit: .millilitres)
        #expect(model.start == start)
        #expect(model.end == start)
        #expect(model.durationSeconds == 0)

        model.start = start.addingTimeInterval(60)

        #expect(model.validationMessage == TimeSpanCopy.endBeforeStart)
        #expect(try model.save() == false)
    }

    /// Parked from Task 3: a side or note edit of a lone part that never
    /// recorded its own times leaves them unrecorded — the edit writes only
    /// what the parent changed.
    @Test func aSideAndNoteEditOfALoneUntimedPartWritesNoTimes() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let breastID = try #require(feed.components.first?.id)
        feed.components = [.breast(id: breastID, side: .left, seconds: 720)]
        try store.update(feed)
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: breastID),
                                  unit: .millilitres)

        model.side = .right
        model.note = "sleepy"

        #expect(try model.save())
        #expect(feed.components == [.breast(id: breastID, side: .right, seconds: 720)])
        #expect(feed.startedAt == start)
        #expect(feed.endedAt == start.addingTimeInterval(720))
        #expect(feed.note == "sleepy")
    }

    /// The 30-second floor is judged only once the duration changes, so an
    /// old part stored shorter keeps taking side and note edits.
    @Test func anOldShortBreastPartStillSavesASideEdit() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let breastID = try #require(feed.components.first?.id)
        feed.components = [.breast(id: breastID, side: .left, seconds: 20, startedAt: start,
                                   endedAt: start.addingTimeInterval(20))]
        feed.endedAt = start.addingTimeInterval(20)
        try store.update(feed)
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: breastID),
                                  unit: .millilitres)
        #expect(model.validationMessage == nil)

        model.side = .right

        #expect(try model.save())
        #expect(feed.components == [.breast(id: breastID, side: .right, seconds: 20, startedAt: start,
                                            endedAt: start.addingTimeInterval(20))])
        model.end = model.start.addingTimeInterval(25)
        #expect(model.validationMessage == TimeSpanCopy.breastTooShort)
    }

    @Test func aBreastPartUnderThirtySecondsIsRefused() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let componentID = try #require(feed.components.first?.id)
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: componentID),
                                  unit: .millilitres)

        model.end = model.start.addingTimeInterval(20)

        #expect(model.validationMessage == TimeSpanCopy.breastTooShort)
        #expect(try model.save() == false)
        #expect(feed.components.first?.durationSeconds == 720)
    }

    @Test func theNoteRoundTrips() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let target = PartEditTarget.breast(feedID: feed.id, componentID: try #require(feed.components.first?.id))
        let model = PartEditModel(store: store, babyID: baby, target: target, unit: .millilitres)

        model.note = "  sleepy at the end "
        #expect(try model.save())
        #expect(feed.note == "sleepy at the end")
        #expect(PartEditModel(store: store, babyID: baby, target: target, unit: .millilitres).note == "sleepy at the end")

        model.note = "   "
        #expect(try model.save())
        #expect(feed.note == nil)
    }

    // MARK: - Sleep

    @Test func sleepOpensWithItsStartEndKindAndNote() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-7200)
        let sleep = try store.logSleep(from: start, to: start.addingTimeInterval(3600), for: baby)
        sleep.kindOverride = .night
        sleep.note = "car seat"
        try store.update(sleep)

        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: sleep.id), unit: .millilitres)

        #expect(model.start == start)
        #expect(model.end == start.addingTimeInterval(3600))
        #expect(model.sleepKind == .night)
        #expect(model.note == "car seat")
        #expect(model.validationMessage == nil)
    }

    @Test func napNightAndAutoSetTheKindOverride() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-7200)
        let sleep = try store.logSleep(from: start, to: start.addingTimeInterval(3600), for: baby)
        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: sleep.id), unit: .millilitres)
        #expect(model.sleepKind == nil)

        model.sleepKind = .nap
        #expect(try model.save())
        #expect(sleep.kindOverride == .nap)

        model.sleepKind = .night
        #expect(try model.save())
        #expect(sleep.kindOverride == .night)

        model.sleepKind = nil
        #expect(try model.save())
        #expect(sleep.kindOverride == nil)
    }

    @Test func sleepSavesANewStartAndEnd() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-7200)
        let sleep = try store.logSleep(from: start, to: start.addingTimeInterval(3600), for: baby)
        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: sleep.id), unit: .millilitres)
        let logged = try store.changeLog().count

        model.start = start.addingTimeInterval(-300)
        model.end = start.addingTimeInterval(4000)

        #expect(try model.save())
        #expect(sleep.startedAt == start.addingTimeInterval(-300))
        #expect(sleep.endedAt == start.addingTimeInterval(4000))
        #expect(try store.changeLog().count == logged + 1)
    }

    @Test func aSleepEndingBeforeItStartsIsRefused() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-7200)
        let sleep = try store.logSleep(from: start, to: start.addingTimeInterval(3600), for: baby)
        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: sleep.id), unit: .millilitres)

        model.end = start.addingTimeInterval(-60)

        #expect(model.validationMessage == TimeSpanCopy.endBeforeStart)
        #expect(try model.save() == false)
        #expect(sleep.endedAt == start.addingTimeInterval(3600))
    }

    @Test func aSleepEndInTheFutureIsRefused() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-7200)
        let sleep = try store.logSleep(from: start, to: start.addingTimeInterval(3600), for: baby)
        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: sleep.id), unit: .millilitres)

        model.end = clock.now.addingTimeInterval(600)

        #expect(model.validationMessage == TimeSpanCopy.inTheFuture)
        #expect(try model.save() == false)
    }

    // MARK: - Session bounds (H-R14, H-R15)

    /// Review Focus 4 for a sleep (H-R14): the clear message shows, and
    /// nothing is saved.
    @Test func aSleepStartBeforeThePreviousSleepEndsIsRefusedAndNothingIsSaved() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-6 * 3600)
        let first = try store.logSleep(from: base, to: base.addingTimeInterval(2400), for: baby)
        let second = try store.addSleepPart(from: base.addingTimeInterval(3000), to: base.addingTimeInterval(5400),
                                            toSession: first.id, for: baby)
        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: second.id), unit: .millilitres)
        let logged = try store.changeLog().count

        model.start = base.addingTimeInterval(2000)

        let expected = "The start can't be before the previous part ended, at \(clockText(base.addingTimeInterval(2400), zone: second.timeZoneID))."
        #expect(model.validationMessage == expected)
        #expect(!model.canSave)
        #expect(try model.save() == false)
        #expect(model.validationMessage == expected)
        #expect(second.startedAt == base.addingTimeInterval(3000))
        #expect(second.endedAt == base.addingTimeInterval(5400))
        #expect(try store.changeLog().count == logged)

        model.start = base.addingTimeInterval(2400)
        #expect(model.validationMessage == nil)
        #expect(try model.save())
        #expect(second.startedAt == base.addingTimeInterval(2400))
    }

    /// Moved into the next part, a part is refused, and nothing is saved.
    @Test func aStartIntoTheNextPartIsRefusedAndNothingIsSaved() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-7200)
        let left = try store.logBreast(side: .left, from: base, to: base.addingTimeInterval(720), for: baby)
        let right = try store.addFeedPart(.breast(side: .right, from: base.addingTimeInterval(900), to: base.addingTimeInterval(1800)),
                                          toSession: left.id, for: baby)
        let before = left.components
        let model = PartEditModel(store: store, babyID: baby,
                                  target: .breast(feedID: left.id, componentID: try #require(left.components.first?.id)),
                                  unit: .millilitres)
        let logged = try store.changeLog().count

        // Both rows moved, so the span itself is sound and the overlap is
        // what refuses it.
        model.start = base.addingTimeInterval(1000)
        model.end = base.addingTimeInterval(1720)

        let expected = "The start can't be before the previous part ended, at \(clockText(base.addingTimeInterval(1800), zone: left.timeZoneID))."
        #expect(model.validationMessage == expected)
        #expect(try model.save() == false)
        #expect(left.components == before)
        #expect(left.startedAt == base)
        #expect(right.effectiveFeedingSessionID == left.id)
        #expect(try store.changeLog().count == logged)

        model.start = base.addingTimeInterval(-300)
        model.end = base.addingTimeInterval(720)
        #expect(model.validationMessage == nil)
        #expect(try model.save())
        #expect(left.startedAt == base.addingTimeInterval(-300))
    }

    /// Final review M3: a longer duration or a later end may not run into
    /// the next part, which would count the overlap twice ("27m breast",
    /// day totals, Home), as the old editor's overlap check prevented.
    @Test func aDurationRunningIntoTheNextPartIsRefused() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-7200)
        let left = try store.logBreast(side: .left, from: base, to: base.addingTimeInterval(720), for: baby)
        _ = try store.addFeedPart(.breast(side: .right, from: base.addingTimeInterval(900), to: base.addingTimeInterval(1800)),
                                  toSession: left.id, for: baby)
        let before = left.components
        let model = PartEditModel(store: store, babyID: baby,
                                  target: .breast(feedID: left.id, componentID: try #require(left.components.first?.id)),
                                  unit: .millilitres)

        model.end = model.start.addingTimeInterval(1000)

        #expect(model.validationMessage
            == "The end can't be after the next part began, at \(clockText(base.addingTimeInterval(900), zone: left.timeZoneID)).")
        #expect(try model.save() == false)
        #expect(left.components == before)

        model.end = model.start.addingTimeInterval(900)
        #expect(model.validationMessage == nil)
        #expect(try model.save())
    }

    @Test func aSleepEndRunningIntoTheNextSleepIsRefused() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-6 * 3600)
        let first = try store.logSleep(from: base, to: base.addingTimeInterval(2400), for: baby)
        _ = try store.addSleepPart(from: base.addingTimeInterval(3000), to: base.addingTimeInterval(5400),
                                   toSession: first.id, for: baby)
        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: first.id), unit: .millilitres)

        model.end = base.addingTimeInterval(3100)

        #expect(model.validationMessage
            == "The end can't be after the next part began, at \(clockText(base.addingTimeInterval(3000), zone: first.timeZoneID)).")
        #expect(try model.save() == false)
        #expect(first.endedAt == base.addingTimeInterval(2400))
    }

    /// Like the start (ruling H-R12), only a changed end is judged: a part
    /// that already overlaps its neighbour keeps a note edit saveable.
    @Test func anExistingOverlapStaysEditableWhileTheEndIsLeftAlone() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-6 * 3600)
        let first = try store.logSleep(from: base, to: base.addingTimeInterval(2400), for: baby)
        _ = try store.addSleepPart(from: base.addingTimeInterval(3000), to: base.addingTimeInterval(5400),
                                   toSession: first.id, for: baby)
        first.endedAt = base.addingTimeInterval(3200)
        try store.context.save()
        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: first.id), unit: .millilitres)

        model.note = "woke briefly"

        #expect(model.validationMessage == nil)
        #expect(try model.save())
    }

    /// A part moved past the next one, into a free stretch, reorders the
    /// session: the part now first keys it, so Split still cuts where shown.
    @Test func aPartMovedPastTheNextOneReordersTheSession() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-7200)
        let right = try store.logBreast(side: .right, from: base, to: base.addingTimeInterval(1140), for: baby)
        let bottle = try store.addFeedPart(.bottle(volumeML: 100, contents: .formula, at: base.addingTimeInterval(4860)),
                                           toSession: right.id, for: baby)
        let left = try store.addFeedPart(.breast(side: .left, from: base.addingTimeInterval(4861),
                                                 to: base.addingTimeInterval(5461)),
                                         toSession: right.id, for: baby)
        let model = PartEditModel(store: store, babyID: baby,
                                  target: .breast(feedID: left.id, componentID: try #require(left.components.first?.id)),
                                  unit: .millilitres)

        model.start = base.addingTimeInterval(1140)
        model.end = base.addingTimeInterval(1740)

        #expect(model.validationMessage == nil)
        #expect(try model.save())
        let session = try #require(try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture).first)
        #expect(session.id == right.id)
        #expect(session.feedIDs == [right.id, left.id, bottle.id])

        // Moved before the first part, it keys the session itself.
        model.start = base.addingTimeInterval(-900)
        model.end = base.addingTimeInterval(-300)
        #expect(try model.save())
        let rekeyed = try #require(try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture).first)
        #expect(rekeyed.id == left.id)
        #expect(rekeyed.feedIDs == [left.id, right.id, bottle.id])
        #expect(left.feedingSessionID == nil)
        #expect(right.feedingSessionID == left.id)
        #expect(bottle.feedingSessionID == left.id)
        try store.separateFeed(at: bottle.id, for: baby)
        #expect(try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture).map(\.feedIDs)
            == [[left.id, right.id], [bottle.id]])
    }

    @Test func aSleepMovedPastTheNextSleepReordersTheSession() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-6 * 3600)
        let first = try store.logSleep(from: base, to: base.addingTimeInterval(2400), for: baby)
        let second = try store.addSleepPart(from: base.addingTimeInterval(3000), to: base.addingTimeInterval(5400),
                                            toSession: first.id, for: baby)
        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: first.id), unit: .millilitres)

        model.start = base.addingTimeInterval(5500)
        model.end = base.addingTimeInterval(6000)

        #expect(model.validationMessage == nil)
        #expect(try model.save())
        #expect(second.sleepSessionID == nil)
        #expect(first.sleepSessionID == second.id)
        #expect(try store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture).map(\.sleepIDs)
            == [[second.id, first.id]])
    }

    @Test func aSleepStartIntoTheNextSleepIsRefused() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-6 * 3600)
        let first = try store.logSleep(from: base, to: base.addingTimeInterval(2400), for: baby)
        let second = try store.addSleepPart(from: base.addingTimeInterval(3000), to: base.addingTimeInterval(5400),
                                            toSession: first.id, for: baby)
        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: first.id), unit: .millilitres)
        let logged = try store.changeLog().count

        model.start = base.addingTimeInterval(3100)
        model.end = base.addingTimeInterval(3200)

        #expect(model.validationMessage
            == "The start can't be before the previous part ended, at \(clockText(base.addingTimeInterval(5400), zone: first.timeZoneID)).")
        #expect(try model.save() == false)
        #expect(first.startedAt == base)
        #expect(second.effectiveSleepSessionID == first.id)
        #expect(try store.changeLog().count == logged)
    }

    /// The store's check covers the next part too, when the editor's figure
    /// is out of date: a next sleep moved earlier after the editor opened.
    @Test func theStoreCheckRefusesWhenTheNextSleepMovedAfterOpening() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-6 * 3600)
        let first = try store.logSleep(from: base, to: base.addingTimeInterval(2400), for: baby)
        let second = try store.addSleepPart(from: base.addingTimeInterval(4000), to: base.addingTimeInterval(5400),
                                            toSession: first.id, for: baby)
        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: first.id), unit: .millilitres)

        second.startedAt = base.addingTimeInterval(3000)
        try store.update(second)
        let logged = try store.changeLog().count

        model.start = base.addingTimeInterval(3500)
        model.end = base.addingTimeInterval(3600)
        #expect(try model.save() == false)
        #expect(model.validationMessage
            == "The start can't be before the previous part ended, at \(clockText(base.addingTimeInterval(5400), zone: first.timeZoneID)).")
        #expect(first.startedAt == base)
        #expect(try store.changeLog().count == logged)
    }

    // MARK: - Delete (H-R2, H-R11)

    @Test func deletingTheOnlyComponentTombstonesTheEntry() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let model = PartEditModel(store: store, babyID: baby,
                                  target: .breast(feedID: feed.id, componentID: try #require(feed.components.first?.id)),
                                  unit: .millilitres)

        try model.delete()

        #expect(feed.isTombstoned)
        #expect(try store.entry(id: feed.id, for: baby) == nil)
    }

    @Test func deletingOneOfTwoComponentsRemovesOnlyThatOne() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let breast = try #require(feed.components.first)
        let bottleID = UUID()
        feed.components.append(.bottle(id: bottleID, volumeML: 60, contents: .formula, startedAt: start.addingTimeInterval(900)))
        try store.update(feed)
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: breast.id),
                                  unit: .millilitres)

        try model.delete()

        #expect(!feed.isTombstoned)
        #expect(feed.components.map(\.id) == [bottleID])
    }

    @Test func deletingASleepTombstonesIt() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-7200)
        let sleep = try store.logSleep(from: start, to: start.addingTimeInterval(3600), for: baby)
        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: sleep.id), unit: .millilitres)

        try model.delete()

        #expect(sleep.isTombstoned)
    }

    /// Deleting the root part keeps the session keyed by a live entry: the
    /// part editor deletes through the session-aware store call.
    @Test func deletingTheFirstPartOfASessionPromotesTheNext() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-7200)
        let left = try store.logBreast(side: .left, from: base, to: base.addingTimeInterval(720), for: baby)
        let right = try store.addFeedPart(.breast(side: .right, from: base.addingTimeInterval(900), to: base.addingTimeInterval(1800)),
                                          toSession: left.id, for: baby)
        let model = PartEditModel(store: store, babyID: baby,
                                  target: .breast(feedID: left.id, componentID: try #require(left.components.first?.id)),
                                  unit: .millilitres)

        try model.delete()

        #expect(left.isTombstoned)
        #expect(right.feedingSessionID == nil)
        #expect(right.effectiveFeedingSessionID == right.id)
    }

    // MARK: - Gone (Review Focus 5)

    @Test func becomesGoneWhenThePartIsRemovedElsewhere() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(720), for: baby)
        let componentID = try #require(feed.components.first?.id)
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: componentID),
                                  unit: .millilitres)

        try store.removeComponent(componentID, fromFeed: feed.id, for: baby)
        model.reload()

        #expect(model.isGone)
        #expect(throws: EditModeError.gone) { try model.save() }
    }

    @Test func reloadTakesTheOtherPhonesEditOnlyIntoUntouchedFields() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-7200)
        let sleep = try store.logSleep(from: start, to: start.addingTimeInterval(3600), for: baby)
        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: sleep.id), unit: .millilitres)

        model.note = "typing"
        sleep.note = "remote"
        sleep.endedAt = start.addingTimeInterval(4200)
        try store.update(sleep)
        model.reload()

        #expect(model.note == "typing")
        #expect(model.end == start.addingTimeInterval(4200))
    }

    // MARK: - Ported from the old generic editor's tests

    /// 13:00 in Berlin derives as a nap. Night overrides it; Auto clears the
    /// override rather than flipping it, and the derived kind returns.
    @Test func autoClearsTheOverrideAndTheDerivedKindReturns() throws {
        let (store, baby, _) = try fixture()
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = try #require(TimeZone(identifier: "Europe/Berlin"))
        let afternoon = try #require(calendar.date(from: DateComponents(year: 2026, month: 9, day: 18, hour: 13)))
        let sleep = try store.logSleep(from: afternoon, to: afternoon.addingTimeInterval(7200), for: baby)
        sleep.timeZoneID = "Europe/Berlin"
        try store.update(sleep)
        #expect(sleep.kind == .nap)

        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: sleep.id), unit: .millilitres)
        #expect(model.sleepKind == nil, "an untouched sleep starts on Auto")
        model.sleepKind = .night
        #expect(try model.save())
        #expect(sleep.kind == .night)

        let reopened = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: sleep.id), unit: .millilitres)
        #expect(reopened.sleepKind == .night)
        reopened.sleepKind = nil
        #expect(try reopened.save())
        #expect(sleep.kindOverride == nil)
        #expect(sleep.kind == .nap, "with no override the derived kind returns")
    }

    @Test func aSleepEndOnlyEditKeepsItsStart() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-7200)
        let end = clock.now.addingTimeInterval(-3600)
        let sleep = try store.logSleep(from: start, to: end, for: baby)
        let model = PartEditModel(store: store, babyID: baby, target: .sleep(sleepID: sleep.id), unit: .millilitres)
        let revision = store.revision

        model.end = end.addingTimeInterval(1200)

        #expect(model.canSave)
        #expect(try model.save())
        #expect(sleep.startedAt == start)
        #expect(sleep.endedAt == end.addingTimeInterval(1200))
        #expect(store.revision > revision)
        #expect(try store.changeLog().last?.operation == .update)
    }

    /// A note edit on one part of an old combined feed leaves every part's
    /// recorded times exactly as they were.
    @Test func aNoteEditKeepsEveryPartsKnownTimes() throws {
        let (store, baby, clock) = try fixture()
        let feed = try store.logBottle(volumeML: 1, contents: .formula, for: baby)
        let start = clock.now.addingTimeInterval(-600)
        let breastID = UUID(), bottleID = UUID()
        feed.components = [
            .breast(id: breastID, side: .left, seconds: 120, startedAt: start, endedAt: start.addingTimeInterval(120)),
            .bottle(id: bottleID, volumeML: 60, contents: .formula, seconds: 90,
                    startedAt: start.addingTimeInterval(180), endedAt: start.addingTimeInterval(270)),
        ]
        try store.update(feed)
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: breastID),
                                  unit: .millilitres)

        model.note = "After a short break"

        #expect(try model.save())
        #expect(feed.components[0].startedAt == start)
        #expect(feed.components[0].endedAt == start.addingTimeInterval(120))
        #expect(feed.components[1].startedAt == start.addingTimeInterval(180))
        #expect(feed.components[1].endedAt == start.addingTimeInterval(270))
        #expect(feed.note == "After a short break")
    }

    /// An older phone's feed with two breast sides in one entry: moving the
    /// first side's end changes that side's length and nothing of the other.
    @Test func anEndCorrectionChangesOnlyThatSidesLength() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-600)
        let firstEnd = start.addingTimeInterval(120)
        let secondEnd = firstEnd.addingTimeInterval(180)
        let feed = try store.logBreast(side: .left, from: start, to: secondEnd, for: baby)
        let firstID = try #require(feed.components.first?.id)
        feed.components = [
            .breast(id: firstID, side: .left, seconds: 120, startedAt: start, endedAt: firstEnd),
            .breast(id: UUID(), side: .right, seconds: 180, startedAt: firstEnd, endedAt: secondEnd),
        ]
        try store.update(feed)
        let second = feed.components[1]
        let model = PartEditModel(store: store, babyID: baby, target: .breast(feedID: feed.id, componentID: firstID),
                                  unit: .millilitres)

        model.end = start.addingTimeInterval(90)

        #expect(try model.save())
        #expect(feed.components[0].durationSeconds == 90)
        #expect(feed.components[0].startedAt == start)
        #expect(feed.components[0].endedAt == start.addingTimeInterval(90))
        #expect(feed.components[1] == second)
        #expect(feed.totalBreastSeconds == 270)
    }
}
