import Foundation
import Testing
@testable import BabyCore
@testable import BabyProtocol
@testable import BabyStore

@MainActor
@Suite("Durable import")
struct ImportStoreTests {
    private func localStore() throws -> BabyStore {
        try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    }

    private func storeWithLocalData() throws -> (BabyStore, Baby, FeedEntry) {
        let store = try localStore()
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let feed = try store.logBottle(volumeML: 120, contents: .formula, for: baby.id)
        _ = try store.logDiaper(.wet, for: baby.id)
        return (store, baby, feed)
    }

    // MARK: Mode

    @Test func importModeIsImmutableFromChunkZero() throws {
        let store = try localStore()
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)

        #expect(throws: ImportStoreError.importModeChanged) {
            _ = try store.beginImport(checkpointID: checkpoint, mode: .matchShared)
        }
        #expect(try store.resumeImport(checkpointID: checkpoint).importMode == .preserveLocalIDs)
    }

    @Test func resumingAnUnknownImportFails() throws {
        let store = try localStore()
        #expect(throws: ImportStoreError.missingCheckpoint(ImportFixture.checkpointID)) {
            _ = try store.resumeImport(checkpointID: ImportFixture.checkpointID)
        }
    }

    // MARK: Freeze

    @Test func freezeCapturesEveryLiveEntityOnce() throws {
        let (store, baby, feed) = try storeWithLocalData()
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)

        let frozen = try store.freezeImportManifest(checkpointID: checkpoint)

        #expect(frozen.sourceBases[EntityKey(kind: "baby", id: baby.id)] != nil)
        #expect(frozen.sourceBases[EntityKey(kind: "feed", id: feed.id)] != nil)
        #expect(frozen.sourceBases.count == 4)
        #expect(try store.resumeImport(checkpointID: checkpoint).phase == .manifestFrozen)
    }

    /// Everything done before the freeze is in it; the freeze is a line, not a
    /// sampling.
    @Test func preFreezeCreateAndDeleteAreCapturedInOneRevision() throws {
        let (store, baby, feed) = try storeWithLocalData()
        let created = try store.logDiaper(.dirty, for: baby.id)
        try store.softDelete(feed)
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)

        let frozen = try store.freezeImportManifest(checkpointID: checkpoint)

        #expect(frozen.sourceBases[EntityKey(kind: "diaper", id: created.id)] != nil)
        #expect(frozen.sourceBases[EntityKey(kind: "feed", id: feed.id)] == nil)
    }

    /// A retried chunk must resend the frozen bytes. Rebuilding from live rows
    /// would silently drop a record the user deleted in the meantime, and the
    /// server would accept a different import than the one it half-received.
    @Test func frozenBytesSurviveALaterLocalDelete() throws {
        let (store, _, feed) = try storeWithLocalData()
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)
        let frozen = try store.freezeImportManifest(checkpointID: checkpoint)

        try store.softDelete(feed)

        let reread = try #require(try store.frozenManifest(checkpointID: checkpoint))
        #expect(reread == frozen)
        #expect(reread.chunks.first?.manifestHash == frozen.chunks.first?.manifestHash)
        #expect(reread.sourceBases[EntityKey(kind: "feed", id: feed.id)] != nil)
    }

    /// Freezing twice is the resume path, not a refreeze.
    @Test func freezingTwiceReturnsTheSameManifest() throws {
        let (store, _, _) = try storeWithLocalData()
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)

        let first = try store.freezeImportManifest(checkpointID: checkpoint)
        _ = try store.logDiaper(.wet, for: try #require(try store.babies().first).id)
        let second = try store.freezeImportManifest(checkpointID: checkpoint)

        #expect(first == second)
    }

    /// The resolver's `content_sha256` is the server's `CanonicalContentHash`,
    /// not a hash of this device's JSON bytes. Pinned to the same literals
    /// the Go side computes for the sleep and diaper fixtures, so a record
    /// frozen here is admitted rather than refused as a manifest mismatch.
    ///
    /// The sleep literal changed with v8: a client now always sends
    /// `sleep_session_id`, so this sleep (no session) hashes at v2 with the
    /// key present and null, rather than v1 without it.
    @Test func manifestRecordsCarryTheServerContentHash() throws {
        let store = try localStore()
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let sleep = SleepEntry(
            babyID: baby.id, startedAt: try WireFixture.date("2026-09-20T13:00:00Z"), timeZoneID: "Europe/Zurich"
        )
        sleep.endedAt = try WireFixture.date("2026-09-20T14:00:00Z")
        sleep.kindOverride = .nap
        store.context.insert(sleep)
        try store.record(.create, on: sleep)
        let diaper = try store.logDiaper(
            .mixed, for: baby.id, at: try WireFixture.date("2026-09-20T14:10:00Z"), consistency: "soft", color: "mustard"
        )
        diaper.timeZoneID = "Europe/Zurich"
        try store.update(diaper)
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)

        let records = try store.freezeImportManifest(checkpointID: checkpoint).records()

        #expect(records.first { $0.recordID == sleep.id }?.contentSHA256
            == "d3fd9350402e119e2bf99e814b8e963702f039660e1a2c8eb87198c3e61c5e5e")
        #expect(records.first { $0.recordID == diaper.id }?.contentSHA256
            == "32d53d1dd1b2ac16c08953f678d5f163d4804d243d5be00f1514e114552aa029")
    }

    /// A composite feed (two breast parts and a bottle) frozen into the
    /// owner's import: its manifest hash, and the hash of the import
    /// mutation queued for it, equal the one Go pins for the same parts in
    /// `TestCompositeFeedV4ContentHashMatchesClient`
    /// (`Server/internal/protocol/content_hash_test.go`).
    @Test func compositeFeedManifestHashMatchesTheServer() throws {
        let compositeSHA256 = "253a09be435d9c90eae64d6904bbdc97ee49149f3c39d0540eb1c2b85a133be5"
        let store = try localStore()
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let feed = FeedEntry(
            id: UUID(uuidString: "20000000-0000-4000-8000-0000000000b1")!, babyID: baby.id, kind: .breast,
            startedAt: try WireFixture.date("2026-09-24T08:00:00Z"), timeZoneID: "Europe/Zurich"
        )
        feed.endedAt = try WireFixture.date("2026-09-24T08:20:00Z")
        feed.components = [
            .breast(id: UUID(uuidString: "c9625297-288b-54c2-8e3b-6b80d0b1f9ff")!, side: .left, seconds: 300),
            .breast(id: UUID(uuidString: "5a0487fb-fcc3-5e2f-a7b5-a3b990f33dcd")!, side: .right, seconds: 240),
            .bottle(id: UUID(uuidString: "01b7623d-08d1-53d5-b0df-65221e4079ef")!, volumeML: 60, contents: .formula),
        ]
        store.context.insert(feed)
        try store.record(.create, on: feed)
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)

        let manifest = try store.freezeImportManifest(checkpointID: checkpoint)

        #expect(try manifest.records().first { $0.recordID == feed.id }?.contentSHA256 == compositeSHA256)
        try store.saveMapping([:], checkpointID: checkpoint)
        try store.rewriteAndMoveWorkspace(checkpointID: checkpoint, to: ImportFixture.realm)
        let imported = try #require(try store.liveOutboxRows(in: ImportFixture.realm).first { $0.entityID == feed.id })
            .decodedMutation()
        #expect(imported.payloadVersion == 4)
        #expect(try ProtocolCanonicalizer.contentSHA256(entityKind: "feed", payload: imported.payload) == compositeSHA256)
    }

    // MARK: Post-import intents

    @Test func editsAfterFreezeBecomeOrderedPostImportIntents() throws {
        let (store, baby, _) = try storeWithLocalData()
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)
        _ = try store.freezeImportManifest(checkpointID: checkpoint)

        let first = ImportFixture.mutation(entityID: baby.id, kind: .diaper, operation: .create)
        let second = ImportFixture.mutation(entityID: baby.id, kind: .feed, operation: .create)
        try store.appendPostImportIntent(first, checkpointID: checkpoint)
        try store.appendPostImportIntent(second, checkpointID: checkpoint)
        // Idempotent: a resumed retry must not double-queue.
        try store.appendPostImportIntent(first, checkpointID: checkpoint)

        #expect(try store.postImportIntents(checkpointID: checkpoint).map(\.id) == [first.id, second.id])
    }

    @Test func postImportIntentsNeverAlterTheFrozenManifest() throws {
        let (store, baby, _) = try storeWithLocalData()
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)
        let frozen = try store.freezeImportManifest(checkpointID: checkpoint)

        try store.appendPostImportIntent(
            ImportFixture.mutation(entityID: baby.id, kind: .diaper, operation: .create),
            checkpointID: checkpoint
        )

        #expect(try store.frozenManifest(checkpointID: checkpoint) == frozen)
    }

    // MARK: Mapping

    @Test func nonInjectiveMappingIsRejectedBeforeAnythingIsRewired() throws {
        let store = try localStore()
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .matchShared)
        let shared = UUID()

        #expect(throws: ImportStoreError.nonInjectiveMapping) {
            try store.saveMapping([UUID(): shared, UUID(): shared], checkpointID: checkpoint)
        }
        #expect(try store.resumeImport(checkpointID: checkpoint).phase == .membershipReady)
    }

    @Test func mappingIsDurableBeforeDependentIDsChange() throws {
        let (store, baby, feed) = try storeWithLocalData()
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .matchShared)
        _ = try store.freezeImportManifest(checkpointID: checkpoint)
        let canonical = UUID()

        try store.saveMapping([baby.id: canonical], checkpointID: checkpoint)

        // Persisted, but nothing rewired yet.
        #expect(try store.resumeImport(checkpointID: checkpoint).completeBabyIDMap == [baby.id: canonical])
        #expect(try store.feedEntryForTesting(feed.id)?.babyID == baby.id)
    }

    // MARK: Rewrite

    @Test func rewriteMovesEveryRowAndDependentReferenceAtomically() throws {
        let (store, baby, feed) = try storeWithLocalData()
        let checkpoint = UUID()
        let canonical = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .matchShared)
        _ = try store.freezeImportManifest(checkpointID: checkpoint)
        try store.saveMapping([baby.id: canonical], checkpointID: checkpoint)

        try store.rewriteAndMoveWorkspace(checkpointID: checkpoint, to: ImportFixture.realm)

        #expect(store.activeRealm == ImportFixture.realm)
        #expect(try store.babies(in: .local).isEmpty)
        #expect(try store.babies(in: ImportFixture.realm).map(\.id) == [canonical])
        #expect(try store.feedEntryForTesting(feed.id)?.babyID == canonical)
        #expect(try store.resumeImport(checkpointID: checkpoint).phase == .sharedRealmExposed)
    }

    /// The imported baseline is queued before the edits made while it was in
    /// flight, so the family sees the import and then the change.
    @Test func importMutationsAreQueuedBeforePostImportIntents() throws {
        let (store, baby, _) = try storeWithLocalData()
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)
        _ = try store.freezeImportManifest(checkpointID: checkpoint)
        let later = ImportFixture.mutation(entityID: baby.id, kind: .diaper, operation: .create)
        try store.appendPostImportIntent(later, checkpointID: checkpoint)
        try store.saveMapping([:], checkpointID: checkpoint)

        try store.rewriteAndMoveWorkspace(checkpointID: checkpoint, to: ImportFixture.realm)

        // Feed, diaper and growth profile, then the later edit. The baby is published by the
        // server's resolver and never queued as a create.
        let queued = try store.liveOutboxRows(in: ImportFixture.realm)
        #expect(queued.count == 4)
        #expect(queued.last?.id == later.id)
        #expect(queued.dropLast().allSatisfy { try! $0.decodedMutation().importID == checkpoint })
        #expect(try store.outboxEntry(later.id).decodedMutation().importID == nil)
    }

    @Test func rewriteBeforeMappingIsRefused() throws {
        let (store, _, _) = try storeWithLocalData()
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)
        _ = try store.freezeImportManifest(checkpointID: checkpoint)

        #expect(throws: ImportStoreError.wrongPhase(expected: .mappingDurable, actual: .manifestFrozen)) {
            try store.rewriteAndMoveWorkspace(checkpointID: checkpoint, to: ImportFixture.realm)
        }
        #expect(store.activeRealm == .local)
    }

    /// The realm must not move without the manifest that describes what is
    /// moving.
    @Test func rewriteWithoutAFrozenManifestIsRefusedAndLeavesTheRealmAlone() throws {
        let (store, _, _) = try storeWithLocalData()
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)
        try store.saveMapping([:], checkpointID: checkpoint)

        #expect(throws: ImportStoreError.missingFrozenManifest(checkpoint)) {
            try store.rewriteAndMoveWorkspace(checkpointID: checkpoint, to: ImportFixture.realm)
        }
        #expect(store.activeRealm == .local)
        #expect(try store.babies(in: .local).count == 1)
    }

    /// Resuming after the rewrite must not run it twice.
    @Test func resumeAfterRewriteReportsExposedRealm() throws {
        let (store, baby, _) = try storeWithLocalData()
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .matchShared)
        _ = try store.freezeImportManifest(checkpointID: checkpoint)
        try store.saveMapping([baby.id: baby.id], checkpointID: checkpoint)
        try store.rewriteAndMoveWorkspace(checkpointID: checkpoint, to: ImportFixture.realm)

        let resumed = try store.resumeImport(checkpointID: checkpoint)
        #expect(resumed.phase == .sharedRealmExposed)
        #expect(resumed.targetRealmKey == ImportFixture.realm.encodedKey)
    }

    // MARK: Validator

    @Test func ownerConflictChoiceEncodesOnlyTheTwoValidShapes() {
        #expect(ImportConflictResolution(choice: .useExisting, canonicalBabyID: UUID()).isValid)
        #expect(ImportConflictResolution(choice: .keepLocal, canonicalBabyID: nil).isValid)
        #expect(!ImportConflictResolution(choice: .useExisting, canonicalBabyID: nil).isValid)
        #expect(!ImportConflictResolution(choice: .keepLocal, canonicalBabyID: UUID()).isValid)
    }

    @Test func pageTokensAreBoundedBeforeTransport() {
        #expect(throws: Never.self) { try ImportStoreValidator.validatePageToken(String(repeating: "a", count: 512)) }
        #expect(throws: ImportStoreError.invalidResolution) {
            try ImportStoreValidator.validatePageToken(String(repeating: "a", count: 513))
        }
    }

    @Test func injectiveMappingIsAccepted() {
        #expect(throws: Never.self) {
            try ImportStoreValidator.validate([UUID(): UUID(), UUID(): UUID()])
        }
    }
}

enum ImportFixture {
    static let checkpointID = UUID(uuidString: "cccccccc-cccc-4ccc-8ccc-cccccccccccc")!
    static let realm = RealmID.shared(
        canonicalOrigin: "https://sync.example",
        serverInstanceID: UUID(uuidString: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")!,
        familyID: UUID(uuidString: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")!
    )

    static func mutation(entityID: UUID, kind: WireEntityKind, operation: WireOperation) -> WireMutation {
        WireMutation(
            mutationID: UUID(), entityID: entityID, entityKind: kind, operation: operation,
            payloadVersion: 1, observedEntityVersion: 0,
            clientMutationTime: "2026-09-21T00:00:00.000Z", importID: nil, payload: .object([:])
        )
    }
}

@MainActor
@Suite("Import rewrite excludes concurrent writers")
struct ImportRewriteRaceTests {
    /// The race the import has to survive: a widget process captures its write
    /// context, the app then moves the whole workspace into the shared realm,
    /// and the widget wakes up and saves against the context it captured
    /// before the move.
    ///
    /// Two real processes, because the in-process lock proves nothing about
    /// this. The requirement is not merely that the stale write fails — it is
    /// that it leaves NOTHING behind: no diaper in either realm, no outbox row
    /// to be dispatched later. A half-written row here is a record that exists
    /// in no family and syncs nowhere.
    @Test func importRewriteLeaseExcludesWidgetWriter() throws {
        let harness = try LeaseTestHarness()
        let store = harness.store
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let checkpoint = UUID()
        _ = try store.beginImport(checkpointID: checkpoint, mode: .preserveLocalIDs)
        _ = try store.freezeImportManifest(checkpointID: checkpoint)
        try store.saveMapping([:], checkpointID: checkpoint)

        // The widget captures its context and parks.
        let widget = try harness.launchStaleWriter(babyID: baby.id)

        // The app moves the workspace out from under it.
        try store.rewriteAndMoveWorkspace(checkpointID: checkpoint, to: ImportFixture.realm)

        // Only now does the widget try to save.
        let result = try widget.resumeAndWait()
        #expect(result == .retryAfterTransition)

        let reopened = try harness.reopenStore()
        #expect(try reopened.diaperCountForTesting() == 0)
        #expect(try reopened.liveOutboxRows(in: ImportFixture.realm).allSatisfy { $0.entityKind != .diaper })

        // A writer that recaptures its context succeeds, and lands in the new
        // realm with exactly one queued intent.
        #expect(try harness.runFreshWriter(babyID: baby.id) == .committed)
        let after = try harness.reopenStore()
        #expect(try after.diaperCountForTesting() == 1)
        #expect(try after.liveOutboxRows(in: ImportFixture.realm).filter { $0.entityKind == .diaper }.count == 1)
    }
}
