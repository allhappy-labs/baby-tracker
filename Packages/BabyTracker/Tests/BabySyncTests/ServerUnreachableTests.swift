import Foundation
import Testing
@testable import BabySync
@testable import BabyStore

/// A phone whose VPN tunnel says "Connected" but carries nothing looks, to
/// the engine, exactly like a phone with no signal: every request fails with a
/// `URLError`. On 2026-10-19 that left one parent reading "Offline" for most
/// of a day while the phone had internet the whole time. Turning Tailscale off
/// and on fixed it in seconds. Once the failures have lasted long enough and
/// the phone does have a network, the engine says the server is out of reach,
/// so Home can name the fix.
@Suite(.serialized) @MainActor
struct ServerUnreachableTests {
    final class Clock {
        var now = Date(timeIntervalSince1970: 1_800_000_000)
        func advance(_ seconds: TimeInterval) { now = now.addingTimeInterval(seconds) }
    }

    @Test func failuresAtFirstReadAsOffline() async throws {
        let family = try Family()
        _ = try family.seedOwner()
        let clock = Clock()
        let owner = family.ownerEngine(now: { clock.now }, internetAvailable: { true })
        try await owner.activateAsOwner()

        family.ownerTransport.offline = true
        await owner.syncNow(.foreground)
        clock.advance(FamilySyncEngine.serverUnreachableAfter - 1)
        await owner.syncNow(.foreground)

        #expect(owner.status == .offline)
    }

    @Test func failuresThatOutlastTheThresholdWithInternetReadAsServerUnreachable() async throws {
        let family = try Family()
        _ = try family.seedOwner()
        let clock = Clock()
        let owner = family.ownerEngine(now: { clock.now }, internetAvailable: { true })
        try await owner.activateAsOwner()

        family.ownerTransport.offline = true
        await owner.syncNow(.foreground)
        clock.advance(FamilySyncEngine.serverUnreachableAfter)
        await owner.syncNow(.foreground)

        #expect(owner.status == .serverUnreachable)
    }

    /// No Wi-Fi and no signal: "Offline" is the truth, however long it lasts.
    @Test func withoutInternetFailuresStayOffline() async throws {
        let family = try Family()
        _ = try family.seedOwner()
        let clock = Clock()
        let owner = family.ownerEngine(now: { clock.now }, internetAvailable: { false })
        try await owner.activateAsOwner()

        family.ownerTransport.offline = true
        await owner.syncNow(.foreground)
        clock.advance(FamilySyncEngine.serverUnreachableAfter * 10)
        await owner.syncNow(.foreground)

        #expect(owner.status == .offline)
    }

    /// Hours without a network do not count: the clock starts when the
    /// network comes back.
    @Test func timeWithoutInternetDoesNotCount() async throws {
        let family = try Family()
        _ = try family.seedOwner()
        let clock = Clock()
        var internet = false
        let owner = family.ownerEngine(now: { clock.now }, internetAvailable: { internet })
        try await owner.activateAsOwner()

        family.ownerTransport.offline = true
        await owner.syncNow(.foreground)
        clock.advance(FamilySyncEngine.serverUnreachableAfter * 10)
        internet = true
        await owner.syncNow(.foreground)
        #expect(owner.status == .offline)

        clock.advance(FamilySyncEngine.serverUnreachableAfter)
        await owner.syncNow(.foreground)
        #expect(owner.status == .serverUnreachable)
    }

    /// One answer from the server ends the outage; the next failure starts a
    /// new one rather than inheriting the old one's age.
    @Test func aSuccessfulSyncRestartsTheClock() async throws {
        let family = try Family()
        _ = try family.seedOwner()
        let clock = Clock()
        let owner = family.ownerEngine(now: { clock.now }, internetAvailable: { true })
        try await owner.activateAsOwner()

        family.ownerTransport.offline = true
        await owner.syncNow(.foreground)
        clock.advance(FamilySyncEngine.serverUnreachableAfter)
        await owner.syncNow(.foreground)
        #expect(owner.status == .serverUnreachable)

        family.ownerTransport.offline = false
        await owner.syncNow(.foreground)
        #expect(owner.status.isUpToDate)

        family.ownerTransport.offline = true
        clock.advance(1)
        await owner.syncNow(.foreground)
        #expect(owner.status == .offline)
    }

    /// A retry keeps the failure on screen until it has an answer. Flipping
    /// to "Syncing" for each 20-second retry made Home's card blink out and
    /// back, shifting everything under it.
    @Test(arguments: [false, true])
    func aRetryInFlightKeepsTheFailureShowing(pastThreshold: Bool) async throws {
        let family = try Family()
        _ = try family.seedOwner()
        let clock = Clock()
        let owner = family.ownerEngine(now: { clock.now }, internetAvailable: { true })
        try await owner.activateAsOwner()

        family.ownerTransport.offline = true
        await owner.syncNow(.foreground)
        if pastThreshold {
            clock.advance(FamilySyncEngine.serverUnreachableAfter)
            await owner.syncNow(.foreground)
        }
        let failure = owner.status
        #expect(failure == (pastThreshold ? .serverUnreachable : .offline))

        family.ownerTransport.holdNext = true
        let retry = Task { await owner.syncNow(.foreground) }
        while !family.ownerTransport.isHolding { await Task.yield() }
        #expect(owner.status == failure)

        family.ownerTransport.offline = false
        family.ownerTransport.release()
        await retry.value
        #expect(owner.status.isUpToDate)
    }
}

extension Family {
    func ownerEngine(now: @escaping () -> Date, internetAvailable: @escaping () -> Bool) -> FamilySyncEngine {
        let importClient = PublishingResolverClient(
            inner: resolver, server: server,
            accountID: EngineFixture.ownerAccount, deviceID: EngineFixture.ownerDevice
        )
        return FamilySyncEngine(
            store: owner, familyKey: ownerKey, transport: ownerTransport, importClient: importClient,
            renew: {}, now: now, internetAvailable: internetAvailable
        )
    }
}
