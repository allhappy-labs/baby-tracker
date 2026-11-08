import BabyCore
import BabyProtocol
@testable import BabyStore
import Foundation
import SwiftData
import Testing
@testable import BabySync
import HomeFeature
import LoggingFeature
@testable import SettingsFeature

/// Two phones, one family, the real server — the gate before real phones.
///
/// Every other sync test in this package runs against a fake server, and each
/// fake was written from someone's reading of the Go code. This one runs the
/// app's own object graph (`FamilySyncComposition.assemble`, the same function
/// `make` calls) twice, against the container `protocol-e2e.sh` built from this
/// tree, over TLS. Tokens come out of a Keychain store through
/// `KeychainTokenProvider` and `CurrentSessionKey`, get renewed by
/// `FamilySessionRenewer` over `HTTPIdentityClient`, and every record goes
/// through each store's outbox and remote apply.
///
/// Both phones sign in through the app's own path — the Caregivers screen's
/// name first, then `signInWithApple`: server probe, `POST /v1/auth/challenge`,
/// `POST /v1/auth/apple` over `HTTPIdentityClient`. Only Apple itself is not
/// the phone's: the proof comes from the server's identity-test adapter,
/// signed for the nonce the client computed, exactly where Apple's would.
///
/// Each step prints one `two-caregiver proof:` line, so the script's output is
/// the record of what was proved.
@MainActor
@Suite("Two caregivers through a live server", .serialized)
struct TwoCaregiverSyncE2ETests {
    @Test func twoPhonesShareOneFamilyThroughTheRealServer() async throws {
        guard let environment = E2EEnvironment.current() else { return }
        let live = try LiveServer(environment)
        // Both phones read one clock, set in the past so every timestamp the
        // server sees has already happened, and moved only when a step says so.
        // A fixed fraction exercises timestamp round-trip precision on every
        // run instead of letting Date() randomly hide a one-millisecond drift.
        let clock = TestDateProvider(Date(timeIntervalSince1970: floor(Date().timeIntervalSince1970) - 3 * 3600 + 0.008))
        let scratch = FileManager.default.temporaryDirectory
            .appending(path: "two-caregiver-e2e-\(UUID().uuidString)", directoryHint: .isDirectory)
        try FileManager.default.createDirectory(at: scratch, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: scratch) }

        // A brand-new Apple account for the owner — the first sign-in, which
        // the server refuses without a profile — and, for the joiner, the
        // account the Go bootstrap already registered, signing in from a new
        // phone.
        let owner = try Phone(name: "owner", live: live, clock: clock, directory: scratch, appleSubject: UUID().uuidString.lowercased())
        var joiner = try Phone(name: "joiner", live: live, clock: clock, directory: scratch, appleSubject: LiveServer.outsider2Subject)
        defer { owner.forgetDefaults(); joiner.forgetDefaults() }
        // Every phone this test builds, so a throw from any step can stop
        // every engine before the scratch directory and Keychain go away.
        // The deletion above is only the last resort if that explicit
        // cleanup itself throws before it gets a chance to run — otherwise a
        // catch-up or hint sync still reading the store would log a
        // misleading CoreData disk I/O error instead of the real failure.
        var phones: [Phone] = [owner, joiner]

        do {
        // MARK: 1. The owner's history, tracked before there was a family.

        let baby = try owner.store.addBaby(name: "Zoë Мія 🌙", birthDate: clock.now.addingTimeInterval(-40 * 86_400), referenceSex: .girls, expectedDueDate: nil)
        let localMeasurement = try owner.store.logMeasurement(for: baby.id, at: clock.now, timeZoneID: "Europe/Zurich",
            values: MeasurementValues(weightKG: 4.125, lengthCM: 54, headCircumferenceCM: 35, lengthPosition: .lying))
        // A composite feed, as an older build's timer (one running part,
        // a side switch, two bottles, a stop) would have produced it.
        // One-part timers never build this shape live anymore — each part
        // gets its own timer — so it is built directly here, the same shape
        // remote apply (`RemoteTimerState`) leaves a legacy timer's stop in.
        let compositeStart = clock.now
        let leftEnd = compositeStart.addingTimeInterval(8 * 60)
        let rightEnd = leftEnd.addingTimeInterval(6 * 60)
        let composite = FeedEntry(babyID: baby.id, kind: .breast, startedAt: compositeStart,
                                  timeZoneID: "Europe/Zurich", createdBy: "owner-phone", realm: .local)
        composite.components = [
            .breast(id: UUID(), side: .left, seconds: 480, startedAt: compositeStart, endedAt: leftEnd),
            .breast(id: UUID(), side: .right, seconds: 360, startedAt: leftEnd, endedAt: rightEnd),
            .bottle(id: UUID(), volumeML: 50, contents: .breastMilk),
            .bottle(id: UUID(), volumeML: 30, contents: .formula),
        ]
        composite.endedAt = rightEnd
        owner.store.context.insert(composite)
        try owner.store.record(.create, on: composite)
        clock.advance(by: 14 * 60)
        #expect(composite.components.map(Shape.init) == [
            .breast(.left, 480), .breast(.right, 360), .bottle(50, .breastMilk), .bottle(30, .formula),
        ])
        // A second feed of untimed parts: two sides and a bottle.
        clock.advance(by: 20 * 60)
        let untimed = FeedEntry(
            babyID: baby.id, kind: .breast, startedAt: clock.now,
            timeZoneID: "Europe/Zurich", createdBy: "owner-phone", realm: .local
        )
        untimed.endedAt = clock.now.addingTimeInterval(15 * 60)
        untimed.components = [
            .breast(id: UUID(), side: .right, seconds: 420),
            .breast(id: UUID(), side: .left, seconds: 300),
            .bottle(id: UUID(), volumeML: 60, contents: .formula),
        ]
        owner.store.context.insert(untimed)
        try owner.store.record(.create, on: untimed)
        clock.advance(by: 30 * 60)
        let nap = try owner.store.logSleep(from: clock.now.addingTimeInterval(-50 * 60), to: clock.now, for: baby.id)
        let diaper = try owner.store.logDiaper(.mixed, for: baby.id, consistency: "soft")
        let ownerHistory = try owner.history()
        try #require(ownerHistory.babies.count == 1 && ownerHistory.feeds.count == 2)
        try #require(ownerHistory.sleeps.count == 1 && ownerHistory.diapers.count == 1)
        proof(1, "owner logged locally: baby=\(baby.id) composite=\(composite.id) untimed=\(untimed.id) sleep=\(nap.id) diaper=\(diaper.id) realm=\(owner.store.activeRealm)")

        // MARK: 2-3. The owner signs in, creates the family and brings that history.

        let ownerAccount = try await owner.signIn(displayName: "Owner Zoë", live: live)
        #expect(owner.model.profile.displayName == "Owner Zoë")
        let ownerSeed = try owner.accountCredentials(ownerAccount)
        proof(2, "owner signed in through GET /v1/server + POST /v1/auth/challenge + POST /v1/auth/apple as a new account (profile sent, installation_id=\(try owner.installationID()), server mode=\(try await live.authorizationMode())): account=\(ownerAccount.accountID) device=\(ownerAccount.deviceID)")
        try await owner.model.createFamily(name: "E2E two caregivers")
        let ownerKey = try owner.familyKey()
        try owner.expectUpToDate("after creating the family and importing")
        let ownerRenewed = try owner.credentials(ownerKey)
        #expect(ownerRenewed.refreshToken != ownerSeed.refreshToken, "the owner's pre-family session was never renewed")
        #expect(owner.store.activeRealm == ownerKey.realmID)
        #expect(try owner.store.nextDispatchBatch(limit: 100).isEmpty, "the import left mutations unsent")
        proof(3, "owner created family=\(ownerKey.familyID), imported history, renewed to a family session via FamilySessionRenewer (refresh token rotated), status=\(owner.statusText)")

        // MARK: 4-5. An invitation, and the joiner signs in and redeems it.

        let invitation = try await owner.model.createInvitation()
        let parsed = try InvitationParser.parse(invitation.shareURL.absoluteString)
        #expect(parsed.token.count == 43)
        #expect(parsed.origin == live.endpoint.origin, "the link names \(parsed.origin.canonicalString), not the confirmed server")
        let joinerAccount = try await joiner.signIn(displayName: "Joiner Two", live: live)
        let bootstrapped = try live.bootstrapSession("Outsider2")
        #expect(joinerAccount.accountID == bootstrapped.accountID, "a returning Apple account became a new one")
        #expect(joinerAccount.deviceID != bootstrapped.deviceID, "a new phone reused another device's binding")
        proof(4, "joiner signed in through the same path to the existing account \(joinerAccount.accountID) as a new device \(joinerAccount.deviceID)")
        try await joiner.model.acceptInvitation(parsed)
        let joinerKey = try joiner.familyKey()
        #expect(joinerKey.familyID == ownerKey.familyID)
        try joiner.expectUpToDate("after joining and taking the snapshot")
        #expect(joiner.store.activeRealm == joinerKey.realmID)
        proof(5, "owner minted invitation link=\(invitation.shareURL.scheme ?? "")://\(invitation.shareURL.host ?? "")\(invitation.shareURL.path) parsed_origin=\(parsed.origin.canonicalString); joiner parsed it with InvitationParser, redeemed it, renewed and took the snapshot, status=\(joiner.statusText)")

        // MARK: 6. The joiner has the owner's history, identically.

        let arrived = try joiner.history()
        try #require(arrived.babies.count == 1, "babies=\(arrived.babies.map(\.name))")
        #expect(arrived.babies[0].id == baby.id)
        #expect(arrived.babies[0].name == "Zoë Мія 🌙")
        #expect(arrived.babies[0].birthDateISO == baby.birthDateISO)
        let arrivedComposite = try #require(arrived.feeds.first { $0.id == composite.id })
        #expect(arrivedComposite.components == composite.components)
        #expect(arrivedComposite.components.map(Shape.init) == [
            .breast(.left, 480), .breast(.right, 360), .bottle(50, .breastMilk), .bottle(30, .formula),
        ])
        #expect(same(arrivedComposite.startedAt, composite.startedAt))
        #expect(same(arrivedComposite.endedAt, composite.endedAt))
        let arrivedUntimed = try #require(arrived.feeds.first { $0.id == untimed.id })
        #expect(arrivedUntimed.components == untimed.components, "untimed feed arrived as \(arrivedUntimed.components)")
        #expect(same(arrivedUntimed.startedAt, untimed.startedAt) && same(arrivedUntimed.endedAt, untimed.endedAt))
        #expect(arrivedUntimed.timeZoneID == "Europe/Zurich")
        let arrivedNap = try #require(arrived.sleeps.first { $0.id == nap.id })
        #expect(same(arrivedNap.startedAt, nap.startedAt) && same(arrivedNap.endedAt, nap.endedAt))
        let arrivedDiaper = try #require(arrived.diapers.first { $0.id == diaper.id })
        #expect(arrivedDiaper.contents == .mixed && arrivedDiaper.consistency == "soft")
        #expect(same(arrivedDiaper.startedAt, diaper.startedAt))
        #expect(arrived.feeds.count == 2 && arrived.sleeps.count == 1 && arrived.diapers.count == 1)
        proof(6, "joiner received baby name=\"\(arrived.babies[0].name)\", composite components=\(arrivedComposite.components.map(Shape.init)), untimed components=\(arrivedUntimed.components.map(Shape.init)), sleep, diaper — ids and content identical")

        // MARK: 7. The owner edits an imported record straight away.

        let ownerDiaper = try #require(try owner.history().diapers.first { $0.id == diaper.id })
        let noteEdit = DiaperEditModel(store: owner.store, babyID: baby.id, diaperID: ownerDiaper.id)
        noteEdit.note = "blowout, changed outfit"
        try noteEdit.save()
        try await owner.sync(.localWrite)
        try await joiner.sync()
        let editedDiaper = try #require(try joiner.history().diapers.first { $0.id == diaper.id })
        #expect(editedDiaper.note == "blowout, changed outfit")
        proof(7, "owner edited the imported diaper's note; joiner sees note=\"\(editedDiaper.note ?? "")\"")

        // MARK: 8. The joiner corrects a bottle, deletes the diaper, starts a nap.

        let formulaID = try #require(composite.components.first { $0.bottleContents == .formula }?.id)
        let joinerFeed = try #require(try joiner.history().feeds.first { $0.id == composite.id })
        let bottleEdit = PartEditModel(store: joiner.store, babyID: baby.id,
                                       target: .bottle(feedID: joinerFeed.id, componentID: formulaID))
        bottleEdit.volumeML = 40
        #expect(try bottleEdit.save(), "the bottle editor refused: \(bottleEdit.validationMessage ?? "-")")
        let joinerDiaper = try #require(try joiner.history().diapers.first { $0.id == diaper.id })
        try DiaperEditModel(store: joiner.store, babyID: baby.id, diaperID: joinerDiaper.id).delete()
        clock.advance(by: 10 * 60)
        let sleepTimer = try joiner.store.startSleep(for: baby.id)
        try await joiner.sync(.localWrite)
        try await owner.sync()
        let ownerView = try owner.history()
        let correctedFeed = try #require(ownerView.feeds.first { $0.id == composite.id })
        #expect(correctedFeed.components.first { $0.id == formulaID }?.volumeML == 40)
        #expect(correctedFeed.components.map(Shape.init) == [
            .breast(.left, 480), .breast(.right, 360), .bottle(50, .breastMilk), .bottle(40, .formula),
        ])
        #expect(ownerView.diapers.isEmpty, "the diaper the joiner deleted is still on the owner's phone")
        let mirroredSleep = try #require(try owner.store.activeTimer(kind: .sleep, for: baby.id))
        #expect(mirroredSleep.id == sleepTimer.id)
        #expect(same(mirroredSleep.startedAt, sleepTimer.startedAt))
        proof(8, "joiner set formula bottle to 40 ml, deleted the diaper, started a nap; owner sees components=\(correctedFeed.components.map(Shape.init)), diapers=\(ownerView.diapers.count), running sleep timer=\(mirroredSleep.id) with the same start")

        // MARK: 9. The owner stops the nap.

        clock.advance(by: 45 * 60)
        let finishedNap = try owner.store.stopSleep(mirroredSleep)
        try await owner.sync(.localWrite)
        try await joiner.sync()
        #expect(try joiner.store.activeTimers(for: baby.id).isEmpty)
        #expect(try owner.store.activeTimers(for: baby.id).isEmpty)
        for phone in [owner, joiner] {
            let fromTimer = try phone.history().sleeps.filter { same($0.startedAt, sleepTimer.startedAt) }
            #expect(fromTimer.count == 1, "\(phone.name) has \(fromTimer.count) sleeps from the timer")
            #expect(fromTimer.first?.id == finishedNap.id)
            #expect(same(fromTimer.first?.endedAt, finishedNap.endedAt))
            #expect(try phone.history().sleeps.count == 2)
        }
        proof(9, "owner stopped the nap; both phones: no running timer, exactly one completed sleep \(finishedNap.id) of \(finishedNap.durationSeconds ?? -1) s")

        // MARK: 10. A feed timer the owner starts.
        //
        // One-part timers give each side and bottle its own timer, so a
        // live timer carrying a switch and a top-up (as an older build's
        // would) is no longer reachable from a current phone — only the
        // start syncs live here now.

        clock.advance(by: 30 * 60)
        let liveFeed = try owner.store.startPart(.breast(.left), for: baby.id)
        try await owner.sync(.localWrite)
        clock.advance(by: 7 * 60)
        try await joiner.sync()
        let mirroredFeed = try #require(try joiner.store.activeTimer(kind: .feed, for: baby.id))
        #expect(mirroredFeed.id == liveFeed.id)
        #expect(same(mirroredFeed.startedAt, liveFeed.startedAt))
        #expect(mirroredFeed.currentSide == liveFeed.currentSide)
        proof(10, "owner started a feed timer; joiner sees timer=\(mirroredFeed.id) running side=\(mirroredFeed.currentSide?.rawValue ?? "-")")

        // MARK: 11. The joiner corrects the running feed's start through the editor.

        let startBeforeEdit = mirroredFeed.startedAt
        let editedStart = startBeforeEdit.addingTimeInterval(-2 * 60)
        let timerEdit = RunningTimerEditModel(timer: mirroredFeed, store: joiner.store)
        timerEdit.startedAt = editedStart
        #expect(timerEdit.canSave, "the editor refused: \(timerEdit.validationMessage ?? "-")")
        #expect(try await timerEdit.save(), "the store refused: \(timerEdit.validationMessage ?? "-")")
        try await joiner.sync(.localWrite)
        try await owner.sync()
        let editedOnOwner = try #require(try owner.store.activeTimer(kind: .feed, for: baby.id))
        #expect(editedOnOwner.id == liveFeed.id)
        #expect(same(editedOnOwner.startedAt, editedStart), "owner's start is \(editedOnOwner.startedAt), edited \(editedStart)")
        proof(11, "joiner edited the running feed's start with RunningTimerEditModel (2 min earlier); owner sees timer=\(editedOnOwner.id) start moved by \(Int(startBeforeEdit.timeIntervalSince(editedOnOwner.startedAt))) s")

        // MARK: 12. The joiner stops it.

        clock.advance(by: 60)
        let stoppedFeed = try #require(try joiner.store.stopFeed(mirroredFeed))
        try await joiner.sync(.localWrite)
        try await owner.sync()
        #expect(try owner.store.activeTimers(for: baby.id).isEmpty)
        #expect(try joiner.store.activeTimers(for: baby.id).isEmpty)
        // Found by the edited start: a feed at the original one would be the
        // edit lost on the way.
        let ownerFromTimer = try owner.history().feeds.filter { same($0.startedAt, editedStart) }
        let joinerFromTimer = try joiner.history().feeds.filter { same($0.startedAt, editedStart) }
        #expect(ownerFromTimer.count == 1 && joinerFromTimer.count == 1)
        #expect(try owner.history().feeds.filter { same($0.startedAt, startBeforeEdit) }.isEmpty)
        #expect(ownerFromTimer.first?.id == joinerFromTimer.first?.id)
        #expect(ownerFromTimer.first?.components == joinerFromTimer.first?.components)
        #expect(same(ownerFromTimer.first?.endedAt, joinerFromTimer.first?.endedAt))
        proof(12, "joiner stopped the edited feed; both phones have exactly one completed feed starting at the edited start, id=\(ownerFromTimer.first?.id.uuidString ?? "-") (joiner's local id \(stoppedFeed.id)) with components=\(ownerFromTimer.first?.components.map(Shape.init) ?? [])")

        // The logical link and an exact step correction must survive both
        // caregivers' ordinary outbox updates and a fresh server snapshot.
        try owner.store.combineFeeds([composite.id, stoppedFeed.id], for: baby.id)
        try await owner.sync(.localWrite)
        try await joiner.sync()
        let joinerGroups = try joiner.store.feedingSessions(for: baby.id,
            from: composite.startedAt.addingTimeInterval(-1), to: stoppedFeed.startedAt.addingTimeInterval(1))
        #expect(joinerGroups.contains { Set($0.feedIDs) == Set([composite.id, stoppedFeed.id]) })
        let breastStep = try #require(try owner.history().feeds.first { $0.id == stoppedFeed.id }?.components.first { $0.breastSide == .left })
        let originalStepEnd = try #require(breastStep.endedAt)
        let stepEditor = PartEditModel(store: owner.store, babyID: baby.id,
                                       target: .breast(feedID: stoppedFeed.id, componentID: breastStep.id))
        stepEditor.end = originalStepEnd.addingTimeInterval(-60)
        #expect(try stepEditor.save(), "the part editor refused: \(stepEditor.validationMessage ?? "-")")
        try await owner.sync(.localWrite)
        try await joiner.sync()
        let joinerCorrected = try #require(try joiner.history().feeds.first { $0.id == stoppedFeed.id })
        #expect(same(joinerCorrected.components.first { $0.id == breastStep.id }?.endedAt, originalStepEnd.addingTimeInterval(-60)))
        #expect(try joiner.store.feedingSessions(for: baby.id,
            from: composite.startedAt.addingTimeInterval(-1), to: stoppedFeed.startedAt.addingTimeInterval(1))
            .contains { Set($0.feedIDs) == Set([composite.id, stoppedFeed.id]) })

        // Both caregivers edit the same linked feed from the same observed
        // version, then send their outboxes concurrently. The server may pick
        // either note, but neither path may create, remove, or unlink a feed.
        let ownerConcurrentEdit = PartEditModel(store: owner.store, babyID: baby.id,
                                                target: .breast(feedID: stoppedFeed.id, componentID: breastStep.id))
        ownerConcurrentEdit.note = "owner concurrent note"
        #expect(try ownerConcurrentEdit.save())
        let joinerConcurrentEdit = PartEditModel(store: joiner.store, babyID: baby.id,
                                                 target: .breast(feedID: stoppedFeed.id, componentID: breastStep.id))
        joinerConcurrentEdit.note = "joiner concurrent note"
        #expect(try joinerConcurrentEdit.save())
        let ownerRace = Task { @MainActor in try await owner.sync(.localWrite) }
        let joinerRace = Task { @MainActor in try await joiner.sync(.localWrite) }
        try await ownerRace.value
        try await joinerRace.value
        try await owner.sync()
        try await joiner.sync()
        for phone in [owner, joiner] {
            let matching = try phone.history().feeds.filter { $0.id == stoppedFeed.id }
            #expect(matching.count == 1)
            #expect(same(matching.first?.components.first { $0.id == breastStep.id }?.endedAt,
                         originalStepEnd.addingTimeInterval(-60)))
            #expect(try phone.store.feedingSessions(for: baby.id,
                from: composite.startedAt.addingTimeInterval(-1), to: stoppedFeed.startedAt.addingTimeInterval(1))
                .contains { Set($0.feedIDs) == Set([composite.id, stoppedFeed.id]) })
        }
        let ownerRaceNote = try owner.history().feeds.first { $0.id == stoppedFeed.id }?.note
        let joinerRaceNote = try joiner.history().feeds.first { $0.id == stoppedFeed.id }?.note
        #expect(ownerRaceNote == joinerRaceNote)
        // MARK: 13. The owner deletes a record.

        let ownerUntimed = try #require(try owner.history().feeds.first { $0.id == untimed.id })
        #expect(ownerUntimed.components == untimed.components, "the owner's own untimed row is \(ownerUntimed.components)")
        // Three parts in one entry: History lists it as a session, and its
        // swipe (or the session sheet's Delete feed) deletes the session.
        let untimedSession = SessionSheetModel(store: owner.store, babyID: baby.id, kind: .feed, rootID: ownerUntimed.id)
        try untimedSession.deleteSession()
        try await owner.sync(.localWrite)
        try await joiner.sync()
        #expect(try !joiner.history().feeds.contains { $0.id == untimed.id }, "the joiner still shows the feed the owner deleted")
        proof(13, "owner deleted the untimed feed; joiner no longer has it (feeds=\(try joiner.history().feeds.count))")

        // Supplements are created in the shared realm, after import completed.
        clock.advance(by: 60)
        let vitamin = try owner.store.logSupplement(name: "Vitamin D", for: baby.id)
        let vitaminLoggedAt = vitamin.loggedAt
        try await owner.sync(.localWrite)
        try await joiner.sync()
        let receivedVitamin = try #require(try joiner.supplements().first { $0.id == vitamin.id })
        #expect(receivedVitamin.name == "Vitamin D")
        #expect(same(receivedVitamin.loggedAt, vitaminLoggedAt))
        #expect(try joiner.store.supplementNames(for: baby.id) == ["Vitamin D"])
        clock.advance(by: 60)
        let reused = try joiner.store.logSupplement(name: " vitamin d ", for: baby.id)
        #expect(reused.name == "Vitamin D")
        clock.advance(by: 60)
        let iron = try joiner.store.logSupplement(name: "Iron", for: baby.id)
        let ironLoggedAt = iron.loggedAt
        try await joiner.sync(.localWrite)
        try await owner.sync()
        #expect(try owner.supplements().count == 3)
        #expect(try owner.store.supplementNames(for: baby.id) == ["Iron", "Vitamin D"])

        // Correct the older log after Iron was logged. Neither the later
        // acceptance nor backdated event time may replace original log order.
        clock.advance(by: 60)
        let backdated = vitamin.startedAt.addingTimeInterval(-86_400)
        try joiner.store.editSupplement(receivedVitamin, name: "Vitamin D drops", at: backdated, note: "corrected by caregiver")
        try await joiner.sync(.localWrite)
        try await owner.sync()
        let editReceipt = try #require(try joiner.store.context.fetch(FetchDescriptor<SyncOutboxEntry>()).first {
            $0.entityID == vitamin.id && $0.operationRaw == "update"
        })
        try #require(editReceipt.deliveryState == .acknowledged,
                     "supplement edit state=\(editReceipt.deliveryState) code=\(editReceipt.supersededCode ?? "-") mutation=\(String(data: editReceipt.payload, encoding: .utf8) ?? "-") original=\(MutationFactory.wireTime(vitaminLoggedAt)) received=\(MutationFactory.wireTime(receivedVitamin.loggedAt))")
        let correctedVitamin = try #require(try owner.supplements().first { $0.id == vitamin.id })
        #expect(correctedVitamin.name == "Vitamin D drops")
        #expect(correctedVitamin.note == "corrected by caregiver")
        #expect(same(correctedVitamin.startedAt, backdated))
        #expect(same(correctedVitamin.loggedAt, vitaminLoggedAt))
        for phone in [owner, joiner] {
            #expect(try phone.store.lastSupplement(for: baby.id)?.id == iron.id)
            #expect(try phone.store.supplementNames(for: baby.id) == ["Iron", "Vitamin D", "Vitamin D drops"])
        }
        // Before the one fresh snapshot this account may hold open: the
        // server admits two at a time, and the growth steps take the other.
        let shot = try await immunizationProof(owner: owner, joiner: joiner, baby: baby, clock: clock)
        let fresh = try await joiner.freshSnapshot()
        try immunizationSnapshotProof(fresh, shot: shot, phones: [owner, joiner])
        #expect(try fresh.feedingSessions(for: baby.id,
            from: composite.startedAt.addingTimeInterval(-1), to: stoppedFeed.startedAt.addingTimeInterval(1))
            .contains { Set($0.feedIDs) == Set([composite.id, stoppedFeed.id]) })
        let snapshotCorrected = try #require(try fresh.entry(id: stoppedFeed.id, for: baby.id))
        if case .feed(let snapshotFeed) = snapshotCorrected {
            #expect(same(snapshotFeed.components.first { $0.id == breastStep.id }?.endedAt, originalStepEnd.addingTimeInterval(-60)))
        } else {
            Issue.record("fresh snapshot lost the corrected feed")
        }
        proof(19, "linked two feeds and corrected one breast step; concurrent same-feed edits converged without loss or duplication; both caregivers and a fresh snapshot preserve the session and exact end time")
        #expect(try fresh.lastSupplement(for: baby.id)?.id == iron.id)
        #expect(try fresh.supplementNames(for: baby.id) == ["Iron", "Vitamin D", "Vitamin D drops"])
        let snapshotRows = try fresh.context.fetch(FetchDescriptor<SupplementEntry>())
        let snapshotVitamin = try #require(snapshotRows.first { $0.id == vitamin.id })
        #expect(same(snapshotVitamin.loggedAt, vitaminLoggedAt))
        #expect(same(snapshotVitamin.startedAt, backdated))
        #expect(snapshotVitamin.note == "corrected by caregiver")
        #expect(same(snapshotRows.first { $0.id == iron.id }?.loggedAt, ironLoggedAt))
        proof(16, "shared supplement create owner→joiner, normalized reuse joiner→owner, name/time/note edit, immutable logged_at and Iron selection preserved on both phones and fresh server snapshot")

        try SupplementSheetModel(store: owner.store, babyID: baby.id, editing: correctedVitamin.id).delete()
        try await owner.sync(.localWrite)
        try await joiner.sync()
        for phone in [owner, joiner] {
            #expect(try !phone.supplements().contains { $0.id == vitamin.id })
            #expect(try phone.store.supplementNames(for: baby.id) == ["Iron", "Vitamin D"])
        }
        try SupplementSheetModel(store: joiner.store, babyID: baby.id, editing: reused.id).delete()
        try await joiner.sync(.localWrite)
        try await owner.sync()
        for phone in [owner, joiner] {
            #expect(try phone.supplements().map(\.id) == [iron.id])
            #expect(try phone.store.supplementNames(for: baby.id) == ["Iron"])
            #expect(try phone.store.nextDispatchBatch(limit: 100).isEmpty)
        }
        proof(17, "supplement deletes converged in both directions; only Iron remains reusable; both outboxes drained")

        try await growthProof(owner: owner, joiner: joiner, baby: baby, importedMeasurement: localMeasurement,
                              clock: clock, live: live, scratch: scratch)

        // MARK: 14. The joiner relaunches.

        let beforeRelaunch = try joiner.credentials(joinerKey)
        joiner = try await joiner.relaunched()
        phones.append(joiner)
        let restoredKey = try joiner.familyKey()
        #expect(restoredKey == joinerKey)
        try joiner.expectUpToDate("after relaunching")
        let afterRelaunch = try joiner.history()
        #expect(afterRelaunch.babies.map(\.id) == [baby.id])
        clock.advance(by: 20 * 60)
        let lateDiaper = try joiner.store.logDiaper(.wet, for: baby.id)
        try await joiner.sync(.localWrite)
        try await owner.sync()
        #expect(try owner.history().diapers.map(\.id) == [lateDiaper.id])
        clock.advance(by: 5 * 60)
        let lateBottle = try owner.store.logBottle(volumeML: 90, contents: .breastMilk, for: baby.id)
        try await owner.sync(.localWrite)
        try await joiner.sync()
        let lateArrival = try #require(try joiner.history().feeds.first { $0.id == lateBottle.id })
        #expect(lateArrival.components == lateBottle.components)
        let keptSession = try joiner.credentials(joinerKey)
        proof(14, "joiner relaunched from the same Keychain and defaults (restoreFamilySession), a diaper logged after relaunch reached the owner and the owner's bottle reached it; session \(keptSession.refreshToken == beforeRelaunch.refreshToken ? "unchanged" : "renewed") across relaunch")

        // MARK: 15. The joiner hears about the owner's write within seconds.

        // The relaunch left the joiner in the background: no poll, no socket.
        let joinerEngine = try joiner.engine()
        #expect(joinerEngine.foregroundPollInterval == nil)
        let syncsBeforeConnect = joiner.traffic.syncRequests
        await joiner.model.sceneBecameActive()
        let connected = await eventually(within: .seconds(5)) { joinerEngine.liveState == .connected }
        try #require(connected, "the joiner's socket never connected: state=\(joinerEngine.liveState) dials=\(joiner.traffic.liveDials)")
        // Connecting owes a catch-up sync, tagged like a hint. Let it finish
        // before the write, or it could be what carries the diaper across.
        let caughtUp = await eventually(within: .seconds(5)) {
            joiner.traffic.syncRequests > syncsBeforeConnect
                && joinerEngine.lastSyncReason == .liveHint && !joinerEngine.isBusy
        }
        try #require(caughtUp, "the joiner's catch-up after connecting never finished: reason=\(String(describing: joinerEngine.lastSyncReason)) busy=\(joinerEngine.isBusy)")
        // Up, and the poll still an hour away: whatever arrives now came
        // through the socket.
        #expect(joinerEngine.foregroundPollInterval == Phone.pollInterval)
        let syncsBefore = joiner.traffic.syncRequests
        let reasonBefore = joinerEngine.lastSyncReason

        clock.advance(by: 10 * 60)
        let liveDiaper = try owner.store.logDiaper(.dirty, for: baby.id)
        let t0 = ContinuousClock.now
        try await owner.sync(.localWrite)
        let pushed = ContinuousClock.now - t0
        var arrivedAfter: Duration?
        while ContinuousClock.now - t0 < .seconds(10) {
            if try joiner.history().diapers.contains(where: { $0.id == liveDiaper.id }) {
                arrivedAfter = ContinuousClock.now - t0
                break
            }
            try await Task.sleep(for: .milliseconds(25))
        }
        let latency = try #require(arrivedAfter, "the owner's diaper never reached the joiner through the socket")
        let latencyMS = Int(latency / .milliseconds(1))
        #expect(latency < .seconds(3), "the owner's diaper took \(latencyMS) ms")
        // Let the hint's sync finish.
        _ = await eventually(within: .seconds(5)) { !joinerEngine.isBusy }
        let hintSyncs = joiner.traffic.syncRequests - syncsBefore
        #expect(joinerEngine.lastSyncReason == .liveHint, "the joiner's last sync was \(String(describing: joinerEngine.lastSyncReason))")
        // The catch-up already ran: only the hint can have sent this one.
        #expect(hintSyncs == 1, "the joiner sent \(hintSyncs) syncs for one hint")
        try joiner.expectUpToDate("after the live hint")

        // The client dials the way the server needs, which only a real server
        // can say. It connected at all: no `Origin`, or the canonical one.
        let opened = joiner.traffic.liveDials.filter { $0.status == 101 }
        #expect(!opened.isEmpty && opened.allSatisfy { $0.origin == nil || $0.origin == live.endpoint.origin.canonicalString })
        // A refused upgrade surfaces its status: a bogus token is a 401, and
        // a second socket for a device that has its one slot is a 429.
        let connector = URLSessionLiveSocketConnector(endpoint: live.endpoint, session: joiner.session)
        let bogus = await liveRefusal { try await connector.open(token: "not-a-session-token") }
        #expect(bogus == .unauthorized, "a bogus token was \(String(describing: bogus))")
        let second = await liveRefusal { try await connector.open(token: try joiner.credentials(joinerKey).accessToken) }
        #expect(second == .rateLimited(5), "a second socket for the device was \(String(describing: second))")

        // Background closes the socket cleanly, and that frees the device's
        // single slot at once: the next foreground connects with no 429.
        joiner.model.sceneEnteredBackground()
        let closed = await eventually(within: .seconds(5)) { joinerEngine.liveState == .stopped }
        #expect(closed, "background left the socket \(joinerEngine.liveState)")
        let dialsBeforeReturn = joiner.traffic.liveDials.count
        await joiner.model.sceneBecameActive()
        let reconnected = await eventually(within: .seconds(5)) { joinerEngine.liveState == .connected }
        let returnDials = Array(joiner.traffic.liveDials.dropFirst(dialsBeforeReturn))
        #expect(reconnected, "the joiner did not reconnect: state=\(joinerEngine.liveState) dials=\(returnDials)")
        #expect(!returnDials.contains { $0.status == 429 } && returnDials.contains { $0.status == 101 }, "reconnect dials: \(returnDials)")
        joiner.model.sceneEnteredBackground()
        _ = await eventually(within: .seconds(5)) { joinerEngine.liveState == .stopped }

        // The server's close code reaches the client: a data frame from the
        // client is against the protocol, and the server closes with 1008.
        let serverClose = try await policyClose(live: live, phone: joiner, key: joinerKey)
        #expect(serverClose.map { if case .closed(1008, _) = $0 { true } else { false } } == true, "the server's close arrived as \(String(describing: serverClose))")

        proof(15, "live hint delivered owner's diaper in \(latencyMS) ms (owner's push took \(Int(pushed / .milliseconds(1))) ms, poll interval \(Phone.pollInterval)), joiner syncs by reason=[\(reasonBefore.map { "\($0)" } ?? "-") before the write → \(joinerEngine.lastSyncReason.map { "\($0)" } ?? "-") after it], POST /v1/sync after the write=\(hintSyncs); upgrade answers: open=\(opened.first.map(String.init(describing:)) ?? "-") bogus-token=\(bogus.map(String.init(describing:)) ?? "opened") second-socket=\(second.map(String.init(describing:)) ?? "opened") reconnect-after-background=\(returnDials); server close seen as \(serverClose.map(String.init(describing:)) ?? "-")")

        // MARK: 18. The owner sees and revokes each pending invitation.

        // A brand-new Apple account and device, the way the owner's own first
        // sign-in worked in step 2 — this phone exists only to redeem the
        // view-only invitation, so it needs no bootstrap identity of its own.
        let third = try Phone(name: "third", live: live, clock: clock, directory: scratch, appleSubject: UUID().uuidString.lowercased())
        phones.append(third)
        defer { third.forgetDefaults() }

        let caregiverInvitation = try await owner.model.createInvitation(role: .caregiver)
        let viewerInvitation = try await owner.model.createInvitation(role: .viewer)
        await owner.model.reloadInvitations()
        let rolesAfterCreate = Set(owner.model.pendingInvitations.map(\.role))
        #expect(rolesAfterCreate == [.caregiver, .viewer], "pending invitations after creating both: \(owner.model.pendingInvitations.map { "\($0.id)=\($0.role)" })")

        // The QR payload an owner would hold up for the camera — built the
        // same way `InviteSheet` builds it — parsed and redeemed by a third
        // phone that has never seen this family.
        _ = try await third.signIn(displayName: "Third Viewer", live: live)
        let viewerPayload = try InvitationQRCode.payload(for: viewerInvitation)
        let viewerIntent = try InvitationParser.parse(viewerPayload)
        try await third.model.acceptInvitation(viewerIntent)
        let thirdKey = try third.familyKey()
        #expect(thirdKey.familyID == ownerKey.familyID)

        await owner.model.reloadInvitations()
        #expect(owner.model.pendingInvitations.map(\.id) == [caregiverInvitation.id], "after redeeming the view-only invitation: \(owner.model.pendingInvitations.map { "\($0.id)=\($0.role)" })")

        try await owner.model.revokeInvitation(caregiverInvitation.id)
        #expect(owner.model.pendingInvitations.isEmpty, "after revoking the caregiver invitation: \(owner.model.pendingInvitations)")

        // The stored codes are pruned to none, both through the model's own
        // read-back and directly against the Keychain-backed store it uses.
        #expect(owner.model.shareURL(for: caregiverInvitation.id) == nil)
        #expect(owner.model.shareURL(for: viewerInvitation.id) == nil)
        let codeStore = InvitationCodeStore(backend: owner.keychain, realm: ownerKey.realmID)
        #expect(codeStore.shareURL(for: caregiverInvitation.id) == nil)
        #expect(codeStore.shareURL(for: viewerInvitation.id) == nil)
        proof(18, "owner created caregiver=\(caregiverInvitation.id) and view-only=\(viewerInvitation.id) invitations, roles=\(rolesAfterCreate.map { "\($0)" }.sorted()); third phone redeemed the view-only one via its QR payload and joined family=\(thirdKey.familyID); owner then saw only [\(caregiverInvitation.id)], revoked it, list is empty, and both invitations' stored codes are pruned to none")

        // MARK: 24. History's session edits reach the other caregiver.
        //
        // Spec §5: the shapes History's session sheet sends — a create that
        // already carries its session link, a Split, a Join, and deleting a
        // session's first part, which promotes the next part to root and
        // relinks the rest — go through the real server with a part running
        // on from the session, whose link each edit moves too. After every
        // edit both phones, and at the end the server's snapshot, agree on
        // every session and on the running part's link.

        clock.advance(by: 90 * 60)   // well past the feed break after the late bottle
        let sessionStart = clock.now.addingTimeInterval(-25 * 60)
        let firstPart = try owner.store.logBreast(side: .left, from: sessionStart,
                                                  to: sessionStart.addingTimeInterval(10 * 60), for: baby.id)
        #expect(firstPart.feedingSessionID == nil)
        let sheet = SessionSheetModel(store: owner.store, babyID: baby.id, kind: .feed, rootID: firstPart.id)
        let addedBottle = try sheet.addPart(.bottle).entryID
        let addedLeft = try sheet.addPart(.left).entryID
        try await expectSameSessions(owner, joiner, baby.id, "after Add part")
        #expect(try sessionLinks(owner, baby.id).contains("\(firstPart.id)=\([firstPart.id, addedBottle, addedLeft])"))

        let running = try owner.store.startPart(.breast(.right), for: baby.id)
        #expect(running.sessionID == firstPart.id)
        try await expectSameSessions(owner, joiner, baby.id, "after starting a part", timerLink: firstPart.id)

        let bottleRow = try #require(sheet.parts.first { $0.target.entryID == addedBottle })
        try sheet.split(before: bottleRow.id)
        try await expectSameSessions(owner, joiner, baby.id, "after Split here", timerLink: addedBottle)
        #expect(try sessionLinks(joiner, baby.id).contains("\(addedBottle)=\([addedBottle, addedLeft])"))

        // The split-off bottle's own editor offers to join it back.
        let joinRow = JoinRowModel(store: owner.store, babyID: baby.id, kind: .feed, partOf: addedBottle)
        #expect(joinRow.previous?.rootID == firstPart.id)
        try joinRow.join()
        try await expectSameSessions(owner, joiner, baby.id, "after Join", timerLink: firstPart.id)
        #expect(try sessionLinks(joiner, baby.id).contains("\(firstPart.id)=\([firstPart.id, addedBottle, addedLeft])"))

        sheet.reload()
        let firstRow = try #require(sheet.parts.first)
        #expect(firstRow.target.entryID == firstPart.id)
        try sheet.deletePart(firstRow.id)
        #expect(sheet.rootID == addedBottle)
        try await expectSameSessions(owner, joiner, baby.id, "after deleting the first part", timerLink: addedBottle)
        for phone in [owner, joiner] {
            #expect(try sessionLinks(phone, baby.id).contains("\(addedBottle)=\([addedBottle, addedLeft])"), "\(phone.name)")
            try expectNoLinkToATombstone(phone, baby.id)
        }
        // Through the third phone: the server admits two live snapshots per
        // account, and the owner's and the joiner's are both taken by now.
        let snapshot = try await third.freshSnapshot()
        #expect(try snapshot.feedingSessions(for: baby.id, from: .distantPast, to: .distantFuture).map(SessionLink.init)
            == (try owner.store.feedingSessions(for: baby.id, from: .distantPast, to: .distantFuture).map(SessionLink.init)),
            "the server's snapshot disagrees with the owner's sessions")
        #expect(try snapshot.activeTimer(kind: .feed, for: baby.id)?.sessionID == addedBottle, "the server's timer link")

        // The other caregiver stops it: it lands in the promoted session.
        clock.advance(by: 5 * 60)
        let joinerRunning = try #require(try joiner.store.activeTimer(kind: .feed, for: baby.id))
        let continued = try #require(try joiner.store.stopFeed(joinerRunning))
        #expect(continued.feedingSessionID == addedBottle)
        try await joiner.sync(.localWrite)
        try await owner.sync()
        #expect(try owner.store.activeTimer(kind: .feed, for: baby.id) == nil)
        try await expectSameSessions(owner, joiner, baby.id, "after the joiner stopped the running part")
        #expect(try sessionLinks(owner, baby.id).contains("\(addedBottle)=\([addedBottle, addedLeft, continued.id])"))
        proof(24, "History session edits through the server with a part running: Add part (bottle, left) created already linked, Split here (timer to the later half), Join from the bottle's row (timer back), the first part deleted (next part promoted to root, the rest and the timer relinked); both phones agree on every session and the timer's link after each edit, and a fresh snapshot agrees at the end; the joiner stopped the running part into the promoted session \(addedBottle)")
        } catch {
            // Stop every engine before the deferred cleanup removes the
            // scratch directory and Keychain out from under one still
            // reading or writing the store: that would surface as a
            // misleading CoreData disk I/O error instead of this failure.
            await stopEngines(phones)
            throw error
        }
        await stopEngines(phones)
    }
}

/// Stops every phone's engine so the store is never read or written again
/// before the scratch directory and Keychain holding it go away — on a
/// normal return, and ahead of rethrowing when a step fails. Calling `stop()`
/// twice (relaunch already stops the phone it replaces) is harmless.
@MainActor
private func stopEngines(_ phones: [Phone]) async {
    for phone in phones {
        await phone.model.engine?.stop()
    }
}

/// A feed session as both phones and the server must agree on it: its key
/// and its members in order. Times are left out, as the server keeps
/// microseconds and a phone whatever `Date` held.
private struct SessionLink: Equatable, CustomStringConvertible {
    let id: UUID
    let feedIDs: [UUID]
    init(_ session: FeedingSession) {
        id = session.id
        feedIDs = session.feedIDs
    }
    var description: String { "\(id)=\(feedIDs)" }
}

@MainActor
private func sessionLinks(_ phone: Phone, _ babyID: UUID) throws -> [String] {
    try phone.store.feedingSessions(for: babyID, from: .distantPast, to: .distantFuture).map { SessionLink($0).description }
}

/// Sends the owner's writes, takes them on the joiner, and checks both list
/// the same sessions with the same members — and, given `timerLink`, that
/// both phones' running feed is linked to that session.
@MainActor
private func expectSameSessions(_ owner: Phone, _ joiner: Phone, _ babyID: UUID, _ when: String,
                                timerLink: UUID? = nil) async throws {
    try await owner.sync(.localWrite)
    try await joiner.sync()
    #expect(try owner.store.nextDispatchBatch(limit: 100).isEmpty, "the owner still owes mutations \(when)")
    let ownerSessions = try sessionLinks(owner, babyID)
    let joinerSessions = try sessionLinks(joiner, babyID)
    #expect(ownerSessions == joinerSessions, "sessions differ \(when):\n  owner:  \(ownerSessions)\n  joiner: \(joinerSessions)")
    guard let timerLink else { return }
    for phone in [owner, joiner] {
        let link = try phone.store.activeTimer(kind: .feed, for: babyID)?.sessionID
        #expect(link == timerLink, "\(phone.name)'s running feed is linked to \(link.map(\.uuidString) ?? "nothing") \(when), not \(timerLink)")
    }
}

/// No live feed is linked to an entry that is not a live feed, and the
/// running feed's link, if any, names a session that exists.
@MainActor
private func expectNoLinkToATombstone(_ phone: Phone, _ babyID: UUID) throws {
    let live = try phone.history().feeds.filter { $0.babyID == babyID }
    let liveIDs = Set(live.map(\.id))
    let dangling = live.filter { $0.feedingSessionID.map { !liveIDs.contains($0) } ?? false }
    #expect(dangling.isEmpty, "\(phone.name): feeds linked to a deleted entry: \(dangling.map(\.id))")
    if let link = try phone.store.activeTimer(kind: .feed, for: babyID)?.sessionID {
        #expect(live.contains { $0.effectiveFeedingSessionID == link }, "\(phone.name): the running feed is linked to \(link), no session")
    }
}

/// Polls `condition` every 10 ms until it holds or `within` runs out.
@MainActor
private func eventually(within limit: Duration, _ condition: () -> Bool) async -> Bool {
    let start = ContinuousClock.now
    while ContinuousClock.now - start < limit {
        if condition() { return true }
        try? await Task.sleep(for: .milliseconds(10))
    }
    return condition()
}

/// How a dial the test expects refused was refused, or nil if it opened (the
/// socket is closed again at once).
@MainActor
private func liveRefusal(_ dial: () async throws -> any LiveSocket) async -> LiveSocketError? {
    do {
        let socket = try await dial()
        socket.close(.normalClosure)
        return nil
    } catch let error as LiveSocketError {
        return error
    } catch {
        return .network
    }
}

/// Opens a socket with the phone's token, sends the one thing a client may
/// not — a data frame — and returns how the production socket reports the
/// server's answer. The request is the connector's own, so the server sees
/// exactly what the app sends.
@MainActor
private func policyClose(live: LiveServer, phone: Phone, key: FamilySessionKey) async throws -> LiveSocketError? {
    let token = try phone.credentials(key).accessToken
    var lastFailure: (any Error)?
    // The slot the background freed may take the server a moment to release.
    for _ in 0..<3 {
        let task = phone.session.webSocketTask(with: URLSessionLiveSocketConnector.request(origin: live.endpoint.origin, token: token))
        task.resume()
        do {
            try await opened(task)
        } catch {
            lastFailure = error
            task.cancel()
            try await Task.sleep(for: .seconds(1))
            continue
        }
        try await task.send(.string("not allowed"))
        let socket = URLSessionLiveSocket(task: task)
        defer { socket.close(.normalClosure) }
        do {
            _ = try await socket.receive()
            return nil
        } catch let error as LiveSocketError {
            return error
        }
    }
    throw E2EFailure("no socket opened for the close-code probe: \(String(describing: lastFailure)) dials=\(phone.traffic.liveDials)")
}

// MARK: - The server

/// The live server's address, the pinned TLS session every transport shares,
/// and the bootstrap's sessions.
@MainActor
struct LiveServer {
    let environment: E2EEnvironment
    let endpoint: ConfirmedEndpoint
    let session: URLSession
    private let pin: PinnedCertificateDelegate
    let configuration: URLSessionConfiguration

    init(_ environment: E2EEnvironment) throws {
        self.environment = environment
        endpoint = ConfirmedEndpoint(
            origin: try ServerOrigin(validating: environment.baseURL),
            serverInstanceID: environment.serverInstanceID
        )
        configuration = try CapacityTestRoute.currentConfiguration()
        pin = PinnedCertificateDelegate(pinning: environment.certificate, strictHostname: CapacityTestRoute.enabled ? "sync.example.test" : nil)
        session = URLSession(configuration: configuration, delegate: pin, delegateQueue: nil)
    }

    /// Pinned to the same certificate as every other transport.
    func identityClient() -> HTTPIdentityClient {
        let pin = pin
        return HTTPIdentityClient(configuration: configuration, evaluateTrust: { pin.evaluate($0) })
    }

    /// The account the Go bootstrap signed in and left without a family.
    static let outsider2Subject = "10000000-0000-4000-8000-000000000004"

    /// The bootstrap's session for `account`, read with the client's own
    /// decoder.
    func bootstrapSession(_ account: String) throws -> SessionBundle {
        let raw = try #require(environment.sessions[account], "the bootstrap state has no \(account)")
        return try JSONDecoder().decode(WireSessionBundle.self, from: raw).value
    }

    func authorizationMode() async throws -> String {
        try await identityClient().serverInfo(endpoint.origin).appleAuthorizationMode
    }

    /// Stands in for Sign in with Apple: asks the server's identity-test
    /// adapter to sign an identity token for `subject` and the nonce the
    /// client computed — the one thing on a phone that comes from Apple.
    func appleClient(subject: String) -> IdentityTestAppleSignIn {
        IdentityTestAppleSignIn(base: environment.baseURL, subject: subject, session: session)
    }
}

/// Apple, as far as the server under test can tell.
struct IdentityTestAppleSignIn: AppleSignInClient {
    let base: String
    let subject: String
    let session: URLSession

    func requestProof(nonce: String) async throws -> AppleProof {
        var request = URLRequest(url: try #require(URL(string: base + "/__identitytest/proof")))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONSerialization.data(withJSONObject: ["subject": subject, "nonce": nonce])
        let (data, response) = try await session.data(for: request)
        let status = (response as? HTTPURLResponse)?.statusCode ?? 0
        guard status == 200,
              let object = try JSONSerialization.jsonObject(with: data) as? [String: Any],
              let token = object["identity_token"] as? String else {
            throw E2EFailure("identity-test proof refused (\(status)): \(String(decoding: data, as: UTF8.self))")
        }
        // Apple returns an authorization code with every proof; the client
        // decides from the server's mode whether to send it.
        return AppleProof(
            identityToken: token, authorizationCode: "identitytest-code-\(subject)",
            userIdentifier: subject, fullName: nil
        )
    }
}

// MARK: - A phone

/// One phone: its own store on disk, its own Keychain and App Group defaults,
/// and the app's composition over them.
@MainActor
final class Phone {
    let name: String
    let store: BabyStore
    let keychain: any KeychainBackend
    let defaults: UserDefaults
    let suiteName: String
    let appleSubject: String
    let graph: FamilySyncComposition.Graph
    /// This phone's own pinned session, so its requests can be counted apart
    /// from the other phone's.
    let session: URLSession
    let traffic: PhoneTraffic
    var foregroundBridge: ForegroundObservationBridge?
    private let live: LiveServer
    private let clock: TestDateProvider
    private let storeURL: URL

    var model: FamilySettingsViewModel { graph.model }

    /// An hour: no poll fires during the run.
    static let pollInterval: Duration = .seconds(3600)

    /// The engine the model built, as itself rather than as the model's
    /// protocol, for what only the gate reads.
    func engine() throws -> FamilySyncEngine {
        try #require(model.engine as? FamilySyncEngine, "\(name) has no sync engine")
    }

    convenience init(name: String, live: LiveServer, clock: TestDateProvider, directory: URL, appleSubject: String,
        pollOverride: Duration? = Phone.pollInterval, liveHintsEnabled: Bool = true,
        diagnostics: (any SyncDiagnosticSink)? = nil, recoveryDiagnostics: @escaping (RefreshRecoveryEvent) -> Void = { _ in },
        liveActivityPush: ((any KeychainBackend) -> FamilySyncComposition.LiveActivityPushWiring)? = nil) throws {
        let suite = "two-caregiver-e2e-\(name)-\(UUID().uuidString)"
        try self.init(
            name: name, live: live, clock: clock,
            storeURL: directory.appending(path: "\(name).store"),
            keychain: InMemoryKeychainBackend(),
            suiteName: suite, appleSubject: appleSubject, pollOverride: pollOverride, liveHintsEnabled: liveHintsEnabled,
            diagnostics: diagnostics, recoveryDiagnostics: recoveryDiagnostics, liveActivityPush: liveActivityPush
        )
    }

    init(
        name: String, live: LiveServer, clock: TestDateProvider,
        storeURL: URL, keychain: any KeychainBackend, suiteName: String, appleSubject: String,
        pollOverride: Duration? = Phone.pollInterval, liveHintsEnabled: Bool = true,
        diagnostics: (any SyncDiagnosticSink)? = nil, recoveryDiagnostics: @escaping (RefreshRecoveryEvent) -> Void = { _ in },
        liveActivityPush: ((any KeychainBackend) -> FamilySyncComposition.LiveActivityPushWiring)? = nil
    ) throws {
        self.appleSubject = appleSubject
        self.name = name
        self.live = live
        self.clock = clock
        self.storeURL = storeURL
        self.keychain = keychain
        self.suiteName = suiteName
        defaults = try #require(UserDefaults(suiteName: suiteName))
        store = BabyStore(
            container: try BabySchema.container(storeURL: storeURL),
            dateProvider: clock,
            deviceID: "\(name)-phone",
            coordinationLockURL: storeURL.appendingPathExtension("coordination.lock")
        )
        let recorder = RecordingPinnedDelegate(pinning: live.environment.certificate, strictHostname: CapacityTestRoute.enabled ? "sync.example.test" : nil)
        traffic = recorder.traffic
        session = URLSession(configuration: live.configuration, delegate: recorder, delegateQueue: nil)
        // Polling effectively off: every step drives its own syncs, and step
        // 15 must prove a change arrived through the live socket alone.
        //
        // Push as a simulator build has it: the sandbox environment, with no
        // provisioning profile to read. The registration call stays the
        // default no-op, so a phone has an APNs token only when a test hands
        // it one (`graph.push.deviceTokenReceived`); until then the
        // production registrar, lifecycle and HTTP client stay inert.
        graph = FamilySyncComposition.assemble(
            store: store, keychainBackend: keychain, identity: live.identityClient(),
            session: session, defaults: defaults, apple: live.appleClient(subject: appleSubject),
            foregroundPollInterval: pollOverride, liveHintsEnabled: liveHintsEnabled,
            recoveryDiagnostics: recoveryDiagnostics, syncDiagnostics: diagnostics,
            pushEnvironment: PushEnvironmentResolver(isSimulator: true),
            liveActivityPush: liveActivityPush?(keychain)
        )
    }

    /// The push driver the app's graph built, when this phone was given
    /// Live Activity wiring (LiveActivityPushE2ETests).
    var push: LiveActivityPushDriver? { graph.liveActivityPush }

    /// What a parent does on the Caregivers screen: the name first, then
    /// Sign in with Apple. Returns the account session it left behind.
    func signIn(displayName: String, live: LiveServer) async throws -> AccountSessionKey {
        try await model.confirmEndpoint(live.endpoint)
        try await model.confirmDisplayName(displayName)
        try await model.signInWithApple()
        guard case .authenticated(let key) = model.mode else {
            throw E2EFailure("\(name) is not signed in: mode=\(model.mode)")
        }
        let remaining = Int(try accountCredentials(key).expiresAt.timeIntervalSinceNow)
        print("two-caregiver proof: \(name) signed in, access token valid for \(remaining) s")
        return key
    }

    func accountCredentials(_ key: AccountSessionKey) throws -> SessionCredentials {
        try #require(try KeychainSessionStore(backend: keychain).loadAccount(key), "\(name) has no account credentials")
    }

    func installationID() throws -> String {
        try KeychainSessionStore(backend: keychain).installationID()
    }


    /// The app is killed and started again: the engine stops, and a new
    /// store, coordinator and model are built over the same database file,
    /// the same Keychain and the same defaults — then launch runs the way
    /// `AppEnvironment` and `RootView` run it.
    func relaunched() async throws -> Phone {
        await model.engine?.stop()
        let next = try Phone(
            name: name, live: live, clock: clock,
            storeURL: storeURL, keychain: keychain, suiteName: suiteName, appleSubject: appleSubject
        )
        next.model.restoreSession()
        #expect(next.model.engine != nil, "restoreSession found no family session")
        await next.model.startSyncing()
        // The test drives every sync itself; polling in the background would
        // only make the order of events harder to read.
        next.model.sceneEnteredBackground()
        // Connecting the live socket owes a catch-up sync (`liveConnected`).
        // When the socket opens after the launch sync has finished, that sync
        // starts on its own and can still be in flight here; let it land
        // before anything reads the status or the store.
        if let engine = next.model.engine as? FamilySyncEngine {
            _ = await eventually(within: .seconds(5)) { !engine.isBusy }
        }
        return next
    }

    func forgetDefaults() {
        UserDefaults.standard.removePersistentDomain(forName: suiteName)
    }

    func familyKey() throws -> FamilySessionKey {
        guard case .shared(let key) = model.mode else {
            throw E2EFailure("\(name) is not in a family: mode=\(model.mode)")
        }
        return key
    }

    func credentials(_ key: FamilySessionKey) throws -> SessionCredentials {
        try #require(try KeychainSessionStore(backend: keychain).loadFamily(key), "\(name) has no family credentials")
    }

    /// One sync through the engine the model built, which must end up to date.
    func sync(_ reason: SyncReason = .foreground) async throws {
        let engine = try #require(model.engine, "\(name) has no sync engine")
        await engine.syncNow(reason)
        try expectUpToDate("after a \(reason) sync")
    }

    func expectUpToDate(_ when: String) throws {
        let engine = try #require(model.engine, "\(name) has no sync engine")
        guard case .upToDate = engine.status else {
            throw E2EFailure("\(name) is not up to date \(when): \(engine.status)")
        }
    }

    var statusText: String { model.engine.map { "\($0.status)" } ?? "no engine" }

    func supplements() throws -> [SupplementEntry] {
        let realm = store.activeRealm.encodedKey
        return try store.context.fetch(FetchDescriptor<SupplementEntry>()).filter {
            !$0.isTombstoned && $0.realmKey == realm
        }
    }

    /// Rebuild an empty store from the real server, without a local base or
    /// overlay that could hide a missing immutable timestamp in the snapshot.
    func freshSnapshot() async throws -> BabyStore {
        let key = try familyKey()
        let tokens = KeychainTokenProvider(keychain: KeychainSessionStore(backend: keychain), key: { .family(key) })
        let client = FamilySyncHTTPClient(endpoint: live.endpoint, tokens: tokens, session: session)
        let destination = try BabyStore.inMemory(dateProvider: clock)
        try destination.setActiveRealm(key.realmID)
        var page = try await client.snapshot(pageToken: nil)
        let snapshotID = page.snapshotID
        destination.beginSnapshot(snapshotID: snapshotID, highWater: page.highWater)
        while true {
            try destination.applySnapshotPage(page)
            guard let token = page.nextPageToken else { break }
            page = try await client.snapshot(pageToken: token)
        }
        try destination.finishSnapshot(snapshotID: snapshotID)
        return destination
    }

    /// What the phone shows: live rows in its active realm.
    func history() throws -> History {
        let realm = store.activeRealm.encodedKey
        return History(
            babies: try store.context.fetch(FetchDescriptor<Baby>()).filter { !$0.isTombstoned && $0.realmKey == realm },
            feeds: try store.context.fetch(FetchDescriptor<FeedEntry>()).filter { !$0.isTombstoned && $0.realmKey == realm },
            sleeps: try store.context.fetch(FetchDescriptor<SleepEntry>()).filter { !$0.isTombstoned && $0.realmKey == realm },
            diapers: try store.context.fetch(FetchDescriptor<DiaperEntry>()).filter { !$0.isTombstoned && $0.realmKey == realm }
        )
    }
}

struct History {
    let babies: [Baby]
    let feeds: [FeedEntry]
    let sleeps: [SleepEntry]
    let diapers: [DiaperEntry]
}

/// A component without its ID, for readable proof lines and exact shape checks.
enum Shape: Equatable, CustomStringConvertible {
    case breast(BreastSide, Int)
    case bottle(Double, BottleContents)
    case solids([String])

    init(_ component: FeedComponent) {
        switch component {
        case let .breast(_, side, seconds, _, _): self = .breast(side, seconds)
        case let .bottle(_, volume, contents, _, _, _): self = .bottle(volume, contents)
        case let .solids(_, foods, _): self = .solids(foods)
        }
    }

    var description: String {
        switch self {
        case let .breast(side, seconds): "\(side.rawValue) \(seconds)s"
        case let .bottle(volume, contents): "\(Int(volume)) ml \(contents.rawValue)"
        case let .solids(foods): "solids \(foods)"
        }
    }
}

struct E2EFailure: Error, CustomStringConvertible {
    let description: String
    init(_ description: String) { self.description = description }
}

/// The server keeps microseconds; a phone keeps whatever `Date` held.
private func same(_ a: Date?, _ b: Date?) -> Bool {
    guard let a, let b else { return a == nil && b == nil }
    return abs(a.timeIntervalSince(b)) < 0.001
}

/// A vaccination through the real server, both ways: create, the family's
/// spelling and the next dose on the other phone, an edit that clears the
/// dose, a delete, and a fresh snapshot. Its own proof lines, so the script
/// can require the last one.
@MainActor
private func immunizationProof(owner: Phone, joiner: Phone, baby: Baby, clock: TestDateProvider) async throws -> (id: UUID, givenAt: Date) {
    func shots(_ phone: Phone) throws -> [ImmunizationEntry] { try phone.store.immunizations(for: baby.id) }
    func line(_ step: Int, _ text: String) { print("immunization proof: step \(step): \(text)") }

    clock.advance(by: 60)
    let givenFirst = clock.now.addingTimeInterval(-20 * 86_400)
    let first = try owner.store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: baby.id, at: givenFirst, note: "left thigh")
    try await owner.sync(.localWrite)
    try await joiner.sync()
    let received = try #require(try shots(joiner).first { $0.id == first.id })
    #expect(received.vaccine == "Prevenar 13")
    #expect(received.dose == 1)
    #expect(received.note == "left thigh")
    #expect(received.timeZoneID == first.timeZoneID)
    #expect(same(received.givenAt, givenFirst))
    #expect(try joiner.store.vaccineNames() == ["Prevenar 13"])
    #expect(try joiner.store.nextDose(of: "Prevenar 13", for: baby.id) == 2)
    line(1, "a shot created by the owner reached the joiner with its vaccine, dose, note, zone and time; the joiner's next dose is 2")

    clock.advance(by: 60)
    let second = try joiner.store.logImmunization(vaccine: " prevenar 13 ", dose: 2, for: baby.id)
    #expect(second.vaccine == "Prevenar 13")
    try await joiner.sync(.localWrite)
    try await owner.sync()
    #expect(try shots(owner).map(\.id) == [second.id, first.id])
    #expect(try owner.store.nextDose(of: "Prevenar 13", for: baby.id) == 3)
    line(2, "the joiner's shot reused the family's spelling and reached the owner; the owner's next dose is 3")

    clock.advance(by: 60)
    let moved = givenFirst.addingTimeInterval(-86_400)
    try joiner.store.editImmunization(received, vaccine: "Prevenar 13", dose: nil, at: moved, note: nil)
    try await joiner.sync(.localWrite)
    try await owner.sync()
    let editReceipt = try #require(try joiner.store.context.fetch(FetchDescriptor<SyncOutboxEntry>()).first {
        $0.entityID == first.id && $0.operationRaw == "update"
    })
    try #require(editReceipt.deliveryState == .acknowledged, "immunization edit state=\(editReceipt.deliveryState)")
    #expect(first.dose == nil)
    #expect(first.note == nil)
    #expect(same(first.givenAt, moved))
    #expect(try owner.store.nextDose(of: "Prevenar 13", for: baby.id) == 3)
    line(3, "the joiner cleared the owner's dose and note and moved its time; the owner sees all three")

    clock.advance(by: 60)
    try owner.store.softDelete(try #require(try shots(owner).first { $0.id == second.id }))
    try await owner.sync(.localWrite)
    try await joiner.sync()
    for phone in [owner, joiner] {
        #expect(try shots(phone).map(\.id) == [first.id])
        #expect(try phone.store.vaccineNames() == ["Prevenar 13"])
        #expect(try phone.store.nextDose(of: "Prevenar 13", for: baby.id) == 1)
    }
    line(4, "the owner's delete of the joiner's shot converged; one shot remains on both phones")

    return (first.id, moved)
}

/// The fresh snapshot the two-caregiver test takes holds exactly the one live
/// shot, with the other phone's edit.
@MainActor
private func immunizationSnapshotProof(_ fresh: BabyStore, shot: (id: UUID, givenAt: Date), phones: [Phone]) throws {
    func line(_ step: Int, _ text: String) { print("immunization proof: step \(step): \(text)") }
    let snapshot = try fresh.context.fetch(FetchDescriptor<ImmunizationEntry>()).filter { !$0.isTombstoned }
    #expect(snapshot.map(\.id) == [shot.id])
    #expect(snapshot.first?.vaccine == "Prevenar 13")
    #expect(snapshot.first?.dose == nil)
    #expect(snapshot.first?.note == nil)
    #expect(same(snapshot.first?.givenAt, shot.givenAt))
    line(5, "a fresh snapshot from the server holds exactly the one live shot, with the edit")
    for phone in phones {
        #expect(try phone.store.nextDispatchBatch(limit: 100).isEmpty)
    }
    line(6, "create, reuse, edit to no dose, delete and fresh snapshot agree on both phones; both outboxes drained")
}

private func proof(_ step: Int, _ line: String) {
    print("two-caregiver proof: step \(step): \(line)")
}

/// The connector's own proof that an upgrade succeeded: a pong.
private func opened(_ task: URLSessionWebSocketTask) async throws {
    try await URLSessionLiveSocketConnector.awaitPong { task.sendPing(pongReceiveHandler: $0) }
}


@MainActor
private func growthProof(owner: Phone, joiner: Phone, baby: Baby, importedMeasurement: MeasurementCheckIn,
                         clock: TestDateProvider, live: LiveServer, scratch: URL) async throws {
    let received = try #require(try joiner.store.measurements(for: baby.id).first { $0.id == importedMeasurement.id })
    #expect(received.weightKG == 4.125 && received.lengthCM == 54 && received.headCircumferenceCM == 35)
    #expect(try joiner.store.growthProfile(for: baby.id)?.referenceSex == .girls)
    let originalLoggingTime = SupplementTimestamp.format(received.loggedAt)
    clock.advance(by: 1)
    try joiner.store.editMeasurement(received, at: received.measuredAt, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4.25))
    try await joiner.sync(.localWrite)
    try await owner.sync()
    #expect(importedMeasurement.weightKG == 4.25 && importedMeasurement.lengthCM == nil)
    #expect(importedMeasurement.headCircumferenceCM == nil && importedMeasurement.lengthPosition == nil)
    #expect(SupplementTimestamp.format(importedMeasurement.loggedAt) == originalLoggingTime)
    // Both edits observe the same version; the later complete record wins.
    clock.advance(by: 1)
    try owner.store.editMeasurement(importedMeasurement, at: importedMeasurement.measuredAt, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4.5))
    clock.advance(by: 1)
    try joiner.store.editMeasurement(received, at: received.measuredAt, timeZoneID: "UTC", values: MeasurementValues(weightKG: 4.75))
    try await owner.sync(.localWrite)
    try await joiner.sync(.localWrite)
    try await owner.sync()
    #expect(importedMeasurement.weightKG == 4.75 && received.weightKG == 4.75)
    #expect(SupplementTimestamp.format(importedMeasurement.loggedAt) == originalLoggingTime)
    let fresh = try await owner.freshSnapshot()
    let snap = try #require(try fresh.measurements(for: baby.id).first { $0.id == received.id })
    #expect(snap.weightKG == 4.75 && snap.lengthCM == nil && snap.headCircumferenceCM == nil)
    #expect(SupplementTimestamp.format(snap.loggedAt) == originalLoggingTime)
    proof(20, "growth local import and snapshot preserved IDs/raw values; other caregiver cleared optional readings, concurrent corrections converged, immutable fractional logged_at survived edit and fresh snapshot")

    // A complete offline write batch exists before the first network request.
    clock.advance(by: 1)
    let offlineBaby = try owner.store.addBaby(name: "Offline growth", birthDate: clock.now.addingTimeInterval(-86400), referenceSex: .boys, expectedDueDate: nil)
    let offlineReading = try owner.store.logMeasurement(for: offlineBaby.id, at: clock.now, timeZoneID: "UTC", values: MeasurementValues(weightKG: 3.5))
    #expect(try owner.store.nextDispatchBatch(limit: 100).map(\.entityKind.rawValue) == ["baby", "growth_profile", "measurement"])
    try await owner.sync(.localWrite)
    try await joiner.sync()
    #expect(try joiner.store.growthProfile(for: offlineBaby.id)?.referenceSex == .boys)
    let counterpart = try #require(try joiner.store.measurements(for: offlineBaby.id).first)
    #expect(counterpart.id == offlineReading.id && counterpart.weightKG == 3.5)

    // Two caregivers independently fill the same missing legacy profile.
    let legacy = Baby(name: "Legacy growth", birthDate: clock.now.addingTimeInterval(-86400), realm: owner.store.activeRealm)
    owner.store.context.insert(legacy)
    try owner.store.record(.create, on: legacy)
    try await owner.sync(.localWrite)
    try await joiner.sync()
    #expect(try owner.store.growthProfile(for: legacy.id) == nil)
    #expect(try joiner.store.growthProfile(for: legacy.id) == nil)
    try owner.store.setGrowthProfile(for: legacy.id, referenceSex: .girls, expectedDueDate: LocalDate("2026-09-01"))
    try joiner.store.setGrowthProfile(for: legacy.id, referenceSex: .boys, expectedDueDate: nil)
    try await owner.sync(.localWrite)
    try await joiner.sync(.localWrite)
    try await owner.sync()
    let ownerProfile = try #require(try owner.store.growthProfile(for: legacy.id))
    let joinerProfile = try #require(try joiner.store.growthProfile(for: legacy.id))
    #expect(ownerProfile.id == joinerProfile.id && ownerProfile.referenceSex == joinerProfile.referenceSex)
    clock.advance(by: 1)
    try joiner.store.setGrowthProfile(for: legacy.id, referenceSex: .boys, expectedDueDate: nil)
    try await joiner.sync(.localWrite)
    try await owner.sync()
    #expect(ownerProfile.referenceSex == .boys && ownerProfile.expectedDueDateISO == nil)
    proof(21, "offline shared baby/profile/check-in batch delivered; both caregivers filling legacy profile converged to one deterministic ID and a later profile edit cleared due date")

    // The server no longer reads the feature markers: a pull and a snapshot
    // without any of them are served like the client's own.
    let key = try owner.familyKey()
    for path in ["sync", "sync/snapshot"] {
        var request = URLRequest(url: URL(string: live.endpoint.origin.canonicalString + "/v1/" + path)!)
        request.setValue("Bearer \(try owner.credentials(key).accessToken)", forHTTPHeaderField: "Authorization")
        request.setValue("v1", forHTTPHeaderField: "X-Baby-Tracker-Protocol")
        request.setValue(path == "sync" ? "records-v1" : "records-v1,snapshot-v1", forHTTPHeaderField: "X-Baby-Tracker-Capabilities")
        if path == "sync" {
            request.httpMethod = "POST"
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONEncoder().encode(SyncRequest(protocolVersion: "v1", capabilities: ["records-v1"], cursor: nil, mutations: [], pageToken: nil))
        }
        let (data, response) = try await live.session.data(for: request)
        #expect((response as? HTTPURLResponse)?.statusCode == 200)
        let body = try #require(JSONSerialization.jsonObject(with: data) as? [String: Any])
        #expect(body["high_water"] != nil)
    }
    let ownerTokens = KeychainTokenProvider(keychain: KeychainSessionStore(backend: owner.keychain), key: { .family(key) })
    let export = try ExportClient(endpoint: live.endpoint, tokens: ownerTokens, session: owner.session,
                                  files: ExportTemporaryFiles(directory: scratch.appending(path: "owner-export")))
    // This protocol harness deliberately has no export keys or worker DB.
    // Confirm that limitation explicitly; this is not an archive success claim.
    do {
        _ = try await export.request()
        Issue.record("unconfigured export service unexpectedly accepted a request")
    } catch let error as ExportClientError {
        #expect(error == .server("service_unavailable"))
    }
    proof(22, "growth family serves sync/snapshot without feature markers; client export unavailable because this harness has no export configuration (no download claim)")

    clock.advance(by: 1)
    try joiner.store.editMeasurement(counterpart, at: counterpart.measuredAt, timeZoneID: "UTC", values: MeasurementValues(weightKG: 3.7))
    let late = try joiner.store.logMeasurement(for: offlineBaby.id, at: clock.now, timeZoneID: "UTC", values: MeasurementValues(weightKG: 3.8))
    let lateIntent = try #require(try joiner.store.nextDispatchBatch(limit: 100).first { $0.entityID == late.id })
    try owner.store.deleteBaby(offlineBaby)
    try owner.store.deleteBaby(legacy)
    try await owner.sync(.localWrite)
    try await joiner.sync(.localWrite)
    #expect(try joiner.store.baby(offlineBaby.id)?.isTombstoned == true)
    #expect(counterpart.isTombstoned)
    #expect(late.isTombstoned)
    #expect(try joiner.store.growthProfile(for: offlineBaby.id) == nil)
    let refused = try joiner.store.outboxEntry(lateIntent.mutationID)
    #expect(refused.deliveryState == .superseded && refused.supersededCode == "baby_deleted")
    #expect(try joiner.store.nextDispatchBatch(limit: 100).isEmpty)
    #expect(try owner.store.nextDispatchBatch(limit: 100).isEmpty)
    proof(23, "owner child deletion cascaded profile/check-ins and rejected racing measurement writes; both outboxes drained")
}
