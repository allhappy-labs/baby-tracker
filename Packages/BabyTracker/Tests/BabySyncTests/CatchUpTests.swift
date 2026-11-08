import Foundation
import Testing
@testable import BabySync
@testable import BabyStore

/// Coming back to the foreground, the store may predate what the other phone
/// did meanwhile. On 2026-11-01 a phone opened after four silent hours
/// stopped a feed the other parent had already stopped, then the sleep they
/// had started since. Until the first sync has an answer the engine says it
/// is catching up, and a catch-up without an answer names the server as out
/// of reach at once rather than after three minutes.
@Suite(.serialized) @MainActor
struct CatchUpTests {
    @Test func comingToTheForegroundCatchesUpUntilTheFirstSyncAnswers() async throws {
        let family = try Family()
        _ = try family.seedOwner()
        let owner = family.catchUpEngine()
        try await owner.activateAsOwner()
        #expect(!owner.isCatchingUp)

        owner.startForegroundPolling()
        #expect(owner.isCatchingUp)

        await owner.syncNow(.foreground)
        #expect(!owner.isCatchingUp)
        #expect(owner.status.isUpToDate)
        owner.stopForegroundPolling()
    }

    @Test func aCatchUpThatFailsWithANetworkNamesTheServerAtOnce() async throws {
        let family = try Family()
        _ = try family.seedOwner()
        let owner = family.catchUpEngine(internetAvailable: { true })
        try await owner.activateAsOwner()

        family.ownerTransport.offline = true
        owner.startForegroundPolling()
        await owner.syncNow(.foreground)

        #expect(!owner.isCatchingUp)
        #expect(owner.status == .serverUnreachable)
        // The next failure keeps saying so; it does not fall back to "Offline".
        await owner.syncNow(.foreground)
        #expect(owner.status == .serverUnreachable)
        owner.stopForegroundPolling()
    }

    @Test func aCatchUpWithoutANetworkStaysOffline() async throws {
        let family = try Family()
        _ = try family.seedOwner()
        let owner = family.catchUpEngine(internetAvailable: { false })
        try await owner.activateAsOwner()

        family.ownerTransport.offline = true
        owner.startForegroundPolling()
        await owner.syncNow(.foreground)

        #expect(!owner.isCatchingUp)
        #expect(owner.status == .offline)
        owner.stopForegroundPolling()
    }

    /// A dead tunnel gives no answer at all, only a timeout long after. The
    /// hold ends the wait, and the sync landing later still counts.
    @Test func aSyncWithoutAnAnswerEndsTheWaitAtTheHold() async throws {
        let family = try Family()
        _ = try family.seedOwner()
        let owner = family.catchUpEngine(hold: .milliseconds(50), internetAvailable: { true })
        try await owner.activateAsOwner()

        family.ownerTransport.holdNext = true
        owner.startForegroundPolling()
        let sync = Task { await owner.syncNow(.foreground) }
        while !family.ownerTransport.isHolding { await Task.yield() }

        await owner.waitForCatchUp()
        #expect(!owner.isCatchingUp)
        #expect(owner.status == .serverUnreachable)

        family.ownerTransport.release()
        await sync.value
        #expect(owner.status.isUpToDate)
        owner.stopForegroundPolling()
    }

    /// On 2026-11-02 a phone back from an hour asleep read "Offline" while
    /// its tunnel carried nothing. A renewal that finds another process
    /// holding the lock has heard nothing from the server either: the
    /// catch-up goes on, and the failure after it names the server.
    @Test func aRenewalThatFindsTheLockHeldIsNotAnAnswer() async throws {
        let family = try Family()
        _ = try family.seedOwner()
        var lockHeld = false
        let owner = family.catchUpEngine(internetAvailable: { true }, prepareSession: {
            if lockHeld {
                lockHeld = false
                throw SessionRefresher.RefreshError.lockUnavailable
            }
        })
        try await owner.activateAsOwner()

        family.ownerTransport.offline = true
        lockHeld = true
        owner.startForegroundPolling()
        await owner.syncNow(.foreground)
        #expect(owner.isCatchingUp)

        await owner.syncNow(.foreground)
        #expect(!owner.isCatchingUp)
        #expect(owner.status == .serverUnreachable)
        owner.stopForegroundPolling()
    }

    /// The hold passing with no answer is the answer, even when no sync is
    /// in flight at that moment.
    @Test func theHoldEndingWithoutAnAnswerNamesTheServer() async throws {
        let family = try Family()
        _ = try family.seedOwner()
        var lockHeld = false
        let owner = family.catchUpEngine(hold: .milliseconds(50), internetAvailable: { true }, prepareSession: {
            if lockHeld { throw SessionRefresher.RefreshError.lockUnavailable }
        })
        try await owner.activateAsOwner()

        lockHeld = true
        owner.startForegroundPolling()
        await owner.syncNow(.foreground)
        await owner.waitForCatchUp()
        #expect(owner.status == .serverUnreachable)
        owner.stopForegroundPolling()
    }

    @Test func goingToTheBackgroundEndsTheWait() async throws {
        let family = try Family()
        _ = try family.seedOwner()
        let owner = family.catchUpEngine()
        try await owner.activateAsOwner()

        owner.startForegroundPolling()
        let waiting = Task { await owner.waitForCatchUp() }
        owner.stopForegroundPolling()
        await waiting.value
        #expect(!owner.isCatchingUp)
    }
}

extension Family {
    func catchUpEngine(
        hold: Duration = .seconds(60),
        internetAvailable: @escaping () -> Bool = { true },
        prepareSession: @escaping () async throws -> Void = {}
    ) -> FamilySyncEngine {
        let importClient = PublishingResolverClient(
            inner: resolver, server: server,
            accountID: EngineFixture.ownerAccount, deviceID: EngineFixture.ownerDevice
        )
        // A poll far beyond the test, so only the test's own syncs run.
        return FamilySyncEngine(
            store: owner, familyKey: ownerKey, transport: ownerTransport, importClient: importClient,
            renew: {}, prepareSession: prepareSession, debounce: .seconds(1), pollInterval: .seconds(3600), livePollInterval: .seconds(3600),
            catchUpHold: hold, announcer: RecordingTimerAnnouncer(), internetAvailable: internetAvailable
        )
    }
}
