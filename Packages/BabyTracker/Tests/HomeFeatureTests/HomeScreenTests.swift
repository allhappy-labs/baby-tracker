import Testing
import Foundation
import BabyCore
import BabyUI
@testable import BabyStore
@testable import HomeFeature

@MainActor @Suite struct HomeScreenTests {
    @Test func idleShowsAwakeHeroStatusLineStripAndSuggestedSide() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.right), for: baby).advancing(clock, by: 600))
        clock.advance(by: 3_600)
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: try store.suggestedSide(for: baby),
                                     canLog: true, now: clock.now)
        #expect(screen.hero == .awake(since: nil))
        #expect(screen.dock == .start(suggested: .left, big: .breast, tiles: [.bottle, .sleep, .diaper]))
        #expect(screen.status.map(\.tracker) == [.feed])
        #expect(screen.strip?.feedCount == 1)
    }

    @Test func feedRunningIsTheHeroAndStatusShowsSleepAndDiaper() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 600))
        _ = try store.logDiaper(.wet, for: baby)
        let left = try store.startPart(.breast(.left), for: baby)
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .right, canLog: true, now: clock.now)
        guard case .feedRunning(let icon, _, let startedAt) = screen.hero else { Issue.record("expected feed hero"); return }
        #expect(icon == .breastLeft)
        #expect(startedAt == left.startedAt)
        #expect(Set(screen.status.map(\.tracker)) == [.sleep, .diaper])
        #expect(screen.dock == .feedRunning(timerID: left.id, isBottle: false, tiles: [.diaper]))
    }

    /// Today stays on Home while a timer runs. The running part is not an
    /// entry yet, so the strip draws it live from its start.
    @Test func feedRunningKeepsTheTodayStripWithTheRunningPartLive() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 600))
        clock.advance(by: 3_600)
        let right = try store.startPart(.breast(.right), for: baby)
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .left, canLog: true, now: clock.now)
        #expect(screen.strip?.feedCount == 1)
        #expect(screen.strip?.feeds.count == 1)
        #expect(screen.strip?.runningFeedSince == right.startedAt)
        #expect(screen.strip?.runningSleepSince == nil)
    }

    /// A running sleep is drawn live by the strip, so it is not also a
    /// finished span frozen at the moment Home last loaded.
    @Test func sleepRunningKeepsTheTodayStripWithTheSleepLive() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopSleep(try store.startSleep(for: baby).advancing(clock, by: 600))
        clock.advance(by: 1_800)
        let sleep = try store.startSleep(for: baby)
        clock.advance(by: 300)
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .left, canLog: true, now: clock.now)
        #expect(screen.strip?.sleeps.count == 1)
        #expect(screen.strip?.runningSleepSince == sleep.startedAt)
        #expect(screen.strip?.runningFeedSince == nil)
    }

    @Test func theStripCarriesTodaysBottleVolume() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby)
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .left, canLog: true, now: clock.now)
        #expect(screen.strip?.volumeML == 150)
    }

    /// Six-hourly marks strictly between midnight and now; none at midnight
    /// itself, which is the bar's left edge.
    @Test func theStripMarksEverySixHoursUpToNow() {
        let dayStart = Calendar.current.startOfDay(for: Date(timeIntervalSince1970: 1_800_000_000))
        let strip = TodayStrip(dayStart: dayStart, sleeps: [], feeds: [], feedCount: 0, sleepSeconds: 0,
                               diaperCount: 0, volumeML: 0, runningSleepSince: nil, runningFeedSince: nil)
        let hour = { (h: Double) in dayStart.addingTimeInterval(h * 3_600) }
        #expect(strip.hourMarks(now: hour(5.5)) == [])
        #expect(strip.hourMarks(now: hour(6)) == [])
        #expect(strip.hourMarks(now: hour(15.2)) == [hour(6), hour(12)])
        #expect(strip.hourMarks(now: hour(23.9)) == [hour(6), hour(12), hour(18)])
    }

    /// The spring-forward day is 23 hours long: "06" is six o'clock, which is
    /// five hours after midnight.
    @Test func hourMarksFollowTheClockOnADaylightSavingDay() throws {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = try #require(TimeZone(identifier: "Europe/Kyiv"))
        let dayStart = try #require(calendar.date(from: DateComponents(year: 2027, month: 3, day: 28)))
        let strip = TodayStrip(dayStart: dayStart, sleeps: [], feeds: [], feedCount: 0, sleepSeconds: 0,
                               diaperCount: 0, volumeML: 0, runningSleepSince: nil, runningFeedSince: nil)
        let marks = strip.hourMarks(now: dayStart.addingTimeInterval(20 * 3_600), calendar: calendar)
        #expect(marks.map { calendar.component(.hour, from: $0) } == [6, 12, 18])
        #expect(marks.first == dayStart.addingTimeInterval(5 * 3_600))
    }

    @Test func sleepRunningShowsLastFeedAndDiaperInTheStatusLine() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.logBottle(volumeML: 60, contents: .formula, for: baby)
        _ = try store.logDiaper(.dirty, for: baby)
        clock.advance(by: 60)
        let sleep = try store.startSleep(for: baby)
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .left, canLog: true, now: clock.now)
        #expect(screen.hero == .sleepRunning(clockFrom: sleep.startedAt, since: sleep.startedAt))
        #expect(Set(screen.status.map(\.tracker)) == [.feed, .diaper])
    }

    @Test func afterStopTheHeroIsTheBreakWithTheParts() throws {
        let (store, baby, clock) = try fixture()
        let left = try store.startPart(.breast(.left), for: baby)
        clock.advance(by: 542)
        _ = try store.stopFeed(left)
        clock.advance(by: 40)
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .right, canLog: true, now: clock.now)
        guard case .feedBreak(_, let parts) = screen.hero else { Issue.record("expected break hero"); return }
        #expect(parts == [PartSummary(icon: .breastLeft, seconds: 542)])
        #expect(screen.dock == .start(suggested: .right, big: .breast, tiles: [.bottle, .sleep, .diaper]))
    }

    @Test func aJoinedPartShowsTheChip() throws {
        let (store, baby, clock) = try fixture()
        _ = try store.stopFeed(try store.startPart(.breast(.left), for: baby).advancing(clock, by: 300))
        _ = try store.startPart(.breast(.right), for: baby)
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .left, canLog: true, now: clock.now)
        #expect(screen.joinChip == JoinChip(kind: .feed, previousParts: [PartSummary(icon: .breastLeft, seconds: 300)]))
    }

    /// VoiceOver read the "Same sleep" chip's earlier part as "Bottle":
    /// every glyph that was not a breast side fell through to the bottle.
    @Test func aJoinedSleepChipSpeaksItsPartAsAsleep() {
        let part = PartSummary(icon: .sleep, seconds: 7)
        #expect(HomeCopy.spokenPart(part) == "Asleep, 7 seconds")
        #expect(HomeCopy.spokenPart(PartSummary(icon: .bottle, seconds: 7)) == "Bottle, 7 seconds")
    }

    /// Home shows times only: under a minute is "0m", never "just now".
    @Test func homeTimeSinceIsATimeEvenUnderAMinute() {
        let now = Date(timeIntervalSince1970: 1_800_000_000)
        #expect(HomeCopy.elapsed(since: now.addingTimeInterval(-30), now: now) == "0m")
        #expect(HomeCopy.elapsed(since: now, now: now) == "0m")
        #expect(HomeCopy.elapsed(since: now.addingTimeInterval(-61), now: now) == "1m")
        #expect(HomeCopy.elapsed(since: now.addingTimeInterval(-7_500), now: now) == "2h 5m")
    }

    /// A bare "23m" beside a moon read as a sleep 23 minutes long, so the
    /// status line says when: "23m ago".
    @Test func theStatusLineSaysItsTimesAreAgo() {
        #expect(HomeCopy.ago("23m") == "23m ago")
        #expect(HomeCopy.ago("2h 57m") == "2h 57m ago")
    }

    @Test func feedAndSleepTogetherShowFeedHeroAndSleepChip() throws {
        let (store, baby, clock) = try fixture()
        let sleep = try store.startSleep(for: baby)
        clock.advance(by: 30)
        let feed = try store.startPart(.breast(.left), for: baby)
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .right, canLog: true, now: clock.now)
        #expect(screen.hero == .feedRunning(icon: .breastLeft, title: "Left breast", startedAt: feed.startedAt))
        #expect(screen.dock == .feedRunning(timerID: feed.id, isBottle: false, tiles: [.diaper]))
        #expect(!screen.status.contains { $0.tracker == .sleep })
        #expect(screen.sleepChip == sleep.startedAt)
    }

    @Test func viewOnlyCaregiversGetNoDock() throws {
        let (store, baby, clock) = try fixture()
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .left, canLog: false, now: clock.now)
        #expect(screen.dock == .readOnly)
    }

    @Test func theFirstActionOfTheOrderIsTheBigButton() throws {
        let (store, baby, clock) = try fixture()
        let layout = HomeLayout(order: [.bottle, .breast, .solids, .diaper, .sleep, .meds, .growth])
        let screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .left,
                                     canLog: true, layout: layout, now: clock.now)
        #expect(screen.dock == .start(suggested: .left, big: .bottle, tiles: [.breast, .solids, .diaper]))
    }

    @Test func runningDocksCarryTheOrdersTiles() throws {
        let (store, baby, clock) = try fixture()
        let layout = HomeLayout(order: [.bottle, .solids, .diaper, .growth, .breast, .sleep, .meds])
        let sleep = try store.startSleep(for: baby)
        var screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .left,
                                     canLog: true, layout: layout, now: clock.now)
        #expect(screen.dock == .sleepRunning(timerID: sleep.id, tiles: [.feed, .action(.solids), .action(.diaper)]))
        let left = try store.startPart(.breast(.left), for: baby)
        screen = HomeScreen.make(snapshot: try store.homeSnapshot(for: baby), suggested: .right,
                                 canLog: true, layout: layout, now: clock.now)
        // Feed and sleep together: the feed's dock, as before.
        #expect(screen.dock == .feedRunning(timerID: left.id, isBottle: false, tiles: [.solids, .diaper, .growth]))
    }

    /// Home's parts go to the kit's `PartsRow` in the colours Home's own row
    /// used: a sleep in the sleep colour, every feed part in the feed colour.
    @Test func partSummaryMapsToTheKitPartsRow() {
        let sleep = PartSummary(icon: .sleep, seconds: 542).partsRowItem
        let left = PartSummary(icon: .breastLeft, seconds: 250).partsRowItem
        let bottle = PartSummary(icon: .bottle, seconds: 60).partsRowItem
        #expect(sleep.tint == Palette.sleep)
        #expect(left.tint == Palette.feed)
        #expect(bottle.tint == Palette.feed)
        #expect(left.text == "4:10")
        #expect(left.accessibilityLabel == HomeCopy.spokenPart(PartSummary(icon: .breastLeft, seconds: 250)))
    }
}
