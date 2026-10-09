import BabyCore
import BabyProtocol
import BabyStore
import Foundation
import SwiftData
import Testing
@testable import BabySync

/// Records what the engine asked of Live Activities, which are no-ops here.
@MainActor
final class RecordingTimerAnnouncer: RemoteTimerAnnouncer {
    private(set) var started: [UUID] = []
    private(set) var changed: [UUID] = []
    private(set) var ended: [UUID] = []

    func started(_ timer: ActiveTimer, in store: BabyStore) { started.append(timer.id) }
    func changed(_ timer: ActiveTimer, in store: BabyStore) async { changed.append(timer.id) }
    func ended(timerID: UUID, liveActivityID: String?, in store: BabyStore) async { ended.append(timerID) }
}

/// A feed or a nap running on one phone runs on the other, and a stop on
/// either ends it on both — through the fake server, which follows the real
/// server's timer rules.
@MainActor
@Suite("Timers across phones")
struct TimerSyncTests {
    /// Both phones in the family, each having synced once.
    static func joinedFamily(clock: TestDateProvider = TestDateProvider(.now), announcer: RecordingTimerAnnouncer = RecordingTimerAnnouncer())
        async throws -> (Family, Baby, owner: FamilySyncEngine, joiner: FamilySyncEngine) {
        let family = try Family(clock: clock)
        let (baby, _) = try family.seedOwner()
        let owner = family.ownerEngine()
        try await owner.activateAsOwner()
        let joiner = family.joinerEngine(announcer: announcer)
        try await joiner.activateAsJoiner()
        return (family, baby, owner, joiner)
    }

    static func liveSleeps(_ store: BabyStore) throws -> [SleepEntry] {
        try store.context.fetch(FetchDescriptor<SleepEntry>()).filter { !$0.isTombstoned }
    }

    /// The brief's round trip: start on one phone, the other sees it running;
    /// stop on the other, the first sees it stopped. One nap on each, the
    /// same one.
    @Test func napStartedOnOnePhoneAndStoppedOnTheOther() async throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_790_000_000))
        let (family, baby, owner, joiner) = try await Self.joinedFamily(clock: clock)

        let started = try family.owner.startSleep(for: baby.id)
        await owner.syncNow(.localWrite)
        await joiner.syncNow(.foreground)

        let mirrored = try #require(try family.joiner.activeTimer(kind: .sleep, for: baby.id))
        #expect(mirrored.id == started.id)
        #expect(abs(mirrored.startedAt.timeIntervalSince(started.startedAt)) < 0.001)

        clock.advance(by: 45 * 60)
        let finished = try family.joiner.stopSleep(mirrored)
        await joiner.syncNow(.localWrite)
        await owner.syncNow(.foreground)

        #expect(try family.owner.activeTimers(for: baby.id).isEmpty)
        #expect(try family.joiner.activeTimers(for: baby.id).isEmpty)
        #expect(try Self.liveSleeps(family.owner).map(\.id) == [finished.id])
        #expect(try Self.liveSleeps(family.joiner).map(\.id) == [finished.id])
        let arrived = try #require(try Self.liveSleeps(family.owner).first)
        let ended = try #require(arrived.endedAt)
        #expect(abs(ended.timeIntervalSince(clock.now)) < 0.001)
        #expect(family.server.liveTimerIDs.isEmpty)
    }

    /// A feed carries its side and closed components across, under the IDs
    /// the starting phone chose.
    @Test func runningFeedShowsTheSameSideAndComponents() async throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_790_000_000))
        let (family, baby, owner, joiner) = try await Self.joinedFamily(clock: clock)

        // A running timer can already carry a closed part the first time it
        // syncs — one-part timers never grow a second part on a still-running
        // timer locally, so the closed part is built directly here, the way
        // it would arrive from an older phone's switch.
        let timer = ActiveTimer(
            babyID: baby.id, kind: .feed, startedAt: clock.now,
            timeZoneID: "UTC", currentSide: .right, realm: family.owner.activeRealm
        )
        timer.completedComponents = [.breast(id: UUID(), side: .left, seconds: 300)]
        family.owner.context.insert(timer)
        try family.owner.context.save()
        try family.owner.reduceTimerIntent(.start(timerID: timer.id, babyID: baby.id, kind: .feed, side: .right))
        await owner.syncNow(.localWrite)
        await joiner.syncNow(.foreground)

        let mirrored = try #require(try family.joiner.activeTimer(kind: .feed, for: baby.id))
        #expect(mirrored.id == timer.id)
        #expect(mirrored.currentSide == .right)
        #expect(mirrored.completedComponents == timer.completedComponents)
        #expect(mirrored.completedComponents.map(\.durationSeconds) == [300])
    }

    /// Both caregivers start a nap for the same baby before either syncs. The
    /// server keeps the first start (one live timer per baby and kind,
    /// `startTimer` in timers.go) and supersedes the second; the second phone
    /// gives up its own and shows the family's.
    @Test func simultaneousStartsLeaveTheFirstRunningOnBoth() async throws {
        let (family, baby, owner, joiner) = try await Self.joinedFamily()

        let first = try family.owner.startSleep(for: baby.id)
        let second = try family.joiner.startSleep(for: baby.id)
        #expect(first.id != second.id)
        await owner.syncNow(.localWrite)
        await joiner.syncNow(.localWrite)
        await owner.syncNow(.foreground)

        #expect(family.server.liveTimerIDs == [first.id])
        #expect(try family.owner.activeTimers(for: baby.id).map(\.id) == [first.id])
        #expect(try family.joiner.activeTimers(for: baby.id).map(\.id) == [first.id])
    }

    /// Both caregivers stop the same nap before either syncs. The first stop
    /// wins (`endedTimerOutcome`, timers.go); both phones end with that one
    /// nap and no second, locally derived copy.
    @Test func simultaneousStopsLeaveOneNapOnBoth() async throws {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_790_000_000))
        let (family, baby, owner, joiner) = try await Self.joinedFamily(clock: clock)
        let running = try family.owner.startSleep(for: baby.id)
        await owner.syncNow(.localWrite)
        await joiner.syncNow(.foreground)

        clock.advance(by: 600)
        let ownerStop = try family.owner.stopSleep(running)
        let joinerStop = try family.joiner.stopSleep(try #require(try family.joiner.activeTimer(kind: .sleep, for: baby.id)))
        await owner.syncNow(.localWrite)
        await joiner.syncNow(.localWrite)
        await owner.syncNow(.foreground)

        #expect(try Self.liveSleeps(family.owner).map(\.id) == [ownerStop.id])
        #expect(try Self.liveSleeps(family.joiner).map(\.id) == [ownerStop.id])
        #expect(joinerStop.isTombstoned)
    }

    /// In the foreground a remote nap gets a Live Activity here, and loses it
    /// when the other phone stops it.
    @Test func foregroundAnnouncesRemoteTimersStartingAndEnding() async throws {
        let announcer = RecordingTimerAnnouncer()
        let (family, baby, owner, joiner) = try await Self.joinedFamily(announcer: announcer)
        joiner.startForegroundPolling()
        defer { joiner.stopForegroundPolling() }

        let timer = try family.owner.startSleep(for: baby.id)
        await owner.syncNow(.localWrite)
        await joiner.syncNow(.foreground)
        #expect(announcer.started == [timer.id])

        _ = try family.owner.stopSleep(timer)
        await owner.syncNow(.localWrite)
        await joiner.syncNow(.foreground)
        #expect(announcer.ended == [timer.id])
    }

    /// In the background nothing is started — ActivityKit would refuse it —
    /// but an ending still is, so no card outlives its nap.
    @Test func backgroundSyncStartsNoLiveActivityButStillEndsOne() async throws {
        let announcer = RecordingTimerAnnouncer()
        let (family, baby, owner, joiner) = try await Self.joinedFamily(announcer: announcer)

        let timer = try family.owner.startSleep(for: baby.id)
        await owner.syncNow(.localWrite)
        await joiner.syncNow(.backgroundPush)
        #expect(announcer.started.isEmpty)
        #expect(try family.joiner.activeTimer(kind: .sleep, for: baby.id) != nil)

        _ = try family.owner.stopSleep(timer)
        await owner.syncNow(.localWrite)
        await joiner.syncNow(.backgroundPush)
        #expect(announcer.ended == [timer.id])
    }
}

// MARK: - Another caregiver's stop and next start, in either order

/// The sync engine announces a remote stop and the next part's start only
/// after the whole sync has landed, so by then the store already holds the
/// next part, whichever transition is announced first. Either order must
/// leave this phone's one card showing that part running.
@MainActor
@Suite("Remote part handover on the card")
struct RemotePartHandoverTests {
    /// Left ran 5 min on this phone under a card; then another caregiver
    /// stopped it and started Right, which the sync applied.
    static func leftHandedToRight() async throws -> (BabyStore, MemoryCardBoard, left: (id: UUID, card: String), right: ActiveTimer) {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
        let store = try BabyStore.inMemory(dateProvider: clock)
        store.breakLimits = { .standard }
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let board = MemoryCardBoard()
        let left = try store.startPart(.breast(.left), for: baby.id)
        LiveCards.$board.withValue(board) { LiveActivityController.start(for: left, in: store) }
        let leftCard = try #require(left.liveActivityID)
        let leftID = left.id
        clock.advance(by: 300)
        let right = try store.startPart(.breast(.right), for: baby.id)
        return (store, board, (leftID, leftCard), right)
    }

    @Test func theStartAnnouncedFirst() async throws {
        let (store, board, left, right) = try await Self.leftHandedToRight()
        let announcer = LiveActivityRemoteTimerAnnouncer()
        await LiveCards.$board.withValue(board) {
            await announcer.started(right, in: store)
            await announcer.ended(timerID: left.id, liveActivityID: left.card, in: store)
        }
        #expect(board.cards().map(\.id) == [left.card])
        #expect(board.cards().first?.state.phase == .running)
        #expect(board.cards().first?.state.activityKind == .breastRight)
        #expect(right.liveActivityID == left.card)
    }

    @Test func theStopAnnouncedFirst() async throws {
        let (store, board, left, right) = try await Self.leftHandedToRight()
        let announcer = LiveActivityRemoteTimerAnnouncer()
        await LiveCards.$board.withValue(board) {
            await announcer.ended(timerID: left.id, liveActivityID: left.card, in: store)
            await announcer.started(right, in: store)
        }
        #expect(board.cards().map(\.id) == [left.card])
        #expect(board.cards().first?.state.phase == .running)
        #expect(board.cards().first?.state.activityKind == .breastRight)
        #expect(right.liveActivityID == left.card)
    }

    /// In the background the engine announces no start at all — only the
    /// stop. The card must still show the part that runs.
    @Test func onlyTheStopAnnounced() async throws {
        let (store, board, left, right) = try await Self.leftHandedToRight()
        await LiveCards.$board.withValue(board) {
            await LiveActivityRemoteTimerAnnouncer().ended(timerID: left.id, liveActivityID: left.card, in: store)
        }
        #expect(board.cards().first?.state.activityKind == .breastRight)
        #expect(right.liveActivityID == left.card)
    }
}

// MARK: - A joined part follows its session when its anchor is withdrawn

/// The owner stops Left, and the caregiver — whose phone still shows Left
/// running — taps Right. The caregiver's safety net stops Left locally (its
/// own record Y) and starts Right joined to Y. The owner's stop (record X)
/// reaches the server first, so the caregiver's stop is refused
/// `timer_ended` and Y is withdrawn. Right must follow Y's session to X's,
/// or it ends up in a session of its own on both phones.
@MainActor
@Suite("Joined part after a lost stop race")
struct JoinedPartAfterLostStopTests {
    static func race(kind: TimerKind) async throws
        -> (Family, Baby, TestDateProvider, owner: FamilySyncEngine, caregiver: FamilySyncEngine, running: UUID) {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
        let (family, baby, owner, caregiver) = try await TimerSyncTests.joinedFamily(clock: clock)
        family.owner.breakLimits = { .standard }
        family.joiner.breakLimits = { .standard }
        // Well clear of the seeded feed, so Left starts a session of its own.
        clock.advance(by: 3 * 3600)
        let timer = kind == .feed
            ? try family.owner.startPart(.breast(.left), for: baby.id)
            : try family.owner.startSleep(for: baby.id)
        await owner.syncNow(.localWrite)
        await caregiver.syncNow(.foreground)
        try #require(try family.joiner.activeTimer(kind: kind, for: baby.id)?.id == timer.id)
        return (family, baby, clock, owner, caregiver, timer.id)
    }

    static func liveFeeds(_ store: BabyStore) throws -> [FeedEntry] {
        try store.context.fetch(FetchDescriptor<FeedEntry>()).filter { !$0.isTombstoned }
    }

    @Test func runningRightFollowsTheOwnersLeftSession() async throws {
        let (family, baby, clock, owner, caregiver, leftID) = try await Self.race(kind: .feed)

        clock.advance(by: 300)
        let ownerRunning = try #require(try family.owner.activeTimer(id: leftID))
        let ownerLeft = try #require(try family.owner.stopFeed(ownerRunning))
        await owner.syncNow(.localWrite)                                   // the owner's stop lands first
        let right = try family.joiner.startPart(.breast(.right), for: baby.id)
        let caregiverLeftID = try #require(right.sessionID)
        #expect(caregiverLeftID != ownerLeft.id)
        await caregiver.syncNow(.localWrite)

        // The settle path: the caregiver's own Left stop was refused
        // `timer_ended` and `settleRefusedTimerCommand` withdrew its record.
        let caregiverStop = try #require(try family.joiner.pendingOutbox().first {
            $0.operation == .timerStop && $0.entityID == leftID
        })
        #expect(caregiverStop.deliveryState == .superseded)
        #expect(caregiverStop.supersededCode == "timer_ended")
        let caregiverLeft = try #require(try EngineFixture.feeds(family.joiner).first { $0.id == caregiverLeftID })
        #expect(caregiverLeft.isTombstoned)
        #expect(try family.joiner.activeTimer(id: right.id)?.sessionID == ownerLeft.effectiveFeedingSessionID)
        await owner.syncNow(.foreground)
        #expect(try family.owner.activeTimer(id: right.id)?.sessionID == ownerLeft.effectiveFeedingSessionID)

        clock.advance(by: 200)
        let runningRight = try #require(try family.joiner.activeTimer(id: right.id))
        _ = try family.joiner.stopFeed(runningRight)
        await caregiver.syncNow(.localWrite)
        await owner.syncNow(.foreground)

        for store in [family.owner, family.joiner] {
            #expect(try store.activeTimers(for: baby.id).isEmpty)
            let sessions = try store.feedingSessions(for: baby.id, from: clock.now.addingTimeInterval(-3600), to: .distantFuture)
            #expect(sessions.count == 1)
            #expect(sessions.first?.id == ownerLeft.effectiveFeedingSessionID)
            #expect(sessions.first?.feedIDs.count == 2)
            #expect(try store.nextDispatchBatch(limit: 100).isEmpty)
        }
    }

    /// Right already stopped before the caregiver synced: the stored record
    /// is re-linked instead, and the owner sees the same one session.
    @Test func stoppedRightFollowsTheOwnersLeftSession() async throws {
        let (family, baby, clock, owner, caregiver, leftID) = try await Self.race(kind: .feed)

        clock.advance(by: 300)
        let ownerRunning = try #require(try family.owner.activeTimer(id: leftID))
        let ownerLeft = try #require(try family.owner.stopFeed(ownerRunning))
        await owner.syncNow(.localWrite)
        let right = try family.joiner.startPart(.breast(.right), for: baby.id)
        clock.advance(by: 200)
        let caregiverRight = try #require(try family.joiner.stopFeed(right))
        #expect(caregiverRight.feedingSessionID != nil)
        #expect(caregiverRight.feedingSessionID != ownerLeft.effectiveFeedingSessionID)
        await caregiver.syncNow(.localWrite)
        #expect(caregiverRight.feedingSessionID == ownerLeft.effectiveFeedingSessionID)
        await caregiver.syncNow(.foreground)
        await owner.syncNow(.foreground)

        for store in [family.owner, family.joiner] {
            let sessions = try store.feedingSessions(for: baby.id, from: clock.now.addingTimeInterval(-3600), to: .distantFuture)
            #expect(sessions.count == 1)
            #expect(sessions.first?.id == ownerLeft.effectiveFeedingSessionID)
            #expect(Set(sessions.first?.feedIDs ?? []) == [ownerLeft.id, caregiverRight.id])
            #expect(try store.nextDispatchBatch(limit: 100).isEmpty)
        }
    }

    /// The same race for a nap: the caregiver ends the nap and starts the
    /// next one inside the break, after the owner already ended it.
    @Test func runningNapFollowsTheOwnersNapSession() async throws {
        let (family, baby, clock, owner, caregiver, napID) = try await Self.race(kind: .sleep)

        clock.advance(by: 1800)
        let ownerRunning = try #require(try family.owner.activeTimer(id: napID))
        let ownerNap = try family.owner.stopSleep(ownerRunning)
        await owner.syncNow(.localWrite)
        let caregiverRunning = try #require(try family.joiner.activeTimer(id: napID))
        let caregiverNap = try family.joiner.stopSleep(caregiverRunning)
        clock.advance(by: 60)
        let next = try family.joiner.startSleep(for: baby.id)
        #expect(next.sessionID == caregiverNap.id)
        await caregiver.syncNow(.localWrite)

        #expect(caregiverNap.isTombstoned)
        #expect(try family.joiner.activeTimer(id: next.id)?.sessionID == ownerNap.effectiveSleepSessionID)

        clock.advance(by: 1200)
        let runningNext = try #require(try family.joiner.activeTimer(id: next.id))
        _ = try family.joiner.stopSleep(runningNext)
        await caregiver.syncNow(.localWrite)
        await owner.syncNow(.foreground)

        for store in [family.owner, family.joiner] {
            let sessions = try store.sleepSessions(for: baby.id, from: .distantPast, to: .distantFuture)
            #expect(sessions.count == 1)
            #expect(sessions.first?.id == ownerNap.effectiveSleepSessionID)
            #expect(sessions.first?.sleepIDs.count == 2)
            #expect(try store.nextDispatchBatch(limit: 100).isEmpty)
        }
    }
}
