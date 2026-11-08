import Testing
import Foundation
import BabyCore
@testable import BabyStore
import BabyUI
@testable import HistoryFeature

/// Noon local, fixed on a June date so a two-hour `advance(by:)` never
/// crosses local midnight. A raw `timeIntervalSince1970` landed at 23:13
/// local here — 47 minutes before midnight — which split "today" across
/// two days and failed a correct implementation.
@MainActor
private func noonLocal() -> Date {
    localTime(12, 0)
}

/// A local wall-clock time on the fixture's June day, `dayOffset` days away.
@MainActor
private func localTime(_ hour: Int, _ minute: Int, dayOffset: Int = 0) -> Date {
    var parts = DateComponents()
    parts.year = 2026; parts.month = 6; parts.day = 15 + dayOffset
    parts.hour = hour; parts.minute = minute
    return Calendar.current.date(from: parts)!
}

@MainActor
private func makeStore(at now: Date? = nil) throws -> (BabyStore, UUID, TestDateProvider) {
    let clock = TestDateProvider(now ?? noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    store.breakLimits = { .standard }
    // Born well before the fixture day, so a growth check-in is not refused
    // as measured before birth.
    let baby = try store.addBaby(
        name: "Mira", birthDate: localTime(12, 0, dayOffset: -90), referenceSex: .girls, expectedDueDate: nil
    )
    return (store, baby.id, clock)
}

@MainActor
private func clockText(_ date: Date, zone: String = TimeZone.current.identifier) -> String {
    HistoryViewModel.clockTime(date, zone: zone)
}

@MainActor
private func dayKey(_ date: Date) -> DayKey {
    DayBucket.key(for: date, timeZoneID: TimeZone.current.identifier)
}

// MARK: - A2 rows

@MainActor
@Test func aLeftRightBottleSessionIsOneRowWithChipsAndItsStartAboveItsEnd() throws {
    let (store, baby, _) = try makeStore()
    let left = try store.logBreast(side: .left, from: localTime(10, 23), to: localTime(10, 35), for: baby)
    _ = try store.logBreast(side: .right, from: localTime(10, 38), to: localTime(10, 53), for: baby)
    _ = try store.addFeedPart(.bottle(volumeML: 60, contents: .formula, at: localTime(11, 48)), toSession: left.id, for: baby)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)

    let rows = model.days[0].rows
    #expect(rows.count == 1)
    let row = try #require(rows.first)
    #expect(row.tracker == .feed)
    #expect(row.chips.map(\.text) == ["L 12m", "R 15m", "60 ml"])
    #expect(row.chips.map(\.id) == [0, 1, 2])
    #expect(row.chips.map(\.icon) == [nil, nil, .bottle])
    #expect(row.start == clockText(localTime(10, 23)))
    #expect(row.end == clockText(localTime(11, 48)))
    #expect(row.text == nil)
    #expect(!row.isRunning)
    #expect(row.target == .session(.feed, rootID: left.id))
    #expect(row.deleteConfirmation)
}

@MainActor
@Test func aSleepSessionReadsItsAsleepTotalAndItsWakes() throws {
    let (store, baby, _) = try makeStore()
    let first = try store.logSleep(from: localTime(8, 40), to: localTime(9, 45), for: baby)
    _ = try store.addSleepPart(from: localTime(9, 49), to: localTime(10, 53), toSession: first.id, for: baby)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)

    let row = try #require(model.days[0].rows.first)
    #expect(model.days[0].rows.count == 1)
    #expect(row.tracker == .sleep)
    #expect(row.text == "2h 9m · 1 wake")
    #expect(row.chips.isEmpty)
    #expect(row.start == clockText(localTime(8, 40)))
    #expect(row.end == clockText(localTime(10, 53)))
    #expect(row.target == .session(.sleep, rootID: first.id))
    #expect(row.deleteConfirmation)
}

/// R14, carried over: a three-part sleep is still one row; its members are
/// now listed by the session sheet rather than expanded in the list.
@MainActor
@Test func aThreePartSleepIsOneRowWithTwoWakes() throws {
    let (store, babyID, clock) = try makeStore()
    let a = try store.logSleep(from: clock.now, to: clock.now.addingTimeInterval(1_800), for: babyID)
    clock.advance(by: 5_000)
    let b = try store.logSleep(from: clock.now, to: clock.now.addingTimeInterval(1_800), for: babyID)
    clock.advance(by: 5_000)
    let c = try store.logSleep(from: clock.now, to: clock.now.addingTimeInterval(1_800), for: babyID)
    try store.combineSleeps([a.id, b.id, c.id], for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)

    let sleepRows = model.days[0].rows.filter { $0.tracker == .sleep }
    #expect(sleepRows.count == 1)
    let row = try #require(sleepRows.first)
    #expect(row.text == "1h 30m · 2 wakes")
    #expect(row.end != nil)
    #expect(row.target == .session(.sleep, rootID: a.id))
}

@MainActor
@Test func singleEntriesShowTheirValueWithNoTitleWord() throws {
    let (store, baby, _) = try makeStore()
    let diaper = try store.logDiaper(.wet, for: baby, at: localTime(8, 15))
    let med = try store.logSupplement(name: "Vitamin D", for: baby, at: localTime(7, 20))
    let growth = try store.logMeasurement(
        for: baby, at: localTime(9, 0), timeZoneID: TimeZone.current.identifier,
        values: MeasurementValues(weightKG: 4.2, lengthCM: 54, lengthPosition: .lying)
    )

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)
    let rows = model.days[0].rows

    #expect(rows.map(\.tracker) == [.growth, .diaper, .supplement])
    let growthRow = rows[0], diaperRow = rows[1], medRow = rows[2]
    #expect(diaperRow.text == "Wet")
    #expect(diaperRow.start == clockText(localTime(8, 15)))
    #expect(diaperRow.end == nil)
    #expect(diaperRow.target == .entry(.diaper(diaper.id)))
    #expect(!diaperRow.deleteConfirmation)
    #expect(medRow.text == "Vitamin D")
    #expect(medRow.target == .entry(.supplement(med.id)))
    // The number in the reader's locale ("4,2 kg" in German), as on Growth.
    let weight = 4.2.formatted(.number.precision(.significantDigits(1...6)))
    #expect(growthRow.text == "\(weight) kg · 54 cm")
    #expect(growthRow.start == clockText(localTime(9, 0)))
    #expect(growthRow.target == .entry(.measurement(growth.id)))
    #expect(!growthRow.deleteConfirmation)
}

@MainActor
@Test func rowsRouteSinglePartsAsEntriesAndSolidsNeverAsASession() throws {
    let (store, baby, _) = try makeStore()
    let solids = try store.logSolids(foods: ["pear"], amount: .some, for: baby, at: localTime(7, 0))
    let bottle = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: localTime(9, 0))
    let sleep = try store.logSleep(from: localTime(10, 0), to: localTime(10, 40), for: baby)
    // An older one-entry feed holding two parts (H-R2): its parts are listed
    // by the session sheet, not a single-part editor.
    let combo = FeedEntry(babyID: baby, kind: .breast, startedAt: localTime(11, 0), timeZoneID: TimeZone.current.identifier)
    combo.components = [
        .breast(id: UUID(), side: .left, seconds: 600),
        .bottle(id: UUID(), volumeML: 30, contents: .formula),
    ]
    combo.endedAt = localTime(11, 10)
    store.context.insert(combo)
    try store.record(.create, on: combo)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)
    let byID = Dictionary(uniqueKeysWithValues: model.days[0].rows.map { ($0.id, $0) })

    #expect(byID[solids.id]?.target == .entry(.feed(solids.id)))
    #expect(byID[solids.id]?.deleteConfirmation == false)
    #expect(byID[bottle.id]?.target == .entry(.feed(bottle.id)))
    #expect(byID[sleep.id]?.target == .entry(.sleep(sleep.id)))
    #expect(byID[sleep.id]?.text == "40m")
    #expect(byID[combo.id]?.target == .session(.feed, rootID: combo.id))
    #expect(byID[combo.id]?.chips.map(\.text) == ["L 10m", "30 ml"])
}

// MARK: - Running entries

@MainActor
@Test func aRunningFeedIsTheFirstRowOfTodayAndEndsNow() throws {
    let (store, baby, clock) = try makeStore()
    _ = try store.logDiaper(.wet, for: baby, at: localTime(11, 50))
    let timer = try store.startPart(.breast(.left), for: baby)
    clock.advance(by: 300)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)

    let rows = model.days[0].rows
    #expect(rows.count == 2)
    let running = try #require(rows.first)
    #expect(running.isRunning)
    #expect(running.tracker == .feed)
    #expect(running.end == "now")
    #expect(running.start == clockText(timer.startedAt, zone: timer.timeZoneID))
    #expect(running.target == .runningTimer(timer.id))
    #expect(running.runningSince == timer.currentSideStartedAt)
    #expect(rows[1].tracker == .diaper)
    #expect(rows[1].runningSince == nil)
    // A running feed is not a finished session yet.
    #expect(model.days[0].totals.feeds == 0)
}

@MainActor
@Test func aRunningSleepThatBeganYesterdayStillTopsToday() throws {
    let (store, baby, clock) = try makeStore(at: localTime(23, 30, dayOffset: -1))
    let timer = try store.startSleep(for: baby)
    clock.advance(by: 3_600 * 3)
    _ = try store.logDiaper(.wet, for: baby)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 2)

    #expect(model.days[0].title == "Today")
    let running = try #require(model.days[0].rows.first)
    #expect(running.isRunning)
    #expect(running.tracker == .sleep)
    #expect(running.target == .runningTimer(timer.id))
    #expect(running.runningSince == timer.startedAt)
    #expect(model.days.count == 1)
}

/// H-R20: the live duration counts from the same moment as Home's hero —
/// the running part's start, not the timer's — so the list and Home agree.
@MainActor
@Test func aRunningRowCountsFromTheCurrentPartLikeHomesHero() throws {
    let (store, baby, clock) = try makeStore()
    // A timer that ran a left side before the right one now running, as an
    // older phone or the running-timer editor can leave it.
    let timer = try store.startPart(.breast(.right), for: baby)
    timer.startedAt = clock.now.addingTimeInterval(-600)
    timer.completedComponents = [.breast(id: UUID(), side: .left, seconds: 300)]
    timer.currentSideStartedAt = clock.now.addingTimeInterval(-240)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)
    var running = try #require(model.days[0].rows.first)
    #expect(running.runningSince == clock.now.addingTimeInterval(-240))
    #expect(running.start == clockText(clock.now.addingTimeInterval(-600), zone: timer.timeZoneID))
    #expect(running.chips.map(\.text) == ["L 5m"])

    // A running bottle counts from the bottle's own start.
    let bottle = try store.startPart(.bottle(.formula), for: baby)
    clock.advance(by: 90)
    try model.load(dayCount: 1)
    running = try #require(model.days[0].rows.first { $0.isRunning })
    #expect(running.runningSince == bottle.currentBottleStartedAt)
}

@MainActor
@Test func aFilterHidesARunningRowOfAnotherKind() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.startPart(.breast(.left), for: baby)
    _ = try store.logDiaper(.wet, for: baby)

    let model = HistoryViewModel(store: store, babyID: baby)
    model.filter = [.diaper]
    try model.load(dayCount: 1)

    #expect(model.days[0].rows.map(\.tracker) == [.diaper])
}

// MARK: - Midnight (Review Focus 1)

@MainActor
@Test func aSessionAcrossMidnightIsShownOnceOnItsFirstDayEndingAfterMidnight() throws {
    let (store, baby, clock) = try makeStore(at: localTime(0, 30))
    let left = try store.logBreast(
        side: .left, from: localTime(23, 40, dayOffset: -1), to: localTime(23, 55, dayOffset: -1), for: baby
    )
    // 15 minutes after the left side: within the break limit, so it joins.
    _ = try store.logBreast(side: .right, from: localTime(0, 10), to: localTime(0, 25), for: baby)
    #expect(try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture).count == 1)
    clock.advance(by: 3_600 * 7.5)
    _ = try store.logDiaper(.wet, for: baby)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 2)

    let today = try #require(model.days.first { $0.id == dayKey(localTime(8, 0)) })
    let firstDay = try #require(model.days.first { $0.id == dayKey(left.startedAt) })
    #expect(today.rows.filter { $0.tracker == .feed }.isEmpty)
    #expect(today.totals.feeds == 0)
    let feedRows = firstDay.rows.filter { $0.tracker == .feed }
    #expect(feedRows.count == 1)
    #expect(feedRows.first?.start == clockText(localTime(23, 40, dayOffset: -1)))
    #expect(feedRows.first?.end == clockText(localTime(0, 25)))
    #expect(feedRows.first?.chips.map(\.text) == ["L 15m", "R 15m"])
    #expect(firstDay.totals.feeds == 1)
}

/// A session belongs to the day its first part started. A sleep joined across
/// midnight is one sleep, totalled in full in the header of the day it began
/// — not split between the two days by each part's own start.
@MainActor
@Test func aSleepJoinedAcrossMidnightIsTotalledOnTheDayItBegan() throws {
    let (store, babyID, clock) = try makeStore()
    let midnight = Calendar.current.startOfDay(for: clock.now)
    let evening = try store.logSleep(
        from: midnight.addingTimeInterval(-3_600), to: midnight.addingTimeInterval(-600), for: babyID
    )
    let night = try store.logSleep(
        from: midnight, to: midnight.addingTimeInterval(2_400), for: babyID
    )
    if try store.sleepSessions(for: babyID, from: .distantPast, to: .distantFuture).count > 1 {
        try store.combineSleeps([evening.id, night.id], for: babyID)
    }

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 2)

    let began = try #require(model.days.first { $0.id == dayKey(midnight.addingTimeInterval(-1)) })
    #expect(began.totals.sleep == "1h 30m")
    #expect(began.rows.filter { $0.tracker == .sleep }.count == 1)
    #expect(began.rows.first { $0.tracker == .sleep }?.end == clockText(midnight.addingTimeInterval(2_400)))
    #expect(model.days.first { $0.id == dayKey(midnight) } == nil)
}

/// The manual join straddling midnight, from the old screen: the second
/// bottle is not shown again on the later day.
@MainActor
@Test func aJoinedBottleAfterMidnightStaysOnTheFirstFeedsDay() throws {
    let (store, babyID, clock) = try makeStore(at: localTime(23, 55, dayOffset: -1))
    store.breakLimits = { BreakLimits(feed: 60, sleep: 900) }
    let first = try store.logBottle(volumeML: 20, contents: .formula, for: babyID)
    clock.advance(by: 600)
    let second = try store.logBottle(volumeML: 50, contents: .formula, for: babyID)
    try store.combineFeeds([first.id, second.id], for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 2)

    #expect(model.days.first { $0.id == dayKey(first.startedAt) }?.rows.filter { $0.tracker == .feed }.count == 1)
    #expect(model.days.first { $0.id == dayKey(second.startedAt) }?.rows.filter { $0.tracker == .feed }.count ?? 0 == 0)
}

// MARK: - Day totals

@MainActor
@Test func dayTotalsCountEachSessionOnceAndTakeSleepFromSessions() throws {
    let (store, baby, _) = try makeStore()
    let left = try store.logBreast(side: .left, from: localTime(6, 0), to: localTime(6, 10), for: baby)
    _ = try store.addFeedPart(.bottle(volumeML: 60, contents: .formula, at: localTime(6, 20)), toSession: left.id, for: baby)
    _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby, at: localTime(9, 0))
    // Home's `todayFeedCount` counts every feeding session, solids included,
    // so History's header does too: the two numbers must agree.
    _ = try store.logSolids(foods: ["pear"], amount: .some, for: baby, at: localTime(10, 0))
    let nap = try store.logSleep(from: localTime(7, 0), to: localTime(7, 30), for: baby)
    _ = try store.addSleepPart(from: localTime(7, 40), to: localTime(8, 0), toSession: nap.id, for: baby)
    _ = try store.logDiaper(.wet, for: baby, at: localTime(8, 30))
    _ = try store.logDiaper(.dirty, for: baby, at: localTime(11, 0))

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)

    #expect(model.days[0].totals == HistoryDayTotals(feeds: 3, sleep: "50m", bottleML: 150, diapers: 2))
    #expect(model.days[0].rows.filter { $0.tracker == .feed }.count == 3)
    #expect(model.days[0].rows.filter { $0.tracker == .sleep }.count == 1)
}

@MainActor
@Test func dayHeadersCarryThatDaysTotals() throws {
    let (store, babyID, clock) = try makeStore()
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: babyID)
    clock.advance(by: 3_600)
    _ = try store.logBottle(volumeML: 90, contents: .formula, for: babyID)
    clock.advance(by: 3_600)
    _ = try store.logDiaper(.wet, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)

    #expect(model.days[0].totals == HistoryDayTotals(feeds: 2, sleep: "", bottleML: 210, diapers: 1))
}

/// D6. The header describes the rows shown beneath it, not a day the user
/// cannot see: a filtered-out tracker totals nothing.
@MainActor
@Test func dayTotalsDescribeTheRowsActuallyShown() throws {
    let (store, babyID, clock) = try makeStore()
    _ = try store.logBottle(volumeML: 90, contents: .formula, for: babyID)
    _ = try store.logSleep(from: clock.now.addingTimeInterval(-3_600), to: clock.now.addingTimeInterval(-1_800), for: babyID)
    clock.advance(by: 600)
    _ = try store.logDiaper(.wet, for: babyID)
    clock.advance(by: 600)
    _ = try store.logDiaper(.dirty, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    model.filter = [.diaper]
    try model.load(dayCount: 1)

    #expect(model.days[0].rows.count == 2)
    #expect(model.days[0].totals == HistoryDayTotals(feeds: 0, sleep: "", bottleML: 0, diapers: 2))
}

/// A feed across midnight is one session on the day it started, so its
/// bottle counts there too, however late the bottle itself was given.
@MainActor
@Test func aBottleAfterMidnightCountsOnTheDayItsSessionStarted() throws {
    let (store, baby, _) = try makeStore()
    let left = try store.logBreast(side: .left, from: localTime(23, 40, dayOffset: -1), to: localTime(23, 55, dayOffset: -1), for: baby)
    _ = try store.addFeedPart(.bottle(volumeML: 70, contents: .breastMilk, at: localTime(0, 10)), toSession: left.id, for: baby)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 2)

    let yesterday = try #require(model.days.first { $0.id == dayKey(localTime(12, 0, dayOffset: -1)) })
    #expect(yesterday.totals.bottleML == 70)
    #expect(yesterday.totals.feeds == 1)
    #expect(!model.days.contains { $0.id == dayKey(noonLocal()) })
}

/// D6 for the new total: with feeds filtered out, the bottle totals nothing.
@MainActor
@Test func aFilteredOutFeedTotalsNoBottle() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby, at: localTime(9, 0))
    _ = try store.logDiaper(.wet, for: baby, at: localTime(10, 0))

    let model = HistoryViewModel(store: store, babyID: baby)
    model.filter = [.diaper]
    try model.load(dayCount: 1)

    #expect(model.days[0].totals == HistoryDayTotals(feeds: 0, sleep: "", bottleML: 0, diapers: 1))
}

/// A tombstoned bottle neither shows nor counts.
@MainActor
@Test func aDeletedBottleCountsNoVolume() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: baby, at: localTime(9, 0))
    let gone = try store.logBottle(volumeML: 80, contents: .formula, for: baby, at: localTime(11, 30))
    try store.deleteFeedPart(gone.id, for: baby)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)

    #expect(model.days[0].totals.bottleML == 120)
}

// MARK: - Total phrases (VoiceOver and the PDF)

@Test func totalPhrasesReadInHeaderOrderAndSkipZeroes() {
    let totals = HistoryDayTotals(feeds: 8, sleep: "13h 20m", bottleML: 540, diapers: 1)
    #expect(totals.phrases(volumeUnit: .millilitres) == ["8 feeds", "13h 20m sleep", "540 ml bottle", "1 diaper"])
    #expect(HistoryDayTotals(feeds: 1, sleep: "", bottleML: 0, diapers: 0).phrases(volumeUnit: .millilitres) == ["1 feed"])
    #expect(HistoryDayTotals.none.phrases(volumeUnit: .millilitres).isEmpty)
}

@Test func theBottlePhraseFollowsTheVolumeUnit() {
    let totals = HistoryDayTotals(feeds: 0, sleep: "", bottleML: 236.588, diapers: 0)
    #expect(totals.phrases(volumeUnit: .ounces) == ["8 oz bottle"])
}

// MARK: - Cumulative durations are lengths, not ages

@MainActor
@Test func aSubMinuteSleepRowShowsALengthNotJustNow() throws {
    let (store, babyID, clock) = try makeStore()
    // A 30-second nap: short, but it has a length, not an age.
    _ = try store.logSleep(from: clock.now.addingTimeInterval(-30), to: clock.now, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)

    let row = try #require(model.days[0].rows.first { $0.tracker == .sleep })
    #expect(row.text == "0m")
}

@MainActor
@Test func aDayHeaderSleepTotalNeverSaysJustNow() throws {
    let (store, babyID, clock) = try makeStore()
    // Two sub-minute sleeps: a non-zero total that still rounds to 0m.
    _ = try store.logSleep(
        from: clock.now.addingTimeInterval(-40), to: clock.now.addingTimeInterval(-20), for: babyID
    )
    _ = try store.logSleep(from: clock.now.addingTimeInterval(-15), to: clock.now, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)

    #expect(model.days[0].totals.sleep == "0m")
}

@MainActor
@Test func aRealSleepTotalStillFormatsAsHoursAndMinutes() throws {
    let (store, babyID, clock) = try makeStore()
    _ = try store.logSleep(from: clock.now.addingTimeInterval(-8_040), to: clock.now, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)

    let row = try #require(model.days[0].rows.first { $0.tracker == .sleep })
    #expect(row.text == "2h 14m")
    #expect(model.days[0].totals.sleep == "2h 14m")
}

/// D11, carried over: a day holding nothing but a zero-length sleep still has
/// its row, and its header still says something.
@MainActor
@Test func aDayOfNothingButAZeroLengthSleepStillShowsIt() throws {
    let (store, babyID, clock) = try makeStore()
    _ = try store.logSleep(from: clock.now, to: clock.now, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)

    #expect(model.days[0].rows.count == 1)
    #expect(model.days[0].rows[0].end == nil)
    #expect(model.days[0].totals.sleep == "0m")
}

// MARK: - Days and titles

@MainActor
@Test func titlesAreTodayYesterdayThenTheShortDate() throws {
    let (store, baby, clock) = try makeStore(at: localTime(9, 0, dayOffset: -3))
    _ = try store.logDiaper(.wet, for: baby)
    clock.advance(by: 86_400 * 2)
    _ = try store.logDiaper(.wet, for: baby)
    clock.advance(by: 86_400)
    _ = try store.logDiaper(.wet, for: baby)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 7)

    var style = Date.FormatStyle(date: .abbreviated, time: .omitted)
    style.timeZone = .current
    let threeDaysAgo = DayBucket.bounds(of: dayKey(localTime(9, 0, dayOffset: -3)), timeZoneID: TimeZone.current.identifier).start
    #expect(model.days.map(\.title) == ["Today", "Yesterday", threeDaysAgo.formatted(style)])
}

@MainActor
@Test func entriesGroupIntoDaysNewestFirst() throws {
    let (store, babyID, clock) = try makeStore()
    _ = try store.logDiaper(.wet, for: babyID)
    clock.advance(by: 86_400 * 2)
    _ = try store.logDiaper(.dirty, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 7)

    #expect(model.days.count >= 2)
    #expect(model.days[0].id > model.days[1].id)
}

@MainActor
@Test func rowsWithinADayAreNewestFirst() throws {
    let (store, baby, _) = try makeStore()
    _ = try store.logDiaper(.wet, for: baby, at: localTime(7, 0))
    _ = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: localTime(9, 0))
    _ = try store.logSupplement(name: "Iron", for: baby, at: localTime(8, 0))

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)

    #expect(model.days[0].rows.map(\.tracker) == [.feed, .supplement, .diaper])
}

@MainActor
@Test func emptyDaysAreOmittedRatherThanShownBlank() throws {
    let (store, babyID, clock) = try makeStore()
    _ = try store.logDiaper(.wet, for: babyID)
    clock.advance(by: 86_400 * 3)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 7)

    #expect(!model.days.isEmpty)
    #expect(model.days.allSatisfy { !$0.rows.isEmpty })
}

// MARK: - Filters and empty states

@MainActor
@Test func theDefaultFilterIncludesGrowthAndIsNotFiltered() throws {
    let (store, baby, _) = try makeStore()
    let model = HistoryViewModel(store: store, babyID: baby)

    #expect(model.filter == [.feed, .sleep, .diaper, .supplement, .growth, .vaccine, .pump])
    #expect(!model.isFiltered)
    model.filter.remove(.growth)
    #expect(model.isFiltered)
}

@MainActor
@Test func filterHidesOtherKinds() throws {
    let (store, babyID, clock) = try makeStore()
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: babyID)
    _ = try store.logMeasurement(
        for: babyID, at: clock.now, timeZoneID: TimeZone.current.identifier, values: MeasurementValues(weightKG: 4)
    )
    clock.advance(by: 600)
    _ = try store.logDiaper(.wet, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    model.filter = [.diaper]
    try model.load(dayCount: 1)

    #expect(model.isFiltered)
    #expect(model.days[0].rows.count == 1)
    #expect(model.days[0].rows[0].tracker == .diaper)

    model.filter = [.growth]
    try model.load(dayCount: 1)
    #expect(model.days[0].rows.map(\.tracker) == [.growth])
}

/// D7. "Nothing logged yet" while entries sit in the store is the app
/// telling the user their data is gone.
@MainActor
@Test func anEmptyStoreAndAnEmptyFilterSayDifferentThings() throws {
    let (store, babyID, _) = try makeStore()

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 7)
    #expect(model.emptyState == .noData)

    _ = try store.logDiaper(.wet, for: babyID)
    try model.load(dayCount: 7)
    #expect(model.emptyState == nil)

    model.filter = [.feed]
    try model.load(dayCount: 7)
    #expect(model.days.isEmpty)
    #expect(model.emptyState == .nothingMatches)

    model.filter = []
    try model.load(dayCount: 7)
    #expect(model.emptyState == .nothingMatches)
}

@MainActor
@Test func aGrowthCheckInAloneIsData() throws {
    let (store, babyID, clock) = try makeStore()
    _ = try store.logMeasurement(
        for: babyID, at: clock.now, timeZoneID: TimeZone.current.identifier, values: MeasurementValues(weightKG: 4)
    )
    let model = HistoryViewModel(store: store, babyID: babyID)
    model.filter = [.feed]
    try model.load(dayCount: 7)

    #expect(model.emptyState == .nothingMatches)
}

/// M2. Entries older than the window are not "nothing logged".
@MainActor
@Test func entriesOlderThanTheWindowAreNotReportedAsNothingLogged() throws {
    let (store, babyID, clock) = try makeStore()
    _ = try store.logDiaper(.wet, for: babyID)
    clock.advance(by: 86_400 * 40)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 30)

    #expect(model.days.isEmpty)
    #expect(model.emptyState == .nothingRecent(dayCount: 30))

    // And with a filter on as well, the filter is what the user can undo.
    model.filter = [.feed]
    try model.load(dayCount: 30)
    #expect(model.emptyState == .nothingMatches)
}

// MARK: - Permission (H-R6)

@MainActor
@Test func aViewOnlyCaregiverCannotEdit() throws {
    let (store, baby, _) = try makeStore()
    let family = RealmID.shared(canonicalOrigin: "https://example.com", serverInstanceID: UUID(), familyID: UUID())
    try store.setActiveRealm(family)
    _ = try store.logDiaper(.wet, for: baby)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)
    #expect(model.canEdit)

    try store.setWritePermission(.viewer, for: family)
    try model.load(dayCount: 1)
    #expect(!model.canEdit)
    #expect(model.days[0].rows.count == 1)
}

// MARK: - Delete

@MainActor
@Test func deletingASessionRowTombstonesEveryMember() throws {
    let (store, baby, _) = try makeStore()
    let left = try store.logBreast(side: .left, from: localTime(10, 0), to: localTime(10, 10), for: baby)
    let right = try store.logBreast(side: .right, from: localTime(10, 12), to: localTime(10, 20), for: baby)
    let bottle = try store.addFeedPart(.bottle(volumeML: 60, contents: .formula, at: localTime(10, 40)), toSession: left.id, for: baby)
    let diaper = try store.logDiaper(.wet, for: baby, at: localTime(11, 0))

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)
    let row = try #require(model.days[0].rows.first { $0.tracker == .feed })
    try model.delete(row)

    #expect([left, right, bottle].allSatisfy { $0.isTombstoned })
    #expect(!diaper.isTombstoned)
    #expect(try store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture).isEmpty)
    #expect(model.days[0].rows.map(\.tracker) == [.diaper])
}

/// Final review I2: B was joined into A while parts that continued B kept
/// B's id as their session key, so the second session's row carries an id
/// that is also a live member of the first. Swiping that row deletes the
/// parts it shows and never the other feed.
@MainActor
@Test func deletingASessionRowWhoseIdIsAlsoAnotherSessionsMemberDeletesOnlyItsParts() throws {
    let (store, baby, _) = try makeStore()
    let a = try store.logBreast(side: .left, from: localTime(7, 0), to: localTime(7, 10), for: baby)
    let b = try store.logBreast(side: .left, from: localTime(10, 0), to: localTime(10, 10), for: baby)
    try store.combineFeeds([a.id, b.id], for: baby)
    let x = try store.logBreast(side: .right, from: localTime(10, 15), to: localTime(10, 25), for: baby)
    x.feedingSessionID = b.id
    try store.context.save()
    let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: localTime(10, 30))
    #expect(bottle.feedingSessionID == b.id)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)
    let row = try #require(model.days[0].rows.first { $0.target == .session(.feed, rootID: b.id) })
    try model.delete(row)

    #expect(x.isTombstoned && bottle.isTombstoned)
    #expect(!a.isTombstoned && !b.isTombstoned)
    #expect(model.days[0].rows.map(\.target) == [.session(.feed, rootID: a.id)])
}

/// Final review M1: the confirmation holds the row as it was swiped. If the
/// other phone joined that session into an earlier one meanwhile, its id now
/// names a bigger session; confirming must not delete what was never shown.
@MainActor
@Test func aConfirmedDeleteOfASessionRowThatChangedMeanwhileDeletesNothing() throws {
    let (store, baby, _) = try makeStore()
    let earlier = try store.logBreast(side: .left, from: localTime(7, 0), to: localTime(7, 10), for: baby)
    let left = try store.logBreast(side: .left, from: localTime(10, 0), to: localTime(10, 10), for: baby)
    let right = try store.logBreast(side: .right, from: localTime(10, 12), to: localTime(10, 20), for: baby)
    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)
    let swiped = try #require(model.days[0].rows.first { $0.target == .session(.feed, rootID: left.id) })

    try store.combineFeeds([earlier.id, left.id], for: baby)   // the other phone's Join
    #expect(try !model.deleteConfirmed(swiped))

    #expect(![earlier, left, right].contains { $0.isTombstoned })
}

@MainActor
@Test func aConfirmedDeleteOfAnUnchangedSessionRowDeletesIt() throws {
    let (store, baby, _) = try makeStore()
    let left = try store.logBreast(side: .left, from: localTime(10, 0), to: localTime(10, 10), for: baby)
    let right = try store.logBreast(side: .right, from: localTime(10, 12), to: localTime(10, 20), for: baby)
    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)
    let swiped = try #require(model.days[0].rows.first)

    #expect(try model.deleteConfirmed(swiped))

    #expect(left.isTombstoned && right.isTombstoned)
}

@MainActor
@Test func deletingASleepSessionRowTombstonesEveryMember() throws {
    let (store, baby, _) = try makeStore()
    let first = try store.logSleep(from: localTime(8, 0), to: localTime(8, 30), for: baby)
    let second = try store.addSleepPart(from: localTime(8, 40), to: localTime(9, 0), toSession: first.id, for: baby)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)
    try model.delete(try #require(model.days[0].rows.first))

    #expect(first.isTombstoned && second.isTombstoned)
    #expect(model.days.isEmpty)
}

@MainActor
@Test func deletingRemovesTheRowButKeepsTheRecord() throws {
    let (store, babyID, _) = try makeStore()
    _ = try store.logDiaper(.wet, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)
    try model.delete(model.days[0].rows[0])

    #expect(model.days.allSatisfy { $0.rows.isEmpty })
    #expect(try store.changeLog().last?.operation == .delete)
}

@MainActor
@Test func deletingSingleRowsOfEveryKindRemovesJustThatEntry() throws {
    let (store, baby, _) = try makeStore()
    let bottle = try store.logBottle(volumeML: 60, contents: .formula, for: baby, at: localTime(7, 0))
    let sleep = try store.logSleep(from: localTime(8, 0), to: localTime(8, 30), for: baby)
    let med = try store.logSupplement(name: "Iron", for: baby, at: localTime(9, 0))
    let growth = try store.logMeasurement(
        for: baby, at: localTime(10, 0), timeZoneID: TimeZone.current.identifier, values: MeasurementValues(weightKG: 4)
    )
    let diaper = try store.logDiaper(.wet, for: baby, at: localTime(11, 0))

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)
    for tracker in [Tracker.feed, .sleep, .supplement, .growth] {
        try model.delete(try #require(model.days[0].rows.first { $0.tracker == tracker }))
    }

    #expect(bottle.isTombstoned && sleep.isTombstoned && med.isTombstoned && growth.isTombstoned)
    #expect(!diaper.isTombstoned)
    #expect(model.days[0].rows.map(\.tracker) == [.diaper])
}

@MainActor
@Test func aRunningRowIsNotDeletedFromTheList() throws {
    let (store, baby, _) = try makeStore()
    let timer = try store.startPart(.breast(.left), for: baby)

    let model = HistoryViewModel(store: store, babyID: baby)
    try model.load(dayCount: 1)

    #expect(throws: HistoryDeleteError.runningTimer) { try model.delete(model.days[0].rows[0]) }
    #expect(try store.activeTimer(id: timer.id) != nil)
}

// MARK: - Feed chips

/// D9/D8, carried over: a bottle shows its volume, a breast its side and
/// length, and a diaper its capitalised contents.
@MainActor
@Test func bottleBreastAndDiaperRowsReadAsOnHome() throws {
    let (store, babyID, clock) = try makeStore()
    _ = try store.logBottle(volumeML: 90, contents: .mixed, for: babyID)
    // Past the break limit, so the breast part starts its own session.
    clock.advance(by: 1_801)
    let timer = try store.startPart(.breast(.left), for: babyID)
    clock.advance(by: 720)
    _ = try store.stopFeed(timer)
    _ = try store.logDiaper(.dirty, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)

    let chips = model.days[0].rows.flatMap(\.chips).map(\.text)
    #expect(chips.contains("90 ml"))
    #expect(chips.contains("L 12m"))
    #expect(model.days[0].rows.compactMap(\.text).contains("Dirty"))
}

@MainActor
@Test func bottleChipsFollowTheVolumeUnit() throws {
    let (store, babyID, _) = try makeStore()
    _ = try store.logBottle(volumeML: 120, contents: .formula, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID, volumeUnit: .ounces)
    try model.load(dayCount: 1)

    #expect(model.days[0].rows[0].chips.map(\.text) == [Volume(millilitres: 120).formatted(as: .ounces)])
}

/// D13. A sub-minute breast part says how long it was.
@MainActor
@Test func aSubMinuteBreastChipDoesNotClaimAWholeMinute() throws {
    let (store, babyID, clock) = try makeStore()
    let timer = try store.startPart(.breast(.right), for: babyID)
    clock.advance(by: 30)
    _ = try store.stopFeed(timer)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)

    let row = try #require(model.days[0].rows.first { $0.tracker == .feed })
    #expect(row.chips.map(\.text) == ["R 30s"])
}

@MainActor
@Test func aMultiPartFeedShowsEachPartAsAChipInOrder() throws {
    let (store, babyID, _) = try makeStore()
    let feed = try store.logBottle(volumeML: 1, contents: .formula, for: babyID)
    feed.components = [
        .breast(id: UUID(), side: .left, seconds: 480),
        .breast(id: UUID(), side: .right, seconds: 360),
        .bottle(id: UUID(), volumeML: 50, contents: .breastMilk),
        .bottle(id: UUID(), volumeML: 30, contents: .formula, seconds: 135),
    ]
    try store.update(feed)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)
    let row = try #require(model.days.first?.rows.first)

    #expect(row.chips.map(\.text) == ["L 8m", "R 6m", "50 ml", "30 ml"])
    #expect(model.days.first?.totals.feeds == 1)
}

/// How long each breast took, in the order it happened, when a feed swaps
/// sides more than once.
@MainActor
@Test func eachBreastInAFeedShowsItsOwnDuration() throws {
    let (store, babyID, _) = try makeStore()
    let feed = try store.logBottle(volumeML: 1, contents: .formula, for: babyID)
    feed.components = [
        .breast(id: UUID(), side: .left, seconds: 492),
        .breast(id: UUID(), side: .right, seconds: 26),
        .breast(id: UUID(), side: .left, seconds: 180),
    ]
    try store.update(feed)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)

    // Whole minutes in the list (H-R19); seconds only under a minute.
    #expect(model.days.first?.rows.first?.chips.map(\.text) == ["L 8m", "R 26s", "L 3m"])
}

/// H-R19: a chip reads in whole minutes, never rounding up, and keeps its
/// seconds only when there is not yet a whole minute to show.
@MainActor
@Test func breastChipsReadInWholeMinutesAndSecondsOnlyUnderAMinute() throws {
    let (store, babyID, _) = try makeStore()
    let feed = try store.logBottle(volumeML: 1, contents: .formula, for: babyID)
    feed.components = [
        .breast(id: UUID(), side: .left, seconds: 530),
        .breast(id: UUID(), side: .right, seconds: 45),
        .breast(id: UUID(), side: .left, seconds: 60),
        .breast(id: UUID(), side: .right, seconds: 3_725),
    ]
    try store.update(feed)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)

    #expect(model.days.first?.rows.first?.chips.map(\.text) == ["L 8m", "R 45s", "L 1m", "R 1h 2m"])
}

@MainActor
@Test func mixedBottleStepsAreEachAChip() throws {
    let (store, babyID, _) = try makeStore()
    let feed = try store.logBottle(volumeML: 20, contents: .formula, for: babyID)
    feed.components = [
        .bottle(id: UUID(), volumeML: 20, contents: .formula),
        .bottle(id: UUID(), volumeML: 30, contents: .breastMilk),
    ]
    try store.update(feed)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)

    #expect(model.days.first?.rows.first?.chips.map(\.text) == ["20 ml", "30 ml"])
}

/// Clock text is the entry's own zone, even when the viewer's zone differs.
@MainActor
@Test func aTimedBottleShowsItsStartAndEndInTheStoredZone() throws {
    let (store, babyID, clock) = try makeStore()
    let timer = try store.startPart(.bottle(.formula), for: babyID)
    clock.advance(by: 135)
    try store.finishBottle(volumeML: 80, contents: .formula, on: timer)
    let feed = try #require(try store.stopFeed(timer))
    feed.timeZoneID = "Pacific/Honolulu"
    try store.update(feed)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)
    let row = try #require(model.days.first?.rows.first)

    #expect(row.chips.map(\.text) == ["80 ml"])
    #expect(row.start == clockText(feed.startedAt, zone: "Pacific/Honolulu"))
    #expect(row.end == clockText(try #require(feed.endedAt), zone: "Pacific/Honolulu"))
}

@MainActor
@Test func manuallyLoggedBottleDoesNotInventAnEndTime() throws {
    let (store, babyID, _) = try makeStore()
    let feed = try store.logBottle(volumeML: 60, contents: .breastMilk, for: babyID)

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)
    let row = try #require(model.days.first?.rows.first)

    #expect(row.id == feed.id)
    #expect(row.end == nil)
    #expect(row.chips.map(\.text) == ["60 ml"])
}

@MainActor
@Test func linkedFeedsAreOneRowWithEveryMembersParts() throws {
    let (store, babyID, clock) = try makeStore()
    let first = try store.logBottle(volumeML: 40, contents: .breastMilk, for: babyID)
    clock.advance(by: 1_200)
    // Within the break limit, so startPart's own session-joining links it.
    let timer = try store.startPart(.breast(.left), for: babyID)
    clock.advance(by: 120)
    _ = try #require(try store.stopFeed(timer))

    let model = HistoryViewModel(store: store, babyID: babyID)
    try model.load(dayCount: 1)

    let feedRows = model.days[0].rows.filter { $0.tracker == .feed }
    #expect(feedRows.count == 1)
    #expect(feedRows.first?.chips.map(\.text) == ["40 ml", "L 2m"])
    #expect(feedRows.first?.target == .session(.feed, rootID: first.id))
    #expect(model.days[0].totals.feeds == 1)
}
