import BabyCore
import BabyProtocol
@testable import BabyStore
@testable import BabySync
import Foundation
import SwiftData
import Testing
@testable import SettingsFeature

/// Pumping through the real server, on two phones built from the app's own
/// composition (`Race`). Each step prints one `pumping proof:` line; the
/// script requires the last.
@MainActor
@Suite("Pumping between two caregivers", .serialized)
struct PumpSyncE2ETests {
    @Test func pumpsReachTheFamilyRunBesideABottleAndSurviveTheRaceASnapshotAndABabyDelete() async throws {
        guard let environment = E2EEnvironment.current() else { return }
        func line(_ step: Int, _ text: String) { print("pumping proof: step \(step): \(text)") }
        func all(_ phone: Phone) throws -> [PumpEntry] { try phone.store.pumps(from: .distantPast, to: .distantFuture) }

        // Review Focus 4. The owner pumped before there was a family.
        var early: UUID?
        let race = try await Race(environment: environment, beforeFamily: { owner, _, clock in
            early = try owner.store.logPump(side: .left, from: clock.now.addingTimeInterval(-1800),
                                            to: clock.now.addingTimeInterval(-1200), volumeML: 40, note: "before the family").id
        })
        defer { race.tearDown() }
        let (owner, caregiver, baby, clock) = (race.owner, race.caregiver, race.babyID, race.clock)
        let earlyID = try #require(early)

        try await owner.sync(.localWrite)
        try await caregiver.sync()
        let arrived = try #require(try caregiver.store.pump(id: earlyID), "the pump logged before the family never reached the caregiver")
        #expect(arrived.side == .left)
        #expect(arrived.volumeML == 40)
        #expect(arrived.note == "before the family")
        #expect(arrived.endedAt != nil)
        #expect(try owner.store.nextDispatchBatch(limit: 100).isEmpty, "the owner's outbox did not drain after the import")
        line(1, "a pump the owner logged before creating the family reached the caregiver as a plain create (40 ml, left)")

        // A bottle runs on the owner's phone the whole time.
        let bottle = try owner.store.startPart(.bottle(.formula), for: baby)
        try await owner.sync(.localWrite)
        try await caregiver.sync()
        try #require(try caregiver.store.activeTimer(kind: .feed, for: baby)?.id == bottle.id)

        // Review Focus 1. Both phones start a pump before either has synced.
        clock.advance(by: 60)
        let theirs = try caregiver.store.startPump(side: .both)
        let mine = try owner.store.startPump(side: .left)
        let losingStart = try #require(try owner.store.liveOutboxRows().first { $0.entityID == mine.id })
        try await caregiver.sync(.localWrite)
        // The owner has already stopped its own by the time it syncs.
        clock.advance(by: 300)
        try owner.store.stopPump(mine)
        try owner.store.setPumpAmount(mine, volumeML: 30)
        try await owner.sync(.localWrite)
        try await owner.sync(.localWrite)
        #expect(losingStart.deliveryState == .superseded, "the owner's losing start settled as \(losingStart.deliveryState)")
        #expect(losingStart.supersededCode == "pump_running")
        #expect(try owner.store.runningPump()?.id == theirs.id, "the owner does not show the caregiver's running pump")
        #expect(throws: PumpError.alreadyRunning) { try owner.store.startPump(side: .left) }
        let kept = try #require(try owner.store.pump(id: mine.id), "the owner's finished session was dropped")
        #expect(kept.endedAt != nil)
        #expect(kept.volumeML == 30)
        #expect(try owner.store.nextDispatchBatch(limit: 100).isEmpty, "the owner's finished session was not delivered")
        try await caregiver.sync()
        let crossed = try #require(try caregiver.store.pump(id: mine.id), "the owner's finished session never reached the caregiver")
        #expect(crossed.volumeML == 30)
        #expect(crossed.side == .left)
        #expect(try caregiver.store.runningPump()?.id == theirs.id)
        line(2, "both phones started a pump; the caregiver's reached the server first and runs on both; the owner's start was superseded (pump_running) and its already-stopped session was sent again and kept, 30 ml")

        // The bottle never noticed.
        for phone in [owner, caregiver] {
            #expect(try phone.store.activeTimer(kind: .feed, for: baby)?.id == bottle.id, "\(phone.name) lost the running bottle")
        }
        line(3, "the bottle timer \(bottle.id) is still running on both phones beside the pump")

        // The other phone edits the running session, then stops it with an amount.
        let running = try #require(try owner.store.runningPump())
        try owner.store.updatePump(running, side: .right, startedAt: running.startedAt.addingTimeInterval(-120), endedAt: nil, volumeML: nil, note: "owner moved the start")
        try await owner.sync(.localWrite)
        try await caregiver.sync()
        #expect(theirs.side == .right)
        #expect(theirs.note == "owner moved the start")
        #expect(theirs.isRunning)
        clock.advance(by: 900)
        try caregiver.store.stopPump(theirs)
        try caregiver.store.setPumpAmount(theirs, volumeML: 90)
        try await caregiver.sync(.localWrite)
        try await owner.sync()
        for phone in [owner, caregiver] {
            let stored = try #require(try phone.store.pump(id: theirs.id), "\(phone.name) lost the pump")
            #expect(stored.endedAt != nil && stored.volumeML == 90 && stored.side == .right,
                    "\(phone.name) pump ended=\(String(describing: stored.endedAt)) ml=\(String(describing: stored.volumeML)) side=\(stored.sideRaw)")
            #expect(try phone.store.runningPump() == nil, "\(phone.name) still shows a running pump")
            #expect(Set(try all(phone).map(\.id)) == [earlyID, mine.id, theirs.id])
            #expect(try phone.store.nextDispatchBatch(limit: 100).isEmpty, "\(phone.name)'s outbox did not drain")
            #expect(try phone.store.activeTimer(kind: .feed, for: baby)?.id == bottle.id, "\(phone.name) lost the running bottle")
        }
        line(4, "the owner edited the caregiver's running pump (side, start, note); the caregiver stopped it at 90 ml; both phones agree on three finished sessions and no running one")

        let fresh = try await owner.freshSnapshot()
        let snapshot = try fresh.pumps(from: .distantPast, to: .distantFuture)
        #expect(Set(snapshot.map(\.id)) == [earlyID, mine.id, theirs.id])
        #expect(snapshot.first { $0.id == theirs.id }?.volumeML == 90)
        #expect(snapshot.first { $0.id == mine.id }?.volumeML == 30)
        #expect(snapshot.first { $0.id == earlyID }?.note == "before the family")
        line(5, "a fresh snapshot holds exactly the three sessions with their amounts")

        // Deleting the baby never deletes pumping history.
        try owner.store.deleteBaby(try #require(try owner.store.baby(baby)))
        try await owner.sync(.localWrite)
        try await caregiver.sync()
        for phone in [owner, caregiver] {
            #expect(Set(try all(phone).map(\.id)) == [earlyID, mine.id, theirs.id], "\(phone.name) lost pumps with the baby")
        }
        let afterDelete = try await caregiver.freshSnapshot()
        #expect(try afterDelete.pumps(from: .distantPast, to: .distantFuture).count == 3)
        line(6, "the baby was deleted; all three sessions are still on both phones and in a fresh snapshot")
    }
}
