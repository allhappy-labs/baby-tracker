import BabyCore
import BabyProtocol
@testable import BabyStore
@testable import BabySync
import Foundation
import SwiftData
import Testing
@testable import SettingsFeature

/// The one-part timer race, through the real server: one caregiver stops
/// Left while the other — whose phone still shows Left running — taps Right.
/// Each phone runs the app's own composition (`Phone`), exactly as in
/// `TwoCaregiverSyncE2ETests`, against the container `protocol-e2e.sh` built.
@MainActor
@Suite("One-part timers between two caregivers", .serialized)
struct OnePartTimerSyncE2ETests {
    /// Outcome the harness produced (recorded from `make server-protocol-e2e`):
    /// both stops reach the server. The caregiver's sync goes first, so the
    /// caregiver's own Left stop — sent with Right's start in one batch — is
    /// the one the server accepts; the owner's Left stop for the same timer
    /// is refused (`timer_ended`) and `settleRefusedTimerCommand` withdraws
    /// the completion the owner derived from it. Right started already joined
    /// to the caregiver's Left record, which is the record that survives, so
    /// both phones end with exactly one session holding exactly two parts.
    ///
    /// The reverse order — the owner's stop reaching the server first — is
    /// `ownersStopFirstStillKeepsOneSession` below.
    @Test func twoCaregiversStopAndStartKeepOneSession() async throws {
        guard let environment = E2EEnvironment.current() else { return }
        let race = try await Race(environment: environment)
        defer { race.tearDown() }
        let (owner, caregiver, baby, clock) = (race.owner, race.caregiver, race.babyID, race.clock)

        let left = try owner.store.startPart(.breast(.left), for: baby)
        try await owner.sync(.localWrite)
        try await caregiver.sync()
        try #require(try caregiver.store.activeTimer(kind: .feed, for: baby)?.id == left.id)

        clock.advance(by: 300)
        _ = try owner.store.stopFeed(left)                                        // A stops
        let right = try caregiver.store.startPart(.breast(.right), for: baby)     // B still saw Left running
        try await caregiver.sync(.localWrite)
        try await owner.sync(.localWrite)
        try await caregiver.sync()

        clock.advance(by: 200)
        _ = try caregiver.store.stopFeed(right)
        try await caregiver.sync(.localWrite)
        try await owner.sync()

        let ownerStop = try #require(try timerStop(of: left.id, on: owner))
        #expect(ownerStop.deliveryState == .superseded, "the owner's losing Left stop settled as \(ownerStop.deliveryState)")
        let caregiverStop = try #require(try timerStop(of: left.id, on: caregiver))
        #expect(caregiverStop.deliveryState == .acknowledged, "the caregiver's winning Left stop settled as \(caregiverStop.deliveryState)")

        let shapes = try expectOneSessionOfLeftThenRight(on: [owner, caregiver], baby: baby)
        print("one-part timer proof: caregiver's Left stop won, owner's settled superseded (\(ownerStop.supersededCode ?? "-")); \(shapes)")
    }

    /// The realistic arrival order: the owner's Left stop reaches the server
    /// first. The caregiver's stop — sent with Right's start — is refused
    /// `timer_ended`, and `settleRefusedTimerCommand` withdraws the
    /// caregiver's Left record. Right was started joined to that record, so
    /// it follows it to the owner's Left session (`followWinningCompletion`)
    /// with a timer edit, and both phones end with one session of two parts.
    @Test func ownersStopFirstStillKeepsOneSession() async throws {
        guard let environment = E2EEnvironment.current() else { return }
        let race = try await Race(environment: environment)
        defer { race.tearDown() }
        let (owner, caregiver, baby, clock) = (race.owner, race.caregiver, race.babyID, race.clock)

        let left = try owner.store.startPart(.breast(.left), for: baby)
        try await owner.sync(.localWrite)
        try await caregiver.sync()
        try #require(try caregiver.store.activeTimer(kind: .feed, for: baby)?.id == left.id)

        clock.advance(by: 300)
        let ownerLeft = try #require(try owner.store.stopFeed(left))             // A stops…
        try await owner.sync(.localWrite)                                         // …and reaches the server first
        let right = try caregiver.store.startPart(.breast(.right), for: baby)     // B still saw Left running
        #expect(right.sessionID != ownerLeft.effectiveFeedingSessionID)
        try await caregiver.sync(.localWrite)
        try await owner.sync()

        let caregiverStop = try #require(try timerStop(of: left.id, on: caregiver))
        #expect(caregiverStop.deliveryState == .superseded, "the caregiver's losing Left stop settled as \(caregiverStop.deliveryState)")
        #expect(caregiverStop.supersededCode == "timer_ended")
        let ownerStop = try #require(try timerStop(of: left.id, on: owner))
        #expect(ownerStop.deliveryState == .acknowledged, "the owner's winning Left stop settled as \(ownerStop.deliveryState)")
        for side in [owner, caregiver] {
            let running = try #require(try side.store.activeTimer(id: right.id), "\(side.name) lost Right")
            #expect(running.sessionID == ownerLeft.effectiveFeedingSessionID, "\(side.name)'s Right is in \(String(describing: running.sessionID))")
        }

        clock.advance(by: 200)
        let caregiverRight = try #require(try caregiver.store.activeTimer(id: right.id))
        _ = try caregiver.store.stopFeed(caregiverRight)
        try await caregiver.sync(.localWrite)
        try await owner.sync()

        let shapes = try expectOneSessionOfLeftThenRight(on: [owner, caregiver], baby: baby)
        print("one-part timer proof (owner first): owner's Left stop won, caregiver's settled superseded (\(caregiverStop.supersededCode ?? "-")); \(shapes)")
    }

    /// The walkthrough's scenarios 1 and 2 across two phones, in the plain
    /// order: the owner times Left and stops it; once both phones have
    /// synced, the caregiver taps Right inside the break. Right joins the
    /// owner's Left, and both phones show one feed of two parts.
    @Test func theOtherPhonesRightJoinsLeftInItsBreak() async throws {
        guard let environment = E2EEnvironment.current() else { return }
        let race = try await Race(environment: environment)
        defer { race.tearDown() }
        let (owner, caregiver, baby, clock) = (race.owner, race.caregiver, race.babyID, race.clock)

        let left = try owner.store.startPart(.breast(.left), for: baby)
        clock.advance(by: 60)
        let ownerLeft = try #require(try owner.store.stopFeed(left))
        try await owner.sync(.localWrite)
        try await caregiver.sync()

        clock.advance(by: 120)
        let right = try caregiver.store.startPart(.breast(.right), for: baby)
        #expect(right.sessionID == ownerLeft.effectiveFeedingSessionID, "Right did not join the owner's Left")
        try await caregiver.sync(.localWrite)
        try await owner.sync()
        let ownersRight = try #require(try owner.store.activeTimer(id: right.id), "the owner never saw Right")
        #expect(ownersRight.sessionID == ownerLeft.effectiveFeedingSessionID)

        clock.advance(by: 90)
        _ = try caregiver.store.stopFeed(try #require(try caregiver.store.activeTimer(id: right.id)))
        try await caregiver.sync(.localWrite)
        try await owner.sync()

        let shapes = try expectOneSessionOfLeftThenRight(on: [owner, caregiver], baby: baby)
        print("cross-phone join proof: owner's Left, caregiver's Right in the break; \(shapes)")
    }

    /// The walkthrough's scenario 5 across two phones: the owner's sleep
    /// ends, and the caregiver starts the next one inside the 15-minute
    /// break. Both phones show one sleep of two parts.
    @Test func aSleepStartedOnTheOtherPhoneAfterAShortWakeJoins() async throws {
        guard let environment = E2EEnvironment.current() else { return }
        let race = try await Race(environment: environment)
        defer { race.tearDown() }
        let (owner, caregiver, baby, clock) = (race.owner, race.caregiver, race.babyID, race.clock)

        let first = try owner.store.startSleep(for: baby)
        clock.advance(by: 1_800)
        let ownerFirst = try owner.store.stopSleep(first)
        try await owner.sync(.localWrite)
        try await caregiver.sync()

        clock.advance(by: 600)
        let second = try caregiver.store.startSleep(for: baby)
        #expect(second.sessionID == ownerFirst.effectiveSleepSessionID, "the second sleep did not join")
        try await caregiver.sync(.localWrite)
        try await owner.sync()

        clock.advance(by: 1_200)
        _ = try caregiver.store.stopSleep(try #require(try caregiver.store.activeTimer(id: second.id)))
        try await caregiver.sync(.localWrite)
        try await owner.sync()

        var shapes: [String] = []
        for side in [owner, caregiver] {
            #expect(try side.store.activeTimers(for: baby).isEmpty, "\(side.name) still has a running timer")
            let sessions = try side.store.sleepSessions(for: baby, from: .distantPast, to: .distantFuture)
            #expect(sessions.count == 1, "\(side.name) has \(sessions.count) sleep sessions")
            let session = try #require(sessions.first)
            #expect(session.sleepIDs.count == 2, "\(side.name)'s sleep has \(session.sleepIDs.count) parts")
            #expect(session.id == ownerFirst.effectiveSleepSessionID)
            #expect(session.asleepSeconds == 3_000, "\(side.name) counts \(session.asleepSeconds) s asleep")
            #expect(try side.store.nextDispatchBatch(limit: 100).isEmpty, "\(side.name)'s outbox did not drain")
            shapes.append("\(side.name)=\(session.sleepIDs.count) parts session=\(session.id) asleep=\(session.asleepSeconds)s")
        }
        print("cross-phone sleep join proof: \(shapes.joined(separator: "; "))")
    }

    /// A snapshot carrying running timers, applied by the client.
    ///
    /// Since the PostgreSQL authority redesign (Phase 2), snapshot pages serve
    /// a running timer's stored wire verbatim. The decoder already stores the
    /// start (and a running side's start) at whole microseconds (`.250000Z`),
    /// but a running bottle's start and any part bounds stay exactly as the
    /// phone sent them (`.750Z`), where the legacy snapshot re-rendered them
    /// (`.750000Z`). The caregiver joins while the owner's sleep and bottle
    /// timers run, so its join snapshot (`activateAsJoiner` → `takeSnapshot`)
    /// is where both timers first reach it. Both must appear exactly as the
    /// owner has them, also in a store built from a fresh snapshot alone, and
    /// the joined timer must still work: the caregiver stops the bottle and
    /// the owner gets the record.
    @Test func aJoinersSnapshotCarriesRunningTimers() async throws {
        guard let environment = E2EEnvironment.current() else { return }
        var timers: (sleep: UUID, bottle: UUID)?
        let race = try await Race(environment: environment) { owner, baby, clock in
            // Fractions of a second, so no timestamp is a whole second.
            clock.advance(by: 0.25)
            let sleep = try owner.store.startSleep(for: baby)
            clock.advance(by: 30.5)
            let bottle = try owner.store.startPart(.bottle(.formula), for: baby)
            try await owner.sync(.localWrite)
            timers = (sleep.id, bottle.id)
        }
        defer { race.tearDown() }
        let (owner, caregiver, baby, clock) = (race.owner, race.caregiver, race.babyID, race.clock)
        let (sleepID, bottleID) = try #require(timers)
        let joinPulls = caregiver.traffic.syncCursors

        let fresh = try await caregiver.freshSnapshot()
        // Nothing has written to the family since the join, so a fresh
        // snapshot records the join snapshot's high water: the join's first
        // pull must start exactly there, which only the snapshot can have set.
        let snapshotCursor = try fresh.syncCursor()
        let highWater = UInt64(try #require(snapshotCursor?.sequence, "the fresh snapshot recorded no high water"))
        #expect(joinPulls.first.flatMap { $0 } == highWater, "the join pulled from \(joinPulls), not from the snapshot's high water \(highWater)")
        var served: [String] = []
        for id in [sleepID, bottleID] {
            let sent = try #require(try owner.store.activeTimer(id: id), "the owner lost timer \(id)")
            let base = try #require(try fresh.context.fetch(FetchDescriptor<CanonicalEntityBase>()).first { $0.entityID == id },
                                    "the snapshot did not carry timer \(id)")
            let wire = try #require(try JSONSerialization.jsonObject(with: base.payload) as? [String: Any])
            let servedStart = try #require(wire["started_at"] as? String)
            #expect(abs(try BabyStore.wireDate(servedStart).timeIntervalSince(sent.startedAt)) < 0.001,
                    "the snapshot served started_at \(servedStart) for \(MutationFactory.wireTime(sent.startedAt))")
            var line = "\(sent.kind.rawValue)=\(id) started_at=\(servedStart)"
            if let sentBottle = sent.currentBottleStartedAt {
                let sentBottleWire = MutationFactory.wireTime(sentBottle)
                #expect(!sentBottleWire.hasSuffix(".000Z"), "the owner sent a whole second: \(sentBottleWire)")
                let servedBottle = try #require(wire["current_bottle_started_at"] as? String)
                #expect(servedBottle == sentBottleWire, "the snapshot served current_bottle_started_at \(servedBottle), the owner sent \(sentBottleWire)")
                line += " current_bottle_started_at=\(servedBottle) (sent \(sentBottleWire))"
            }
            for (store, label) in [(caregiver.store, "the joined caregiver"), (fresh, "a fresh-snapshot store")] {
                let applied = try #require(try store.activeTimer(id: id), "\(label) has no running timer \(id)")
                #expect(applied.kind == sent.kind && applied.babyID == sent.babyID && applied.sessionID == sent.sessionID)
                #expect(abs(applied.startedAt.timeIntervalSince(sent.startedAt)) < 0.001, "\(label) shows \(applied.startedAt), not \(sent.startedAt)")
                #expect(applied.currentSide == sent.currentSide)
                #expect((applied.currentBottleStartedAt == nil) == (sent.currentBottleStartedAt == nil))
                if let appliedBottle = applied.currentBottleStartedAt, let sentBottle = sent.currentBottleStartedAt {
                    #expect(abs(appliedBottle.timeIntervalSince(sentBottle)) < 0.001, "\(label) shows the bottle started at \(appliedBottle), not \(sentBottle)")
                }
            }
            served.append(line)
        }

        clock.advance(by: 240)
        let joinedBottle = try #require(try caregiver.store.activeTimer(id: bottleID))
        let stopped = try #require(try caregiver.store.stopBottle(joinedBottle, volumeML: 60))
        try await caregiver.sync(.localWrite)
        try await owner.sync()
        for side in [owner, caregiver] {
            #expect(try side.store.activeTimer(id: bottleID) == nil, "\(side.name) still runs the stopped bottle")
            #expect(try side.store.activeTimer(id: sleepID) != nil, "\(side.name) lost the running sleep")
            let feeds = try side.store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture)
            #expect(feeds.count == 1 && feeds.first?.feedIDs == [stopped.id], "\(side.name) has feeds \(feeds.map(\.feedIDs)), not [\(stopped.id)]")
            #expect(try side.store.nextDispatchBatch(limit: 100).isEmpty, "\(side.name)'s outbox did not drain")
        }
        print("running-timer snapshot proof: the caregiver joined while the owner's timers ran; the join pulled from cursor=\(joinPulls.first.flatMap { $0 }.map(String.init) ?? "-"), the snapshot's high water \(highWater); snapshot served \(served.joined(separator: ", ")); joined phone and fresh-snapshot store show both timers identically; the caregiver stopped the joined bottle (\(stopped.id)) and the owner has it")
    }

    /// Both phones: nothing running, exactly one session holding exactly two
    /// parts, Left then Right, and an outbox with nothing left to send.
    private func expectOneSessionOfLeftThenRight(on phones: [Phone], baby: UUID) throws -> String {
        var shapes: [String] = []
        for side in phones {
            #expect(try side.store.activeTimers(for: baby).isEmpty, "\(side.name) still has a running timer")
            let sessions = try side.store.feedingSessions(for: baby, from: .distantPast, to: .distantFuture)
            #expect(sessions.count == 1, "\(side.name) has \(sessions.count) sessions")
            let session = try #require(sessions.first)
            let parts = session.feedIDs.compactMap { id -> FeedEntry? in
                guard case .feed(let feed) = try? side.store.entry(id: id, for: baby) else { return nil }
                return feed
            }
            #expect(parts.count == 2, "\(side.name)'s session has \(parts.count) parts")
            let sides = parts.sorted { $0.startedAt < $1.startedAt }.flatMap { $0.components.compactMap(\.breastSide) }
            #expect(sides == [.left, .right], "\(side.name)'s parts are \(sides)")
            #expect(try side.store.nextDispatchBatch(limit: 100).isEmpty, "\(side.name)'s outbox did not drain")
            shapes.append("\(side.name)=\(sides.map(\.rawValue)) session=\(session.id)")
        }
        return shapes.joined(separator: "; ")
    }

    /// The outbox row of `timerID`'s stop on `phone`.
    private func timerStop(of timerID: UUID, on phone: Phone) throws -> SyncOutboxEntry? {
        try phone.store.context.fetch(FetchDescriptor<SyncOutboxEntry>()).first {
            $0.entityKind == .timer && $0.operation == .timerStop && $0.entityID == timerID
        }
    }
}
