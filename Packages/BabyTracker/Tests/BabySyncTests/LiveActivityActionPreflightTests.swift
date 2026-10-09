import BabyCore
import BabyProtocol
import BabyStore
import Foundation
import Testing
@testable import BabySync

/// The joiner phone's connection to the fake family server, with a hook that
/// runs while a catch-up is on the wire — the window in which another
/// caregiver, a demotion or a session switch can land.
@MainActor
final class PreflightTransport: FamilySyncTransport, @unchecked Sendable {
    let inner: FakeServerTransport
    /// Runs once, inside the next sync call, before the server answers.
    var duringSync: (@MainActor () throws -> Void)?
    /// Holds every sync until its task is cancelled: a catch-up that would
    /// outlive its timeout.
    var hangsUntilCancelled = false
    private(set) var cancelledCalls = 0

    init(inner: FakeServerTransport) { self.inner = inner }

    var syncCalls: Int { inner.syncCalls }

    func sync(_ request: SyncRequest) async throws -> SyncResponse {
        if let hook = duringSync {
            duringSync = nil
            try hook()
        }
        if hangsUntilCancelled {
            do {
                // Never wakes on its own: only the preflight's cancellation ends it.
                try await Task.sleep(for: .seconds(86_400))
            } catch {
                cancelledCalls += 1
                throw error
            }
        }
        return try await inner.sync(request)
    }

    func snapshot(pageToken: String?) async throws -> SnapshotResponse { try await inner.snapshot(pageToken: pageToken) }
}

/// Two phones in one family on the fake server; the joiner's Lock Screen card
/// is what a button acts on. The joiner's preflight catches up through a real
/// `SyncCoordinator` over its own transport.
@MainActor
struct PreflightHarness {
    let family: Family
    let baby: Baby
    let owner: FamilySyncEngine
    let joiner: FamilySyncEngine
    let clock: TestDateProvider
    let transport: PreflightTransport
    let preflight: LiveActivityActionPreflight

    var store: BabyStore { family.joiner }
    var realm: RealmID { family.joinerKey.realmID }

    /// A timeout that never fires on its own: every catch-up in these tests
    /// finishes, or is held, deterministically.
    static let neverTimesOut: @Sendable (Duration) async throws -> Void = { _ in try await Task.sleep(for: .seconds(86_400)) }

    static func make(sleep: @escaping @Sendable (Duration) async throws -> Void = neverTimesOut) async throws -> PreflightHarness {
        let clock = TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000))
        let (family, baby, owner, joiner) = try await TimerSyncTests.joinedFamily(clock: clock)
        family.owner.breakLimits = { .standard }
        family.joiner.breakLimits = { .standard }
        let transport = PreflightTransport(inner: family.joinerTransport)
        let key = family.joinerKey
        let store = family.joiner
        let catchUp = SyncCoordinatorCatchUp(store: store, transport: transport, accountKey: key.accountSessionKey, renew: {})
        let preflight = LiveActivityActionPreflight(
            store: store, catchUp: catchUp,
            sessionIsCurrent: { (try? store.accountAllowsOrdinarySync(key.leaseKey)) == true },
            sleep: sleep
        )
        return PreflightHarness(family: family, baby: baby, owner: owner, joiner: joiner, clock: clock, transport: transport, preflight: preflight)
    }

    /// The owner does something; both phones sync, so the joiner's store and
    /// its card agree.
    func ownerDoes<T>(_ change: () throws -> T) async rethrows -> T {
        let result = try change()
        await owner.syncNow(.localWrite)
        await joiner.syncNow(.foreground)
        return result
    }

    /// The owner does something the joiner has not pulled yet.
    func ownerDoesUnseen<T>(_ change: () throws -> T) async rethrows -> T {
        let result = try change()
        await owner.syncNow(.localWrite)
        return result
    }

    var appliedSequence: Int64 { (try? store.syncCursor()?.sequence) ?? 0 }

    func scope(_ kind: TimerKind, family familyID: UUID = FakeFamilyServer.familyID, epoch: Int64 = 1) throws -> LiveActivityScope {
        try LiveActivityScope(serverInstanceID: FakeFamilyServer.instanceID, recoveryEpoch: epoch, familyID: familyID,
                              babyID: baby.id, kindRaw: kind.rawValue)
    }

    /// The target a pushed card's button carries: its revision, as the server
    /// built it at `sequence` (by default, what the joiner has applied).
    func target(_ kind: TimerKind, timer: UUID?, session: UUID? = nil, sequence: Int64? = nil,
                family familyID: UUID = FakeFamilyServer.familyID, canWrite: Bool = true) throws -> LiveActivityActionTarget {
        let revision = try LiveActivityRevision(
            scope: scope(kind, family: familyID), generationID: UUID(), timerID: timer, sessionID: session,
            familySequence: sequence ?? appliedSequence, presentationVersion: 1, presentationTimestamp: 1_800_000_000,
            canWrite: canWrite
        )
        return LiveActivityActionTarget(revision: revision)
    }

    /// A card the server pushed for what the owner just did, which the
    /// joiner has not pulled: it is ahead of the joiner's store.
    func cardAhead(_ kind: TimerKind, timer: UUID?) throws -> LiveActivityActionTarget {
        try target(kind, timer: timer, sequence: family.server.sequence)
    }

    /// What this phone has queued or sent — the evidence of a mutation.
    func outboxIDs() throws -> Set<UUID> { Set(try store.liveOutboxRows().map(\.id)) }
}

@MainActor
@Suite("Live Activity action preflight", .timeLimit(.minutes(1)))
struct LiveActivityActionPreflightTests {
    /// The card still shows the nap the partner ended and replaced, and this
    /// phone has pulled both: the store is past the card, so the card is
    /// reconciled without the network and nothing is written.
    @Test func aCardForAStoppedTimerIsAlreadyFinishedWhenANewOneRuns() async throws {
        let h = try await PreflightHarness.make()
        let first = try await h.ownerDoes { try h.family.owner.startSleep(for: h.baby.id) }
        let card = try h.target(.sleep, timer: first.id)

        h.clock.advance(by: 600)
        let second = try await h.ownerDoes { () throws -> ActiveTimer in
            _ = try h.family.owner.stopSleep(first)
            h.clock.advance(by: 60)
            return try h.family.owner.startSleep(for: h.baby.id)
        }
        h.family.joinerTransport.offline = true
        let outbox = try h.outboxIDs()
        let calls = h.transport.syncCalls

        let decision = await h.preflight.prepare(target: card)

        #expect(decision == .alreadyFinished)
        #expect(h.transport.syncCalls == calls)
        #expect(try h.store.activeTimer(kind: .sleep, for: h.baby.id)?.id == second.id)
        #expect(try h.outboxIDs() == outbox)
    }

    /// The partner ended the nap and started the next, and this phone has
    /// pulled neither. The store still shows the card's nap, so Wake up acts
    /// at once — and the server, where the nap already ended, answers the
    /// stale stop `superseded`: the partner's nap and the new one stand on
    /// both phones.
    @Test func aStaleWakeTheStoreStillVouchesForIsSupersededByTheServer() async throws {
        let h = try await PreflightHarness.make()
        let first = try await h.ownerDoes { try h.family.owner.startSleep(for: h.baby.id) }
        let card = try h.target(.sleep, timer: first.id)
        h.clock.advance(by: 600)
        let second = try await h.ownerDoesUnseen { () throws -> ActiveTimer in
            _ = try h.family.owner.stopSleep(first)
            h.clock.advance(by: 60)
            return try h.family.owner.startSleep(for: h.baby.id)
        }
        let calls = h.transport.syncCalls

        let decision = await h.preflight.prepare(target: card)
        guard case .ready(let context) = decision else { Issue.record("not ready: \(decision)"); return }
        #expect(h.transport.syncCalls == calls)
        let stale = try #require(try h.store.activeTimer(kind: .sleep, for: h.baby.id))
        h.clock.advance(by: 300)
        let staleStop = try h.store.stopSleep(stale, expectedWriteContext: context)
        await h.joiner.syncNow(.localWrite)
        await h.owner.syncNow(.foreground)

        #expect(try h.store.activeTimer(kind: .sleep, for: h.baby.id)?.id == second.id)
        #expect(try h.family.owner.activeTimer(kind: .sleep, for: h.baby.id)?.id == second.id)
        #expect(try TimerSyncTests.liveSleeps(h.store).map(\.id) == TimerSyncTests.liveSleeps(h.family.owner).map(\.id))
        #expect(try TimerSyncTests.liveSleeps(h.store).count == 1)
        #expect(staleStop.isTombstoned)
    }

    /// A card from another family — a family left, rejoined elsewhere — whose
    /// baby happens to carry the same identifier. It names another realm, so
    /// nothing is fetched or written for it.
    @Test func aCardFromAnotherFamilyWithTheSameBabyIsWrongRealm() async throws {
        let h = try await PreflightHarness.make()
        let timer = try await h.ownerDoes { try h.family.owner.startSleep(for: h.baby.id) }
        let calls = h.transport.syncCalls

        let decision = await h.preflight.prepare(target: try h.target(.sleep, timer: timer.id, family: UUID()))

        #expect(decision == .wrongRealm)
        #expect(h.transport.syncCalls == calls)
        #expect(try h.store.activeTimer(kind: .sleep, for: h.baby.id)?.id == timer.id)
    }

    /// The card shows a nap the partner started, which this phone has not
    /// pulled: one catch-up brings it here, and the action may go ahead.
    @Test func aCardAheadOfTheStoreCatchesUpFirst() async throws {
        let h = try await PreflightHarness.make()
        let nap = try await h.ownerDoesUnseen { try h.family.owner.startSleep(for: h.baby.id) }
        let card = try h.cardAhead(.sleep, timer: nap.id)
        let calls = h.transport.syncCalls

        let decision = await h.preflight.prepare(target: card)

        #expect(decision == .ready(try h.store.captureWriteContext()))
        #expect(h.transport.syncCalls == calls + 1)
        #expect(try h.store.activeTimer(kind: .sleep, for: h.baby.id)?.id == nap.id)
    }

    /// The card shows state this phone has not applied, and the catch-up did
    /// not reach it: the phone cannot vouch for what the card shows.
    @Test func aCardAheadOfTheAppliedCursorHandsOff() async throws {
        let h = try await PreflightHarness.make()
        let timer = try await h.ownerDoes { try h.family.owner.startSleep(for: h.baby.id) }

        let decision = await h.preflight.prepare(target: try h.target(.sleep, timer: UUID(), sequence: h.appliedSequence + 50))

        #expect(decision == .openApp(reason: .cardAhead))
        #expect(try h.store.activeTimer(kind: .sleep, for: h.baby.id)?.id == timer.id)
    }

    /// No answer from the server — offline, or a refusal — is no freshness
    /// for a card ahead of the store. `BackgroundSync.runOnce` would report
    /// the same attempt as `true`: it says an attempt was made, not that it
    /// got through.
    @Test func aFailedPullHandsOffWithoutWriting() async throws {
        let h = try await PreflightHarness.make()
        let nap = try await h.ownerDoesUnseen { try h.family.owner.startSleep(for: h.baby.id) }
        let card = try h.cardAhead(.sleep, timer: nap.id)
        let outbox = try h.outboxIDs()

        h.family.joinerTransport.offline = true
        let attempted = await BackgroundSync(
            coordinator: SyncCoordinator(store: h.store, transport: h.transport, accountKey: h.family.joinerKey.accountSessionKey),
            store: h.store, accountKey: h.family.joinerKey.accountSessionKey
        ).runOnce(reason: .extensionRequest)
        #expect(attempted)
        #expect(await h.preflight.prepare(target: card) == .openApp(reason: .refreshFailed))

        h.family.joinerTransport.offline = false
        h.family.joinerTransport.failNextWith = .serviceUnavailable
        #expect(await h.preflight.prepare(target: card) == .openApp(reason: .refreshFailed))

        #expect(try h.store.activeTimer(kind: .sleep, for: h.baby.id) == nil)
        #expect(try h.outboxIDs() == outbox)
    }

    /// Demoted to viewer while the catch-up was on the wire. The card still
    /// says it may write; the family's current authority says it may not.
    @Test func aViewerDemotionIsReadOnlyWhateverTheCardSays() async throws {
        let h = try await PreflightHarness.make()
        let nap = try await h.ownerDoesUnseen { try h.family.owner.startSleep(for: h.baby.id) }
        let card = try h.cardAhead(.sleep, timer: nap.id)

        h.transport.duringSync = { try h.store.setWritePermission(.viewer, for: h.realm) }
        #expect(await h.preflight.prepare(target: card) == .readOnly)

        // Already a viewer: refused before anything goes out.
        let calls = h.transport.syncCalls
        #expect(await h.preflight.prepare(target: card) == .readOnly)
        #expect(h.transport.syncCalls == calls)
    }

    /// The realm under the store moved while the catch-up was awaited: a
    /// sign-out and back, or a switch to another family. What was checked
    /// before the await no longer describes the store.
    @Test func aSessionSwitchDuringTheAwaitIsNeverActedOn() async throws {
        let h = try await PreflightHarness.make()
        let nap = try await h.ownerDoesUnseen { try h.family.owner.startSleep(for: h.baby.id) }
        // Ahead of anything a catch-up here brings: every check below needs one.
        let card = try h.target(.sleep, timer: nap.id, sequence: h.family.server.sequence + 50)

        h.transport.duringSync = { [store = h.store, realm = h.realm] in
            try store.setActiveRealm(.local)
            try store.setActiveRealm(realm)
        }
        #expect(await h.preflight.prepare(target: card) == .openApp(reason: .sessionChanged))

        h.transport.duringSync = { [store = h.store, key = h.family.joinerKey] in
            try store.markAccountSignedOut(key.leaseKey)
        }
        #expect(await h.preflight.prepare(target: card) == .openApp(reason: .sessionChanged))

        try h.store.markAccountAuthenticated(h.family.joinerKey.leaseKey, credentialVersion: UUID())
        let elsewhere = RealmID.shared(canonicalOrigin: EngineFixture.origin, serverInstanceID: FakeFamilyServer.instanceID, familyID: UUID())
        h.transport.duringSync = { [store = h.store] in try store.setActiveRealm(elsewhere) }
        #expect(await h.preflight.prepare(target: card) == .wrongRealm)
    }

    /// A catch-up that outlives its budget is cancelled — not left running
    /// behind the intent — and is no evidence of freshness.
    @Test func aCatchUpPastItsTimeoutIsCancelledAndHandsOff() async throws {
        let h = try await PreflightHarness.make(sleep: { _ in })
        let nap = try await h.ownerDoesUnseen { try h.family.owner.startSleep(for: h.baby.id) }
        h.transport.hangsUntilCancelled = true

        let decision = await h.preflight.prepare(target: try h.cardAhead(.sleep, timer: nap.id), timeout: .seconds(5))

        #expect(decision == .openApp(reason: .refreshFailed))
        #expect(h.transport.cancelledCalls == 1)
        #expect(try h.store.activeTimer(kind: .sleep, for: h.baby.id) == nil)
    }

    /// The card shows what runs now, and the phone has already applied the
    /// cursor it was built at: the action may go ahead at once — off the
    /// network too — inside the realm generation that was checked.
    @Test func aCurrentCardIsReadyInTheCheckedRealm() async throws {
        let h = try await PreflightHarness.make()
        let timer = try await h.ownerDoes { try h.family.owner.startSleep(for: h.baby.id) }
        h.family.joinerTransport.offline = true
        let calls = h.transport.syncCalls

        let decision = await h.preflight.prepare(target: try h.target(.sleep, timer: timer.id))

        let context = try h.store.captureWriteContext()
        #expect(decision == .ready(context))
        #expect(context.realm == h.realm)
        #expect(h.transport.syncCalls == calls)
    }

    /// A break card names its session. While that break is open its L, R and
    /// Bottle mean what they say; once this phone has the partner's next
    /// part, the break is over and the card is only reconciled.
    @Test func aBreakCardIsReadyOnlyWhileItsBreakIsOpen() async throws {
        let h = try await PreflightHarness.make()
        let part = try await h.ownerDoes { try h.family.owner.startPart(.breast(.left), for: h.baby.id) }
        h.clock.advance(by: 300)
        let feed = try #require(try await h.ownerDoes { try h.family.owner.stopFeed(part) })
        h.clock.advance(by: 60)
        let session = feed.effectiveFeedingSessionID
        let card = try h.target(.feed, timer: nil, session: session)

        #expect(await h.preflight.prepare(target: card) == .ready(try h.store.captureWriteContext()))

        let next = try await h.ownerDoes { try h.family.owner.startPart(.breast(.right), for: h.baby.id) }
        #expect(await h.preflight.prepare(target: card) == .alreadyFinished)
        #expect(try h.store.activeTimer(kind: .feed, for: h.baby.id)?.id == next.id)
        #expect(try h.store.activeTimer(kind: .feed, for: h.baby.id)?.currentSide == .right)
    }

    /// A token card no push has reached shows only what this phone wrote:
    /// there is nothing newer to wait for, so it is ready without a catch-up
    /// — off the private network too — once realm and authority check out.
    @Test func aTokenCardNoPushHasReachedIsReadyWithoutACatchUp() async throws {
        let h = try await PreflightHarness.make()
        _ = try await h.ownerDoes { try h.family.owner.startSleep(for: h.baby.id) }
        h.family.joinerTransport.offline = true
        let calls = h.transport.syncCalls
        let target = try #require(LiveActivityActionTarget.card(scope: try h.scope(.sleep), remote: nil))
        #expect(!target.isPushed)

        #expect(await h.preflight.prepare(target: target) == .ready(try h.store.captureWriteContext()))
        #expect(h.transport.syncCalls == calls)
    }
}
