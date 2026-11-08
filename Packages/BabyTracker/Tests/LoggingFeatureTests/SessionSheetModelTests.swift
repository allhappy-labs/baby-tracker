import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

/// The clock text the sheet shows, in the zone the entry was logged in.
private func clockText(_ date: Date, zone: String = TimeZone.current.identifier) -> String {
    var style = Date.FormatStyle(date: .omitted, time: .shortened)
    style.timeZone = TimeZone(identifier: zone) ?? .gmt
    return date.formatted(style)
}

/// Left 12m, Right 15m starting 3m after Left ended, and a 60 ml formula
/// bottle 47m after Right ended: three entries, one session.
@MainActor
private func threeEntrySession() throws
    -> (store: BabyStore, baby: UUID, clock: TestDateProvider, left: FeedEntry, right: FeedEntry, bottle: FeedEntry) {
    let (store, baby, clock) = try fixture()
    let base = clock.now.addingTimeInterval(-3 * 3600)
    let left = try store.logBreast(side: .left, from: base, to: base.addingTimeInterval(12 * 60), for: baby)
    let right = try store.addFeedPart(.breast(side: .right, from: base.addingTimeInterval(15 * 60),
                                              to: base.addingTimeInterval(30 * 60)),
                                      toSession: left.id, for: baby)
    let bottle = try store.addFeedPart(.bottle(volumeML: 60, contents: .formula, at: base.addingTimeInterval(77 * 60)),
                                       toSession: left.id, for: baby)
    return (store, baby, clock, left, right, bottle)
}

@MainActor
private func feedSessions(_ store: BabyStore, _ baby: UUID) throws -> [FeedingSession] {
    try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture)
}

@Suite @MainActor struct SessionSheetModelTests {
    // MARK: - Reading

    @Test func partsGapsTitleAndTotalsForAThreeEntrySession() throws {
        let (store, baby, _, left, right, bottle) = try threeEntrySession()
        let base = left.startedAt
        let zone = left.timeZoneID

        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)

        #expect(!model.isGone)
        #expect(model.rootID == left.id)
        #expect(model.title == "\(clockText(base, zone: zone)) – \(clockText(base.addingTimeInterval(77 * 60), zone: zone))")
        #expect(model.totals == "1h 17m · 27m breast · 60 ml")
        #expect(model.parts.count == 3)

        let (l, r, b) = (model.parts[0], model.parts[1], model.parts[2])
        #expect(l.id == left.components[0].id)
        #expect(l.target == .breast(feedID: left.id, componentID: left.components[0].id))
        #expect(l.icon == .breastLeft)
        #expect(l.label == "Left")
        #expect(l.detail == "\(clockText(base, zone: zone))–\(clockText(base.addingTimeInterval(12 * 60), zone: zone))")
        #expect(l.gapBefore == nil)
        #expect(!l.canSplitBefore)

        #expect(r.target == .breast(feedID: right.id, componentID: right.components[0].id))
        #expect(r.icon == .breastRight)
        #expect(r.label == "Right")
        #expect(r.detail == "\(clockText(base.addingTimeInterval(15 * 60), zone: zone))–\(clockText(base.addingTimeInterval(30 * 60), zone: zone))")
        #expect(r.gapBefore == "3m later")
        #expect(r.canSplitBefore)

        #expect(b.target == .bottle(feedID: bottle.id, componentID: bottle.components[0].id))
        #expect(b.icon == .bottle)
        #expect(b.label == "Formula 60 ml")
        #expect(b.detail == clockText(base.addingTimeInterval(77 * 60), zone: zone))
        #expect(b.gapBefore == "47m later")
        #expect(b.canSplitBefore)
    }

    /// The four timed parts the History screenshot showed — Right 13:50 for
    /// 7m 3s, Left 14:18 for 10m 16s, breast milk 14:46 for 5m 10s, formula
    /// 15:05 for 6m 25s — with each entry ending when its part did. Each gap
    /// runs from the previous part's end, and the session ends with the
    /// formula, not later.
    @Test func gapsAndEndOfTheScreenshotsFourTimedParts() throws {
        let (store, baby, clock) = try fixture()
        let zone = TimeZone.current.identifier
        let base = clock.now.addingTimeInterval(-4 * 3600)     // "13:50"
        func entry(_ kind: FeedKind, at minutes: Double, seconds: Int, _ component: (UUID, Date, Date) -> FeedComponent) -> FeedEntry {
            let start = base.addingTimeInterval(minutes * 60)
            let end = start.addingTimeInterval(TimeInterval(seconds))
            let feed = FeedEntry(babyID: baby, kind: kind, startedAt: start, timeZoneID: zone)
            feed.endedAt = end
            feed.components = [component(UUID(), start, end)]
            return feed
        }
        let right = entry(.breast, at: 0, seconds: 423) { .breast(id: $0, side: .right, seconds: 423, startedAt: $1, endedAt: $2) }
        let left = entry(.breast, at: 28, seconds: 616) { .breast(id: $0, side: .left, seconds: 616, startedAt: $1, endedAt: $2) }
        let milk = entry(.bottle, at: 56, seconds: 310) {
            .bottle(id: $0, volumeML: 40, contents: .breastMilk, seconds: 310, startedAt: $1, endedAt: $2)
        }
        let formula = entry(.bottle, at: 75, seconds: 385) {
            .bottle(id: $0, volumeML: 40, contents: .formula, seconds: 385, startedAt: $1, endedAt: $2)
        }
        for feed in [right, left, milk, formula] {
            store.context.insert(feed)
            try store.record(.create, on: feed)
        }
        try store.combineFeeds([right.id, left.id, milk.id, formula.id], for: baby)

        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: right.id)

        // 14:18 − 13:57:03, 14:46 − 14:28:16, 15:05 − 14:51:10: whole minutes.
        #expect(model.parts.map(\.gapBefore) == [nil, "20m later", "17m later", "13m later"])
        let formulaEnd = base.addingTimeInterval(75 * 60 + 385)
        #expect(try feedSessions(store, baby).first?.endedAt == formulaEnd)
        #expect(model.title == "\(clockText(base, zone: zone)) – \(clockText(formulaEnd, zone: zone))")
    }

    /// An older phone's timer could pause, so a part's recorded end may lie
    /// past its start plus its length. The gap runs from the recorded end:
    /// the feed was still under way until then. (The UI seed that showed
    /// "9m later" everywhere stored ends like that by mistake.)
    @Test func theGapRunsFromAPartsRecordedEnd() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-3 * 3600)
        let right = try store.logBreast(side: .right, from: base, to: base.addingTimeInterval(19 * 60), for: baby)
        right.components = [.breast(id: right.components[0].id, side: .right, seconds: 423,
                                    startedAt: base, endedAt: base.addingTimeInterval(19 * 60))]
        try store.update(right)
        let left = try store.addFeedPart(.breast(side: .left, from: base.addingTimeInterval(28 * 60),
                                                 to: base.addingTimeInterval(38 * 60)),
                                         toSession: right.id, for: baby)

        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: right.id)

        #expect(model.parts.map(\.id) == [right.components[0].id, left.components[0].id])
        #expect(model.parts.last?.gapBefore == "9m later")
    }

    /// Ruling H-R2: an old feed holding several components in one entry
    /// lists each as a part, but cannot be split between them.
    @Test func anOldSingleEntryComboCannotBeSplit() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(600), for: baby)
        feed.components = [
            .breast(id: UUID(), side: .left, seconds: 300),
            .breast(id: UUID(), side: .right, seconds: 300),
            .bottle(id: UUID(), volumeML: 30, contents: .breastMilk),
        ]
        try store.update(feed)

        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: feed.id, unit: .millilitres)

        #expect(model.parts.map(\.label) == ["Left", "Right", "Breast milk 30 ml"])
        #expect(model.parts.allSatisfy { !$0.canSplitBefore })
        // Untimed parts of one entry: no gap is known between them.
        #expect(model.parts.allSatisfy { $0.gapBefore == nil })
        #expect(model.totals == "10m · 10m breast · 30 ml")
    }

    /// The gap after an old combined feed is dated from the feed's own end,
    /// though its parts never recorded theirs.
    @Test func theGapAfterAnOldComboUsesTheFeedsEnd() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-3600)
        let feed = try store.logBreast(side: .left, from: start, to: start.addingTimeInterval(600), for: baby)
        feed.components = [.breast(id: UUID(), side: .left, seconds: 300), .breast(id: UUID(), side: .right, seconds: 300)]
        try store.update(feed)
        try store.addFeedPart(.bottle(volumeML: 60, contents: .formula, at: start.addingTimeInterval(900)),
                              toSession: feed.id, for: baby)

        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: feed.id, unit: .millilitres)

        #expect(model.parts.map(\.gapBefore) == [nil, nil, "5m later"])
        #expect(model.parts.map(\.canSplitBefore) == [false, false, true])
    }

    @Test func addPartOffersOnlyTheSessionsKinds() {
        #expect(AddPartKind.offered(for: .feed) == [.left, .right, .bottle, .solids])
        #expect(AddPartKind.offered(for: .sleep) == [.sleep])
    }

    /// Solids open the Solids panel at the feed's end; nothing is written
    /// until they are named, and saving adds them as the feed's last part.
    @Test func solidsAreAddedOnceNamedAndListAsAPart() throws {
        let (store, baby, _, left, _, bottle) = try threeEntrySession()
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)
        #expect(throws: EditModeError.invalid) { try model.addPart(.solids) }
        let logged = try store.changeLog().count

        let solids = try #require(model.newSolidsModel())
        #expect(solids.when == bottle.startedAt.addingTimeInterval(1))
        #expect(!solids.canSave)
        #expect(try store.changeLog().count == logged)

        solids.foodsText = "pear, oat"
        solids.amount = .taste
        try solids.log()
        model.reload()

        let part = try #require(model.parts.last)
        #expect(part.label == "Pear, oat · Taste")
        #expect(part.icon == .solids)
        #expect(part.detail == clockText(bottle.startedAt.addingTimeInterval(1), zone: bottle.timeZoneID))
        #expect(part.gapBefore == "0m later")
        #expect(part.canSplitBefore)
        guard case let .solids(feedID, _) = part.target else { Issue.record("not a solids part"); return }
        #expect(try feedSessions(store, baby).map(\.feedIDs).first?.last == feedID)
        // The totals stay the milk's.
        #expect(model.totals.hasSuffix("27m breast · 60 ml"))

        try model.deletePart(part.id)
        #expect(model.parts.count == 3)
    }

    /// Solids moved before the feed's first part key it, as any part does.
    @Test func solidsMovedFirstKeyTheFeed() throws {
        let (store, baby, _, left, _, _) = try threeEntrySession()
        let solids = try store.addFeedPart(.solids(foods: ["pear"], amount: .some, at: left.startedAt.addingTimeInterval(80 * 60),
                                                   note: nil),
                                           toSession: left.id, for: baby)
        let edit = SolidsEditModel(store: store, babyID: baby, feedID: solids.id)

        edit.when = left.startedAt.addingTimeInterval(-600)
        try edit.save()

        #expect(solids.feedingSessionID == nil)
        #expect(left.feedingSessionID == solids.id)
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: solids.id, unit: .millilitres)
        #expect(model.parts.map(\.target.entryID).first == solids.id)
    }

    @Test func aSleepSessionCountsItsWakes() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-6 * 3600)
        let first = try store.logSleep(from: base, to: base.addingTimeInterval(62 * 60), for: baby)
        let second = try store.addSleepPart(from: base.addingTimeInterval(80 * 60), to: base.addingTimeInterval(147 * 60),
                                            toSession: first.id, for: baby)
        let zone = first.timeZoneID

        let model = SessionSheetModel(store: store, babyID: baby, kind: .sleep, rootID: first.id, unit: .millilitres)

        #expect(model.title == "\(clockText(base, zone: zone)) – \(clockText(base.addingTimeInterval(147 * 60), zone: zone))")
        #expect(model.totals == "2h 9m · 1 wake")
        #expect(model.parts.map(\.label) == ["1h 2m", "1h 7m"])
        #expect(model.parts[0].target == .sleep(sleepID: first.id))
        #expect(model.parts[0].icon == .sleep)
        #expect(model.parts[0].detail
            == "\(clockText(base, zone: zone)) – \(clockText(base.addingTimeInterval(62 * 60), zone: zone))")
        #expect(model.parts[1].id == second.id)
        #expect(model.parts[1].gapBefore == "18m later")
        #expect(model.parts[1].canSplitBefore)
    }

    @Test func twoWakesArePlural() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-6 * 3600)
        let first = try store.logSleep(from: base, to: base.addingTimeInterval(3600), for: baby)
        try store.addSleepPart(from: base.addingTimeInterval(4000), to: base.addingTimeInterval(7600), toSession: first.id, for: baby)
        try store.addSleepPart(from: base.addingTimeInterval(8000), to: base.addingTimeInterval(11600), toSession: first.id, for: baby)

        let model = SessionSheetModel(store: store, babyID: baby, kind: .sleep, rootID: first.id, unit: .millilitres)

        #expect(model.totals == "3h · 2 wakes")
    }

    // MARK: - Split and Join

    @Test func splitBeforeRightMakesTwoSessionsAndKeepsShowingTheFirst() throws {
        let (store, baby, _, left, right, bottle) = try threeEntrySession()
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)

        try model.split(before: model.parts[1].id)

        #expect(try feedSessions(store, baby).count == 2)
        #expect(right.effectiveFeedingSessionID == right.id)
        #expect(bottle.effectiveFeedingSessionID == right.id)
        #expect(model.rootID == left.id)
        #expect(model.parts.map(\.id) == [left.components[0].id])
    }

    @Test func splitBetweenComponentsOfOneEntryIsRefused() throws {
        let (store, baby, clock) = try fixture()
        let feed = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-3600),
                                       to: clock.now.addingTimeInterval(-3000), for: baby)
        feed.components = [.breast(id: UUID(), side: .left, seconds: 300), .breast(id: UUID(), side: .right, seconds: 300)]
        try store.update(feed)
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: feed.id, unit: .millilitres)
        let logged = try store.changeLog().count

        #expect(throws: EditModeError.invalid) { try model.split(before: model.parts[1].id) }
        #expect(try store.changeLog().count == logged)
    }

    /// After Join the sheet shows the merged session, whose root is now the
    /// earlier session's (review minor #5): `rootID` follows it.
    @Test func joinMergesWithThePreviousFeedAndTheSheetFollowsTheMergedSession() throws {
        let (store, baby, _, left, right, bottle) = try threeEntrySession()
        let earlier = left.startedAt.addingTimeInterval(-3 * 3600)
        let previous = try store.logBottle(volumeML: 90, contents: .breastMilk, for: baby, at: earlier)
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)
        #expect(model.join.previous?.rootID == previous.id)

        try model.join.join()
        model.reload()   // the sheet does this on the revision the join bumps

        #expect(try feedSessions(store, baby).count == 1)
        for feed in [left, right, bottle] { #expect(feed.effectiveFeedingSessionID == previous.id) }
        #expect(model.rootID == previous.id)
        #expect(model.join.rootID == previous.id)
        #expect(model.join.previous == nil)
        #expect(model.parts.count == 4)
        #expect(model.parts[0].target == .bottle(feedID: previous.id, componentID: previous.components[0].id))
        #expect(model.parts[1].canSplitBefore)
        #expect(model.title.hasPrefix(clockText(earlier, zone: previous.timeZoneID)))

        // Split undoes it.
        try model.split(before: model.parts[1].id)
        #expect(try feedSessions(store, baby).count == 2)
        #expect(model.parts.count == 1)
    }

    /// Final review I3: a second tap on the Join row, accidental or because
    /// nothing else visibly changed, must not join the session before that
    /// too. The row stays hidden until the sheet opens again.
    @Test func afterAJoinTheRowIsHiddenUntilTheSheetReopens() throws {
        let (store, baby, clock) = try fixture()
        let at = { (hours: Double) in clock.now.addingTimeInterval(-hours * 3_600) }
        let first = try store.logBreast(side: .left, from: at(9), to: at(9).addingTimeInterval(600), for: baby)
        let second = try store.logBreast(side: .left, from: at(6), to: at(6).addingTimeInterval(600), for: baby)
        let third = try store.logBreast(side: .left, from: at(3), to: at(3).addingTimeInterval(600), for: baby)
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: third.id)
        #expect(model.join.previous?.rootID == second.id)

        try model.join.join()
        model.reload()
        try model.join.join()   // the second tap
        model.reload()

        #expect(model.join.previous == nil)
        #expect(try feedSessions(store, baby).map(\.feedIDs) == [[first.id], [second.id, third.id]])
        let reopened = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: model.rootID)
        #expect(reopened.join.previous?.rootID == first.id)
    }

    /// Review Focus 3: with no earlier same-kind session the row is absent.
    @Test func theJoinRowIsAbsentWithNoEarlierSession() throws {
        let (store, baby, _, left, _, _) = try threeEntrySession()
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)

        #expect(model.join.previous == nil)
    }

    // MARK: - Add part

    @Test func addBottleStartsAtTheSessionsEndInThisSession() throws {
        let (store, baby, _, left, _, bottle) = try threeEntrySession()
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)

        let target = try model.addPart(.bottle)

        guard case let .bottle(feedID, componentID) = target else {
            Issue.record("expected a bottle target, got \(target)")
            return
        }
        guard case .feed(let added)? = try store.entry(id: feedID, for: baby) else {
            Issue.record("the added part is not a feed")
            return
        }
        // Ruling H-R17: a second after the zero-length bottle that ends the
        // session, so it never ties with it on start.
        #expect(added.startedAt == bottle.startedAt.addingTimeInterval(1))
        #expect(added.effectiveFeedingSessionID == left.id)
        #expect(added.components.map(\.id) == [componentID])
        #expect(added.totalBottleVolumeML > 0)
        #expect(try feedSessions(store, baby).count == 1)
        #expect(model.parts.count == 4)
        #expect(model.parts.last?.target == target)
        #expect(model.parts.last?.gapBefore == "0m later")
        #expect(model.parts.last?.canSplitBefore == true)
    }

    /// Ruling H-R17: a tie on start is broken by id, so one run could pass
    /// by luck; eight fresh sessions make that one chance in 256.
    @Test func anyPartAddedAfterAClosingBottleListsLastAndSplitsAlone() throws {
        for addition in [AddPartKind.left, .right, .bottle, .left, .right, .bottle, .left, .bottle] {
            let (store, baby, _, left, _, bottle) = try threeEntrySession()
            let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)

            let target = try model.addPart(addition)

            #expect(model.parts.count == 4)
            #expect(model.parts.last?.target == target)
            #expect(model.parts[2].id == bottle.components[0].id)
            let before = model.parts.prefix(3).map(\.id)
            try model.split(before: model.parts[3].id)
            #expect(try feedSessions(store, baby).count == 2)
            #expect(model.parts.map(\.id) == before)
        }
    }

    /// Ruling H-R18: an added breast side ends no later than now, and waits
    /// until it can last the store's 30-second minimum.
    @Test func addedSidesNeverEndInTheFutureAndWaitForThirtySeconds() throws {
        let (store, baby, clock) = try fixture()
        let start = clock.now.addingTimeInterval(-620)
        let first = try store.logBreast(side: .left, from: start, to: clock.now.addingTimeInterval(-20), for: baby)
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: first.id, unit: .millilitres)
        let logged = try store.changeLog().count

        #expect(!model.canAdd(.left))
        #expect(!model.canAdd(.right))
        #expect(model.canAdd(.bottle))
        #expect(!model.canAdd(.sleep))
        #expect(throws: EditModeError.invalid) { try model.addPart(.right) }
        #expect(try store.changeLog().count == logged)

        clock.advance(by: 10)
        #expect(model.canAdd(.left))
        let target = try model.addPart(.left)

        guard case let .breast(feedID, _) = target, case .feed(let added)? = try store.entry(id: feedID, for: baby) else {
            Issue.record("expected a breast part, got \(target)")
            return
        }
        #expect(added.startedAt == first.endedAt)
        #expect(added.endedAt == clock.now)
        #expect(added.components.first?.durationSeconds == 30)
        #expect(model.parts.last?.detail
            == "\(clockText(added.startedAt, zone: added.timeZoneID))–\(clockText(added.endedAt!, zone: added.timeZoneID))")
    }

    /// Final review M6: a part running on from the session started after
    /// its end, so an added part stops where that one began rather than
    /// running under it and being counted twice when it stops.
    @Test func anAddedSideStopsWhereARunningContinuationBegan() throws {
        let (store, baby, clock) = try fixture()
        let first = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-900),
                                        to: clock.now.addingTimeInterval(-300), for: baby)
        let running = try store.startPart(.breast(.left), for: baby)
        #expect(running.sessionID == first.id)
        clock.advance(by: 240)
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: first.id, unit: .millilitres)

        guard case let .breast(feedID, _) = try model.addPart(.right),
              case .feed(let added)? = try store.entry(id: feedID, for: baby) else {
            Issue.record("expected a breast part")
            return
        }
        #expect(added.endedAt == running.startedAt)
        #expect(added.components.first?.durationSeconds == 300)
    }

    @Test func addingASideWaitsWhenAContinuationBeganRightAfterTheSession() throws {
        let (store, baby, clock) = try fixture()
        let first = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-900),
                                        to: clock.now.addingTimeInterval(-10), for: baby)
        _ = try store.startPart(.breast(.right), for: baby)
        clock.advance(by: 600)
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: first.id, unit: .millilitres)

        #expect(!model.canAdd(.left))
        #expect(model.canAdd(.bottle))
    }

    @Test func anAddedSideStopsAtNowWhenTheSessionEndedUnderTenMinutesAgo() throws {
        let (store, baby, clock) = try fixture()
        let first = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-900),
                                        to: clock.now.addingTimeInterval(-180), for: baby)
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: first.id, unit: .millilitres)

        guard case let .breast(feedID, _) = try model.addPart(.right),
              case .feed(let added)? = try store.entry(id: feedID, for: baby) else {
            Issue.record("expected a breast part")
            return
        }
        #expect(added.endedAt == clock.now)
        #expect(added.components.first?.durationSeconds == 180)
    }

    /// A bottle that ends the session right now leaves no second for another
    /// bottle after it without going into the future.
    @Test func aBottleWaitsWhenTheSessionEndsNow() throws {
        let (store, baby, clock) = try fixture()
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: clock.now)
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: bottle.id, unit: .millilitres)

        #expect(!model.canAdd(.bottle))
        #expect(throws: EditModeError.invalid) { try model.addPart(.bottle) }

        clock.advance(by: 1)
        #expect(model.canAdd(.bottle))
        _ = try model.addPart(.bottle)
        #expect(model.parts.count == 2)
    }

    /// Ruling H-R18 for sleep: a minute's minimum, and never past now.
    @Test func anAddedSleepWaitsForAMinuteAndStopsAtNow() throws {
        let (store, baby, clock) = try fixture()
        let first = try store.logSleep(from: clock.now.addingTimeInterval(-3600), to: clock.now.addingTimeInterval(-40),
                                       for: baby)
        let model = SessionSheetModel(store: store, babyID: baby, kind: .sleep, rootID: first.id, unit: .millilitres)

        #expect(!model.canAdd(.sleep))
        #expect(throws: EditModeError.invalid) { try model.addPart(.sleep) }

        clock.advance(by: 20)
        guard case let .sleep(sleepID) = try model.addPart(.sleep),
              case .sleep(let added)? = try store.entry(id: sleepID, for: baby) else {
            Issue.record("expected a sleep part")
            return
        }
        #expect(added.startedAt == first.endedAt)
        #expect(added.endedAt == clock.now)
    }

    @Test func addLeftStartsAtTheSessionsEndAndLastsUpToTenMinutes() throws {
        let (store, baby, clock, left, right, _) = try threeEntrySession()
        // Drop the bottle so the session ends with Right, well in the past.
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)
        try model.deletePart(model.parts[2].id)

        let target = try model.addPart(.left)

        guard case let .breast(feedID, _) = target, case .feed(let added)? = try store.entry(id: feedID, for: baby) else {
            Issue.record("expected a breast part, got \(target)")
            return
        }
        #expect(added.startedAt == right.endedAt)
        #expect(added.endedAt == right.endedAt?.addingTimeInterval(600))
        #expect(added.components.first?.breastSide == .left)
        #expect(added.effectiveFeedingSessionID == left.id)
        #expect(added.endedAt! <= clock.now)
    }

    @Test func addSleepStartsAtTheSessionsEnd() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-4 * 3600)
        let first = try store.logSleep(from: base, to: base.addingTimeInterval(3600), for: baby)
        let model = SessionSheetModel(store: store, babyID: baby, kind: .sleep, rootID: first.id, unit: .millilitres)

        let target = try model.addPart(.sleep)

        guard case let .sleep(sleepID) = target, case .sleep(let added)? = try store.entry(id: sleepID, for: baby) else {
            Issue.record("expected a sleep part, got \(target)")
            return
        }
        #expect(added.startedAt == first.endedAt)
        #expect(added.effectiveSleepSessionID == first.id)
        #expect(model.parts.count == 2)
        #expect(model.totals.hasSuffix("· 1 wake"))
    }

    /// The part editor pushed after Add part edits that part.
    @Test func theEditorModelEditsTheAddedPart() throws {
        let (store, baby, _, left, _, _) = try threeEntrySession()
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)

        let target = try model.addPart(.right)
        let editor = model.editorModel(for: target)

        #expect(editor.target == target)
        #expect(!editor.isGone)
        #expect(editor.side == .right)
    }

    @Test func aKindTheSessionDoesNotTakeIsRefused() throws {
        let (store, baby, _, left, _, _) = try threeEntrySession()
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)
        let logged = try store.changeLog().count

        #expect(throws: EditModeError.invalid) { try model.addPart(.sleep) }
        #expect(try store.changeLog().count == logged)
    }

    // MARK: - Deleting parts (Review Focus 2)

    @Test func deletingTheMiddlePartKeepsOneSession() throws {
        let (store, baby, _, left, right, bottle) = try threeEntrySession()
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)

        try model.deletePart(model.parts[1].id)

        #expect(right.isTombstoned)
        #expect(try feedSessions(store, baby).count == 1)
        #expect(bottle.effectiveFeedingSessionID == left.id)
        #expect(model.rootID == left.id)
        #expect(model.parts.map(\.id) == [left.components[0].id, bottle.components[0].id])
        #expect(model.parts[1].gapBefore == "1h 5m later")
    }

    @Test func deletingTheFirstPartMakesTheNextTheRoot() throws {
        let (store, baby, _, left, right, bottle) = try threeEntrySession()
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)

        try model.deletePart(model.parts[0].id)

        #expect(left.isTombstoned)
        #expect(!model.isGone)
        #expect(model.rootID == right.id)
        #expect(model.join.rootID == right.id)
        #expect(right.feedingSessionID == nil)
        #expect(bottle.effectiveFeedingSessionID == right.id)
        #expect(try feedSessions(store, baby).map(\.id) == [right.id])
        #expect(model.parts.map(\.id) == [right.components[0].id, bottle.components[0].id])
        #expect(model.parts[0].gapBefore == nil)
    }

    @Test func deletingTheLastRemainingPartClosesTheSheet() throws {
        let (store, baby, clock) = try fixture()
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: clock.now.addingTimeInterval(-600))
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: bottle.id, unit: .millilitres)

        try model.deletePart(model.parts[0].id)

        #expect(bottle.isTombstoned)
        #expect(model.isGone)
        #expect(try feedSessions(store, baby).isEmpty)
    }

    @Test func deletingOneComponentOfAnOldComboKeepsTheEntry() throws {
        let (store, baby, clock) = try fixture()
        let feed = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-3600),
                                       to: clock.now.addingTimeInterval(-3000), for: baby)
        let keptID = UUID()
        feed.components = [.breast(id: keptID, side: .left, seconds: 300), .breast(id: UUID(), side: .right, seconds: 300)]
        try store.update(feed)
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: feed.id, unit: .millilitres)

        try model.deletePart(model.parts[1].id)

        #expect(!feed.isTombstoned)
        #expect(feed.components.map(\.id) == [keptID])
        #expect(model.parts.map(\.id) == [keptID])
    }

    @Test func deleteSessionTombstonesEveryMember() throws {
        let (store, baby, _, left, right, bottle) = try threeEntrySession()
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)

        try model.deleteSession()

        #expect(left.isTombstoned && right.isTombstoned && bottle.isTombstoned)
        #expect(model.isGone)
        #expect(try feedSessions(store, baby).isEmpty)
    }

    @Test func deleteSleepSessionTombstonesEveryMember() throws {
        let (store, baby, clock) = try fixture()
        let base = clock.now.addingTimeInterval(-6 * 3600)
        let first = try store.logSleep(from: base, to: base.addingTimeInterval(3600), for: baby)
        let second = try store.addSleepPart(from: base.addingTimeInterval(4000), to: base.addingTimeInterval(7600),
                                            toSession: first.id, for: baby)
        let model = SessionSheetModel(store: store, babyID: baby, kind: .sleep, rootID: first.id, unit: .millilitres)

        try model.deleteSession()

        #expect(first.isTombstoned && second.isTombstoned)
        #expect(model.isGone)
    }

    // MARK: - The other phone (Review Focus 5)

    @Test func aRemoteDeleteThenReloadSetsIsGone() throws {
        let (store, baby, _, left, _, _) = try threeEntrySession()
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)

        try store.deleteSession(.feed, rootID: left.id, for: baby)
        #expect(!model.isGone)
        model.reload()

        #expect(model.isGone)
        #expect(model.parts.isEmpty)
    }

    /// An older phone tombstones the first part without relinking the rest:
    /// the session stays keyed by the deleted entry, and the sheet stays on it.
    @Test func aRemoteTombstoneOfTheRootAloneKeepsTheRest() throws {
        let (store, baby, _, left, right, bottle) = try threeEntrySession()
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)

        try store.softDelete(left)
        model.reload()

        #expect(!model.isGone)
        #expect(model.parts.map(\.id) == [right.components[0].id, bottle.components[0].id])
    }

    @Test func aRemoteEditShowsOnReload() throws {
        let (store, baby, _, left, _, bottle) = try threeEntrySession()
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)
        let componentID = bottle.components[0].id

        bottle.components = [.bottle(id: componentID, volumeML: 90, contents: .breastMilk,
                                     startedAt: bottle.startedAt)]
        try store.update(bottle)
        model.reload()

        #expect(model.parts[2].label == "Breast milk 90 ml")
        #expect(model.totals == "1h 17m · 27m breast · 90 ml")
    }

    @Test func openingOnASessionAlreadyGoneIsGone() throws {
        let (store, baby, _) = try fixture()
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: UUID(), unit: .millilitres)

        #expect(model.isGone)
        #expect(model.parts.isEmpty)
    }

    // MARK: - Read-only (H-R6)

    @Test func aViewerCannotEdit() throws {
        let (store, baby, clock) = try fixture()
        let family = RealmID.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID())
        try store.setActiveRealm(family)
        let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: clock.now.addingTimeInterval(-600))
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: bottle.id, unit: .millilitres)
        #expect(model.canEdit)

        try store.setWritePermission(.viewer, for: family)
        model.reload()

        #expect(!model.canEdit)
        #expect(!model.isGone)
        #expect(throws: WritePermissionError.readOnlyCaregiver) { try model.deleteSession() }
        #expect(!bottle.isTombstoned)
    }

    // MARK: - The other phone's Join and Split, and a chain of edits (final review M10)

    @Test func aRemoteJoinWhileOpenFollowsTheMergedSession() throws {
        let (store, baby, _, left, right, bottle) = try threeEntrySession()
        let earlier = try store.logBottle(volumeML: 90, contents: .breastMilk, for: baby,
                                          at: left.startedAt.addingTimeInterval(-3 * 3600))
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)

        try store.combineFeeds([earlier.id, left.id], for: baby)   // the other phone
        model.reload()

        #expect(!model.isGone)
        #expect(model.rootID == earlier.id)
        #expect(model.parts.map(\.target.entryID) == [earlier.id, left.id, right.id, bottle.id])
    }

    @Test func aRemoteSplitWhileOpenKeepsShowingTheEarlierHalf() throws {
        let (store, baby, _, left, right, bottle) = try threeEntrySession()
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)

        try store.separateFeed(at: right.id, for: baby)   // the other phone
        model.reload()

        #expect(model.rootID == left.id)
        #expect(model.parts.map(\.target.entryID) == [left.id])
        #expect(try feedSessions(store, baby).map(\.feedIDs) == [[left.id], [right.id, bottle.id]])
    }

    /// Edit a part's time, Split before it, Join it back from its own
    /// session, then delete the first part: the sheet follows each step and
    /// ends on the promoted session.
    @Test func editSplitJoinThenDeleteTheFirstPart() throws {
        let (store, baby, _, left, right, bottle) = try threeEntrySession()
        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: left.id, unit: .millilitres)
        let rightPart = try #require(model.parts.first { $0.target.entryID == right.id })

        let editor = model.editorModel(for: rightPart.target)
        editor.start = right.startedAt.addingTimeInterval(60)
        #expect(try editor.save())
        model.reload()
        try model.split(before: rightPart.id)
        #expect(model.parts.map(\.target.entryID) == [left.id])

        let later = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: right.id, unit: .millilitres)
        #expect(later.join.previous?.rootID == left.id)
        try later.join.join()
        model.reload()
        #expect(model.parts.map(\.target.entryID) == [left.id, right.id, bottle.id])

        try model.deletePart(model.parts[0].id)

        #expect(left.isTombstoned)
        #expect(model.rootID == right.id)
        #expect(model.parts.map(\.target.entryID) == [right.id, bottle.id])
        #expect(try feedSessions(store, baby).map(\.feedIDs) == [[right.id, bottle.id]])
        #expect(right.feedingSessionID == nil && bottle.feedingSessionID == right.id)
    }

    // MARK: - A session id that is also a member's id (final review I2)

    @Test func aSessionIdThatIsAlsoAMembersShowsAndDeletesThatSession() throws {
        let (store, baby, clock) = try fixture()
        let (z, a, b, x, bottle) = try collidingOrphan(store, baby, clock)

        let model = SessionSheetModel(store: store, babyID: baby, kind: .feed, rootID: b.id)

        #expect(model.parts.map(\.target.entryID) == [x.id, bottle.id])
        #expect(model.join.previous?.rootID == a.id)
        try model.deleteSession()
        #expect(x.isTombstoned && bottle.isTombstoned)
        #expect(![z, a, b].contains { $0.isTombstoned })
        #expect(model.isGone)
    }
}
