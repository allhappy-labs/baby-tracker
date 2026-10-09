import Testing
import Foundation
import BabyCore
@testable import BabyStore

private func noonLocal() -> Date {
    var components = DateComponents()
    components.year = 2_025
    components.month = 6
    components.day = 12
    components.hour = 12
    return Calendar.current.date(from: components)!
}

@MainActor
private func makeTwoBabies() throws -> (BabyStore, Baby, Baby) {
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(noonLocal()))
    let mira = try store.addBaby(name: "Mira", birthDate: noonLocal(), referenceSex: .girls, expectedDueDate: nil)
    let theo = try store.addBaby(name: "Theo", birthDate: noonLocal(), referenceSex: .girls, expectedDueDate: nil)
    return (store, mira, theo)
}

// MARK: - Siri's "start the other side" decision

// `SiriStartBreastIntent` and the Quick Log widget's L/R both call
// `BabyStore.startPart`, which stops whatever part is running and joins the
// new one — never a refusal. This pins that for the shared, baby-scoped
// `fixture()` the rest of this package's store tests use.

@MainActor
@Test func aWidgetStartWhileTheOtherSideRunsStopsItAndJoins() throws {
    let (store, baby, clock) = try fixture()
    _ = try store.startPart(.breast(.left), for: baby)
    clock.advance(by: 200)
    let right = try store.startPart(.breast(.right), for: baby)   // what StartBreastIntent calls
    #expect(right.sessionID == (try store.lastFeed(for: baby))?.effectiveFeedingSessionID)
}

@MainActor
@Test func aStopFromTheLockScreenActsOnTheTimersBabyNotTheSelectedOne() throws {
    let (store, mira, theo) = try makeTwoBabies()
    let babies = try store.babies()

    // The activity was started for Mira; Theo is the one selected in the app.
    let target = ActivityTarget.resolve(
        activityBabyID: mira.id.uuidString,
        among: babies,
        fallback: theo
    )

    #expect(target?.id == mira.id)
}

@MainActor
@Test func aLegacyButtonWithNoBabyIDActsOnTheSelectedBaby() throws {
    let (store, _, theo) = try makeTwoBabies()

    // Legacy callers with no baby ID still use app selection.
    let target = ActivityTarget.resolve(
        activityBabyID: nil,
        among: try store.babies(),
        fallback: theo
    )

    #expect(target?.id == theo.id)
}

@MainActor
@Test func aConfiguredWidgetButtonKeepsItsBabyAfterAppSelectionChanges() throws {
    let (store, mira, theo) = try makeTwoBabies()

    let target = ActivityTarget.resolve(
        activityBabyID: mira.id.uuidString,
        among: try store.babies(),
        fallback: theo
    )

    #expect(target?.id == mira.id)
}

@MainActor
@Test func anActivityNamingAVanishedBabyActsOnNobody() throws {
    let (store, _, theo) = try makeTwoBabies()

    // The child was deleted while the activity was still on the Lock Screen.
    // Falling back to the selected baby here would log the feed to a sibling.
    let target = ActivityTarget.resolve(
        activityBabyID: UUID().uuidString,
        among: try store.babies(),
        fallback: theo
    )

    #expect(target == nil)
}

@MainActor
@Test func anUnparseableBabyIDActsOnNobody() throws {
    let (store, _, theo) = try makeTwoBabies()

    let target = ActivityTarget.resolve(
        activityBabyID: "not-a-uuid",
        among: try store.babies(),
        fallback: theo
    )

    #expect(target == nil)
}

// MARK: - A Stop control may never start a nap

@MainActor
@Test func aStopOnAStaleSleepActivityStartsNothing() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: noonLocal(), referenceSex: .girls, expectedDueDate: nil)

    // The nap already ended in the app; the Live Activity has not been
    // dismissed yet. The tap arrives on a button labelled "Stop".
    let running = try store.activeTimer(kind: .sleep, for: baby.id)
    let action = SleepToggleAction.decide(
        hasRunningSleep: running != nil,
        isStopControl: true
    )
    #expect(action == .nothing)

    // Applying the decision the way the intent does must leave the store
    // untouched. Treating this as a toggle used to start a brand new nap.
    if action == .start { _ = try store.startSleep(for: baby.id) }

    #expect(try store.activeTimer(kind: .sleep, for: baby.id) == nil)
    #expect(try store.homeSnapshot(for: baby.id).lastSleep == nil)
    #expect(try store.homeSnapshot(for: baby.id).runningTimers.isEmpty)
}

@MainActor
@Test func aStopOnALiveSleepActivityStopsIt() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: noonLocal(), referenceSex: .girls, expectedDueDate: nil)
    _ = try store.startSleep(for: baby.id)

    let running = try store.activeTimer(kind: .sleep, for: baby.id)
    #expect(SleepToggleAction.decide(hasRunningSleep: running != nil, isStopControl: true) == .stop)
}

@MainActor
@Test func theWidgetsSleepButtonIsStillAToggle() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: noonLocal(), referenceSex: .girls, expectedDueDate: nil)

    // No activity, so no id: the Home Screen widget's button, where a tap
    // with nothing running means "start a nap". This path must keep working.
    let running = try store.activeTimer(kind: .sleep, for: baby.id)
    #expect(SleepToggleAction.decide(hasRunningSleep: running != nil, isStopControl: false) == .start)

    _ = try store.startSleep(for: baby.id)
    let now = try store.activeTimer(kind: .sleep, for: baby.id)
    #expect(SleepToggleAction.decide(hasRunningSleep: now != nil, isStopControl: false) == .stop)
}

/// The sleep break card's Sleep starts a nap — and never ends one another
/// caregiver started while the card still showed the break.
@Test func aSleepOnABreakCardNeverStopsARunningNap() {
    #expect(SleepToggleAction.decide(hasRunningSleep: false, isStopControl: false, isStartControl: true) == .start)
    #expect(SleepToggleAction.decide(hasRunningSleep: true, isStopControl: false, isStartControl: true) == .nothing)
}

// MARK: - Which card a stale Stop takes down

// `LiveActivityController.isStale` decides that. The bodies that call
// ActivityKit cannot run on macOS, but this rule can, and it is the part that
// was wrong: it matched the baby alone, so a Stop on a dead feed card dismissed
// that baby's live sleep card too.

@Test func aStaleStopDismissesOnlyTheCardItWasTappedOn() {
    let mira = UUID()

    #expect(LiveActivityController.isStale(
        activityBabyID: mira,
        activityKindRaw: TimerKind.feed.rawValue,
        target: mira.uuidString,
        kind: .feed
    ))

    // The case the one-line fix exists for: same baby, other kind, still
    // running. Concurrent feed and sleep timers are legal here.
    #expect(!LiveActivityController.isStale(
        activityBabyID: mira,
        activityKindRaw: TimerKind.sleep.rawValue,
        target: mira.uuidString,
        kind: .feed
    ))
}

@Test func aStaleStopLeavesTheOtherBabysCardAlone() {
    let mira = UUID()
    let theo = UUID()

    #expect(!LiveActivityController.isStale(
        activityBabyID: theo,
        activityKindRaw: TimerKind.feed.rawValue,
        target: mira.uuidString,
        kind: .feed
    ))
}

@Test func aStopCarryingNoBabyDismissesNothing() {
    // The Home Screen widget's button, which owns no activity — and a target
    // that is not a UUID at all. Neither may take a card down on spec.
    for target in [nil, "", "not-a-uuid"] as [String?] {
        #expect(!LiveActivityController.isStale(
            activityBabyID: UUID(),
            activityKindRaw: TimerKind.sleep.rawValue,
            target: target,
            kind: .sleep
        ))
    }
}
