import BabyCore
import BabyProtocol
@testable import BabyStore
import CryptoKit
import Foundation
import SwiftData
import Testing
@testable import BabySync
import HomeFeature
import LoggingFeature
@testable import SettingsFeature

/// The owner's install day, rehearsed against the real server.
///
/// Two caregivers build up a history: sessions joined live across phones, a
/// Combine, deletes, timers stopped and edited, solids with no foods, meds
/// and diapers. Then the owner's phone loses its store in the two ways a real
/// phone can:
///
/// 1. **The store is moved aside** (an install over an older build, ruling
///    B-R7). The App Group defaults and the Keychain survive, so the phone
///    comes back as the same device in `.shared`, is told to sign in, and
///    after signing in runs `activateAsOwner` with the device's deterministic
///    import checkpoint ID: the import an earlier store already finished on
///    the server. It must import its empty store and pull from cursor 0: every
///    entity's latest state, deletes as tombstones.
/// 2. **The app is deleted and installed again** (Phase C-3). iOS wipes the
///    App Group defaults and keeps the Keychain. The app starts `.localOnly`;
///    signing in offers no device-continuity proof (that lookup needs the
///    recorded session), so the server allocates a new device, and with no
///    `FamilyEntryRecord` for it the phone activates as a joiner and takes a
///    snapshot.
///
/// Both must end with exactly the server's records, session links included,
/// matching the other caregiver, with nothing stuck in the outbox.
/// `protocol-e2e.sh` checks the reinstalled phone's final counts and link
/// digests against Postgres from the `owner-reinstall ledger:` line.
@MainActor
@Suite("An owner reinstall through a live server", .serialized)
struct OwnerReinstallE2ETests {
    @Test func anOwnerReinstallReplaysTheWholeHistory() async throws {
        guard let environment = E2EEnvironment.current() else { return }
        let live = try LiveServer(environment)
        // In the past, so every timestamp has already happened for the
        // server; a fraction so millisecond round-trips are exercised.
        let clock = TestDateProvider(Date(timeIntervalSince1970: floor(Date().timeIntervalSince1970) - 8 * 3600 + 0.004))
        let scratch = FileManager.default.temporaryDirectory
            .appending(path: "owner-reinstall-e2e-\(UUID().uuidString)", directoryHint: .isDirectory)
        try FileManager.default.createDirectory(at: scratch, withIntermediateDirectories: true)
        var phones: [Phone] = []
        defer {
            for phone in phones { phone.forgetDefaults() }
            try? FileManager.default.removeItem(at: scratch)
        }

        let ownerSubject = UUID().uuidString.lowercased()
        let owner = try Phone(name: "owner", live: live, clock: clock, directory: scratch, appleSubject: ownerSubject)
        let caregiver = try Phone(name: "caregiver", live: live, clock: clock, directory: scratch,
                                  appleSubject: UUID().uuidString.lowercased())
        phones += [owner, caregiver]

        // MARK: 1. The owner's history before the family, imported.

        let baby = try owner.store.addBaby(name: "Reinstall Ліза", birthDate: clock.now.addingTimeInterval(-30 * 86_400),
                                           referenceSex: .girls, expectedDueDate: nil)
        let firstLeft = try owner.store.logBreast(side: .left, from: clock.now, to: clock.now.addingTimeInterval(600), for: baby.id)
        clock.advance(by: 15 * 60)
        let topUp = try owner.store.logBottle(volumeML: 60, contents: .formula, for: baby.id)
        try #require(topUp.feedingSessionID == firstLeft.id, "the top-up did not join the Left feed")
        clock.advance(by: 5 * 60)
        let firstNap = try owner.store.logSleep(from: clock.now, to: clock.now.addingTimeInterval(40 * 60), for: baby.id)
        clock.advance(by: 50 * 60)
        let secondNap = try owner.store.logSleep(from: clock.now, to: clock.now.addingTimeInterval(30 * 60), for: baby.id)
        try #require(secondNap.sleepSessionID == firstNap.id, "the second nap did not join the first")
        clock.advance(by: 40 * 60)
        _ = try await owner.signIn(displayName: "Owner Reinstall", live: live)
        try await owner.model.createFamily(name: "E2E owner reinstall")
        let ownerKey = try owner.familyKey()
        try owner.expectUpToDate("after creating the family and importing")
        let finishedImport = try #require(try completedImportID(in: owner.store), "the owner's import never completed")
        proof(1, "owner imported baby, a joined feed (\(firstLeft.id)+top-up) and a joined sleep; family=\(ownerKey.familyID) device=\(ownerKey.deviceID) import=\(finishedImport)")

        let invitation = try await owner.model.createInvitation()
        _ = try await caregiver.signIn(displayName: "Caregiver Reinstall", live: live)
        try await caregiver.model.acceptInvitation(try InvitationParser.parse(invitation.shareURL.absoluteString))
        try caregiver.expectUpToDate("after joining")

        // MARK: 2. History through both phones.

        // A feed session joined across phones.
        let ownerRight = try owner.store.logBreast(side: .right, from: clock.now, to: clock.now.addingTimeInterval(8 * 60), for: baby.id)
        try await owner.sync(.localWrite)
        try await caregiver.sync()
        clock.advance(by: 10 * 60)
        let caregiverBottle = try caregiver.store.logBottle(volumeML: 45, contents: .breastMilk, for: baby.id)
        try #require(caregiverBottle.feedingSessionID == ownerRight.id, "the caregiver's bottle did not join the owner's Right")
        clock.advance(by: 5 * 60)
        let caregiverSecondBottle = try caregiver.store.logBottle(volumeML: 20, contents: .formula, for: baby.id)
        try #require(caregiverSecondBottle.feedingSessionID == ownerRight.id)
        try await caregiver.sync(.localWrite)
        try await owner.sync()

        // A sleep session joined across phones.
        clock.advance(by: 30 * 60)
        let caregiverNap = try caregiver.store.logSleep(from: clock.now.addingTimeInterval(-40 * 60), to: clock.now, for: baby.id)
        try await caregiver.sync(.localWrite)
        try await owner.sync()
        clock.advance(by: 40 * 60)
        let ownerNap = try owner.store.logSleep(from: clock.now.addingTimeInterval(-30 * 60), to: clock.now, for: baby.id)
        try #require(ownerNap.sleepSessionID == caregiverNap.id, "the owner's nap did not join the caregiver's")
        try await owner.sync(.localWrite)
        try await caregiver.sync()

        // A feed timer the caregiver starts, the owner edits while it runs,
        // the caregiver stops, and the owner edits again once it is a feed.
        clock.advance(by: 60 * 60)
        _ = try caregiver.store.startPart(.breast(.left), for: baby.id)
        try await caregiver.sync(.localWrite)
        clock.advance(by: 6 * 60)
        try await owner.sync()
        let ownersTimer = try #require(try owner.store.activeTimer(kind: .feed, for: baby.id))
        let timerEdit = RunningTimerEditModel(timer: ownersTimer, store: owner.store)
        timerEdit.startedAt = ownersTimer.startedAt.addingTimeInterval(-3 * 60)
        #expect(try await timerEdit.save(), "the running-timer edit was refused: \(timerEdit.validationMessage ?? "-")")
        try await owner.sync(.localWrite)
        try await caregiver.sync()
        clock.advance(by: 60)
        let caregiversTimer = try #require(try caregiver.store.activeTimer(kind: .feed, for: baby.id))
        let timedFeed = try #require(try caregiver.store.stopFeed(caregiversTimer))
        try await caregiver.sync(.localWrite)
        try await owner.sync()
        let ownersTimedFeed = try #require(try owner.history().feeds.first { $0.id == timedFeed.id })
        let feedEdit = PartEditModel(store: owner.store, babyID: baby.id,
                                     target: .breast(feedID: ownersTimedFeed.id,
                                                     componentID: try #require(ownersTimedFeed.components.first?.id)))
        feedEdit.note = "edited after the stop"
        #expect(try feedEdit.save(), "the part editor refused: \(feedEdit.validationMessage ?? "-")")
        try await owner.sync(.localWrite)
        try await caregiver.sync()

        // A sleep timer the owner starts and the caregiver stops.
        clock.advance(by: 20 * 60)
        _ = try owner.store.startSleep(for: baby.id)
        try await owner.sync(.localWrite)
        clock.advance(by: 35 * 60)
        try await caregiver.sync()
        let sleepTimer = try #require(try caregiver.store.activeTimer(kind: .sleep, for: baby.id))
        let timedNap = try caregiver.store.stopSleep(sleepTimer)
        try await caregiver.sync(.localWrite)
        try await owner.sync()

        // Combine: the imported Left session and the timed feed, hours apart,
        // become one session rooted at the earliest part.
        try owner.store.combineFeeds([firstLeft.id, timedFeed.id], for: baby.id)
        try await owner.sync(.localWrite)
        try await caregiver.sync()

        // Solids with no foods, meds, diapers.
        clock.advance(by: 10 * 60)
        let solids = try caregiver.store.logSolids(foods: [], amount: .taste, for: baby.id)
        let vitamin = try owner.store.logSupplement(name: "Vitamin D", for: baby.id)
        let ownerDiaper = try owner.store.logDiaper(.wet, for: baby.id)
        try await owner.sync(.localWrite)
        clock.advance(by: 60)
        let iron = try caregiver.store.logSupplement(name: "Iron", for: baby.id, note: "with food")
        let caregiverDiaper = try caregiver.store.logDiaper(.dirty, for: baby.id, consistency: "soft")
        try await caregiver.sync(.localWrite)
        try await owner.sync()

        // Deletes, both ways: a session member, a standalone feed, a med and
        // a diaper. Each leaves a tombstone row on the server.
        clock.advance(by: 30 * 60)
        let standalone = try owner.store.logBottle(volumeML: 90, contents: .formula, for: baby.id)
        try await owner.sync(.localWrite)
        try await caregiver.sync()
        let deleted: [UUID] = [caregiverSecondBottle.id, standalone.id, iron.id, ownerDiaper.id]
        // Through the editors History opens: a bottle's part editor removes
        // its one component, and with it the entry (ruling H-R11).
        let ownersSecondBottle = try #require(try owner.history().feeds.first { $0.id == caregiverSecondBottle.id })
        try PartEditModel(store: owner.store, babyID: baby.id,
                          target: .bottle(feedID: ownersSecondBottle.id,
                                          componentID: try #require(ownersSecondBottle.components.first?.id))).delete()
        let caregiversStandalone = try #require(try caregiver.history().feeds.first { $0.id == standalone.id })
        try PartEditModel(store: caregiver.store, babyID: baby.id,
                          target: .bottle(feedID: caregiversStandalone.id,
                                          componentID: try #require(caregiversStandalone.components.first?.id))).delete()
        try #require(try owner.supplements().contains { $0.id == iron.id })
        try SupplementSheetModel(store: owner.store, babyID: baby.id, editing: iron.id).delete()
        try #require(try caregiver.history().diapers.contains { $0.id == ownerDiaper.id })
        try DiaperEditModel(store: caregiver.store, babyID: baby.id, diaperID: ownerDiaper.id).delete()
        try await owner.sync(.localWrite)
        try await caregiver.sync(.localWrite)
        try await owner.sync()
        try await caregiver.sync()

        let truth = try FamilyLedger(try await caregiver.freshSnapshot())
        try expectClean(owner)
        try expectClean(caregiver)
        expectSame(try FamilyLedger(owner.store), truth, "the owner before losing its store")
        expectSame(try FamilyLedger(caregiver.store), truth, "the caregiver")
        let expectedLinks: [UUID: UUID] = [
            topUp.id: firstLeft.id, timedFeed.id: firstLeft.id, caregiverBottle.id: ownerRight.id,
        ]
        for (feed, root) in expectedLinks {
            #expect(truth.feedLinks[feed] == root, "server link for \(feed) is \(String(describing: truth.feedLinks[feed])), expected \(root)")
        }
        #expect(truth.sleepLinks[secondNap.id] == firstNap.id && truth.sleepLinks[ownerNap.id] == caregiverNap.id)
        #expect(truth.feedLinks[solids.id] == nil && truth.sleepLinks[timedNap.id] == nil)
        #expect(Set(deleted).isDisjoint(with: truth.liveIDs))
        #expect(truth.liveIDs.isSuperset(of: [vitamin.id, caregiverDiaper.id, solids.id]))
        proof(2, "both phones and a fresh snapshot agree: \(truth.summary); links=\(truth.feedLinks.count) feed + \(truth.sleepLinks.count) sleep; deleted \(deleted.count)")

        // MARK: 3. The owner's store is moved aside; defaults and Keychain stay.

        await owner.model.engine?.stop()
        let movedAside = try Phone(
            name: "owner-moved-aside", live: live, clock: clock,
            storeURL: scratch.appending(path: "owner-moved-aside.store"),
            keychain: owner.keychain, suiteName: owner.suiteName, appleSubject: ownerSubject
        )
        movedAside.model.restoreSession()
        #expect(try movedAside.familyKey() == ownerKey, "the restored session is not the owner's")
        await movedAside.model.startSyncing()
        movedAside.model.sceneEnteredBackground()
        #expect(try movedAside.engine().status == .needsSignIn, "a fresh store under a restored session is \(movedAside.statusText)")
        proof(3, "fresh store under the kept defaults and Keychain restored family=\(ownerKey.familyID) as the same device; status=\(movedAside.statusText)")

        // MARK: 4. It signs in again: same device, so the same import ID.

        try await movedAside.model.signInWithApple()
        let movedAsideKey = try movedAside.familyKey()
        #expect(movedAsideKey == ownerKey, "signing in again moved the owner to \(movedAsideKey)")
        // What the server says to the import an earlier store finished on
        // this device, when it is asked to start again from an empty store.
        let reused = try await resolveEmptyImport(finishedImport, live: live, phone: movedAside, key: movedAsideKey)
        #expect(reused.code == "import_chunk_reused", "the server answered \(reused.status) \(reused.code ?? "-")")
        try movedAside.expectUpToDate("after signing in again over an empty store")
        let movedAsideImport = try #require(try completedImportID(in: movedAside.store), "the empty store's import never completed")
        #expect(movedAsideImport != finishedImport, "the empty store re-used the finished import \(finishedImport)")
        #expect(movedAside.store.activeRealm == ownerKey.realmID)
        #expect(movedAside.traffic.syncCursors.first == .some(nil),
                "the first pull after the empty import carried cursor \(String(describing: movedAside.traffic.syncCursors.first))")
        try expectClean(movedAside)
        expectSame(try FamilyLedger(movedAside.store), truth, "the owner after its store was moved aside")
        let movedAsideTombstones = try tombstonedIDs(in: movedAside.store)
        #expect(Set(deleted).isSubset(of: movedAsideTombstones), "the replay is missing tombstones: \(Set(deleted).subtracting(movedAsideTombstones))")
        proof(4, "signed in again as device \(movedAsideKey.deviceID); the server answers \(reused.status) \(reused.code ?? "-") to the finished import \(finishedImport); the empty store imported as \(movedAsideImport), pulled from cursor=\(cursorList(movedAside.traffic.syncCursors)), and matches the server exactly with \(movedAsideTombstones.count) tombstones replayed")

        // MARK: 5. The app is deleted and installed again.

        await movedAside.model.engine?.stop()
        let reinstalled = try Phone(
            name: "owner-reinstalled", live: live, clock: clock,
            storeURL: scratch.appending(path: "owner-reinstalled.store"),
            // iOS keeps the app's Keychain items across an uninstall and
            // wipes its App Group defaults.
            keychain: owner.keychain, suiteName: "owner-reinstall-e2e-\(UUID().uuidString)", appleSubject: ownerSubject
        )
        phones.append(reinstalled)
        reinstalled.model.restoreSession()
        #expect(reinstalled.model.engine == nil, "a fresh install restored a session from nothing")
        guard case .localOnly = reinstalled.model.mode else {
            throw E2EFailure("a fresh install started in \(reinstalled.model.mode)")
        }
        try await reinstalled.model.confirmEndpoint(live.endpoint)
        try await reinstalled.model.confirmDisplayName("Owner Reinstall")
        try await reinstalled.model.signInWithApple()
        let reinstalledKey = try reinstalled.familyKey()
        #expect(reinstalledKey.familyID == ownerKey.familyID && reinstalledKey.accountID == ownerKey.accountID)
        #expect(reinstalledKey.deviceID != ownerKey.deviceID, "a reinstall without defaults kept the old device")
        #expect(FamilyEntryRecord.entry(for: reinstalledKey, in: reinstalled.defaults) == nil)
        try reinstalled.expectUpToDate("after signing in on a fresh install")
        #expect(reinstalled.model.role == .owner, "the reinstalled owner reads role \(reinstalled.model.role)")
        #expect(try completedImportID(in: reinstalled.store) == nil, "a fresh install imported something")
        #expect(reinstalled.store.activeRealm == reinstalledKey.realmID)
        try expectClean(reinstalled)
        expectSame(try FamilyLedger(reinstalled.store), truth, "the reinstalled owner")
        proof(5, "fresh install (Keychain kept, defaults wiped) started localOnly, signed in as new device \(reinstalledKey.deviceID) with no continuity offered, activated as a joiner (no entry record), took the snapshot and pulled from cursor=\(cursorList(reinstalled.traffic.syncCursors)); role=\(reinstalled.model.role); matches the server exactly")

        // MARK: 6. Both phones keep working, and agree with the server.

        clock.advance(by: 20 * 60)
        let afterReinstall = try reinstalled.store.logDiaper(.mixed, for: baby.id)
        try await reinstalled.sync(.localWrite)
        try await caregiver.sync()
        clock.advance(by: 5 * 60)
        let fromCaregiver = try caregiver.store.logBottle(volumeML: 70, contents: .breastMilk, for: baby.id)
        try await caregiver.sync(.localWrite)
        try await reinstalled.sync()
        // Through the owner's account: the server allows each account two
        // unexpired snapshots, and the caregiver's join and the first truth
        // above are both still live.
        let finalTruth = try FamilyLedger(try await reinstalled.freshSnapshot())
        #expect(finalTruth.liveIDs.isSuperset(of: [afterReinstall.id, fromCaregiver.id]))
        try expectClean(reinstalled)
        try expectClean(caregiver)
        let reinstalledLedger = try FamilyLedger(reinstalled.store)
        expectSame(reinstalledLedger, finalTruth, "the reinstalled owner after a round trip")
        expectSame(try FamilyLedger(caregiver.store), finalTruth, "the caregiver after a round trip")
        proof(6, "a diaper from the reinstalled owner and a bottle from the caregiver crossed; both phones and a fresh snapshot agree: \(finalTruth.summary)")
        // Read by protocol-e2e.sh, which recomputes it from Postgres.
        print("owner-reinstall ledger: family=\(ownerKey.familyID.uuidString.lowercased()) \(reinstalledLedger.databaseLine)")
    }
}

// MARK: - What a phone holds

/// Every live record a store holds in its active realm, as text that must
/// match exactly between phones and the server. Timestamps are rounded to the
/// millisecond the wire carries; `createdBy` and `lastModifiedAt` are local
/// bookkeeping and left out.
struct FamilyLedger: Equatable {
    let rows: [String: [String]]
    let feedLinks: [UUID: UUID]
    let sleepLinks: [UUID: UUID]
    let liveIDs: Set<UUID>
    private let feedPairs: [(UUID, UUID?)]
    private let sleepPairs: [(UUID, UUID?)]

    static func == (lhs: FamilyLedger, rhs: FamilyLedger) -> Bool { lhs.rows == rhs.rows }

    @MainActor
    init(_ store: BabyStore) throws {
        let realm = store.activeRealm.encodedKey
        let babies = try store.context.fetch(FetchDescriptor<Baby>()).filter { $0.realmKey == realm && !$0.isTombstoned }
        let profiles = try store.context.fetch(FetchDescriptor<GrowthProfile>()).filter { $0.realmKey == realm && !$0.isTombstoned }
        let measurements = try store.context.fetch(FetchDescriptor<MeasurementCheckIn>()).filter { $0.realmKey == realm && !$0.isTombstoned }
        let feeds = try store.context.fetch(FetchDescriptor<FeedEntry>()).filter { $0.realmKey == realm && !$0.isTombstoned }
        let sleeps = try store.context.fetch(FetchDescriptor<SleepEntry>()).filter { $0.realmKey == realm && !$0.isTombstoned }
        let diapers = try store.context.fetch(FetchDescriptor<DiaperEntry>()).filter { $0.realmKey == realm && !$0.isTombstoned }
        let supplements = try store.context.fetch(FetchDescriptor<SupplementEntry>()).filter { $0.realmKey == realm && !$0.isTombstoned }
        let timers = try store.context.fetch(FetchDescriptor<ActiveTimer>()).filter { $0.realmKey == realm }
        rows = [
            "babies": babies.map { "\($0.id) \($0.name) born=\($0.birthDateISO ?? "-") order=\($0.sortOrder)" }.sorted(),
            "growth_profiles": profiles.map { "\($0.id) baby=\($0.babyID) sex=\($0.referenceSexRaw) due=\($0.expectedDueDateISO ?? "-")" }.sorted(),
            "measurements": measurements.map {
                "\($0.id) baby=\($0.babyID) at=\(ms($0.measuredAt)) kg=\(String(describing: $0.weightKG)) cm=\(String(describing: $0.lengthCM))"
            }.sorted(),
            "feeds": feeds.map {
                "\($0.id) baby=\($0.babyID) \($0.kindRaw) \(ms($0.startedAt))–\(ms($0.endedAt)) session=\(id($0.feedingSessionID)) tz=\($0.timeZoneID) note=\($0.note ?? "-") [\($0.components.map(component).joined(separator: "; "))]"
            }.sorted(),
            "sleeps": sleeps.map {
                "\($0.id) baby=\($0.babyID) \(ms($0.startedAt))–\(ms($0.endedAt)) session=\(id($0.sleepSessionID)) tz=\($0.timeZoneID) kind=\($0.kindOverrideRaw ?? "-") note=\($0.note ?? "-")"
            }.sorted(),
            "diapers": diapers.map {
                "\($0.id) baby=\($0.babyID) \($0.contentsRaw) at=\(ms($0.startedAt)) consistency=\($0.consistency ?? "-") color=\($0.color ?? "-") tz=\($0.timeZoneID) note=\($0.note ?? "-")"
            }.sorted(),
            "supplements": supplements.map {
                "\($0.id) baby=\($0.babyID) \($0.name) at=\(ms($0.startedAt)) logged=\(ms($0.loggedAt)) tz=\($0.timeZoneID) note=\($0.note ?? "-")"
            }.sorted(),
            "timers": timers.map { "\($0.id) baby=\($0.babyID) \($0.kindRaw) at=\(ms($0.startedAt))" }.sorted(),
        ]
        feedLinks = Dictionary(uniqueKeysWithValues: feeds.compactMap { feed in feed.feedingSessionID.map { (feed.id, $0) } })
        sleepLinks = Dictionary(uniqueKeysWithValues: sleeps.compactMap { sleep in sleep.sleepSessionID.map { (sleep.id, $0) } })
        feedPairs = feeds.map { ($0.id, $0.feedingSessionID) }
        sleepPairs = sleeps.map { ($0.id, $0.sleepSessionID) }
        liveIDs = Set(babies.map(\.id) + profiles.map(\.id) + measurements.map(\.id) + feeds.map(\.id) + sleeps.map(\.id)
            + diapers.map(\.id) + supplements.map(\.id) + timers.map(\.id))
    }

    /// The kinds in the order the script's SQL lists them.
    static let kinds = ["babies", "growth_profiles", "measurements", "feeds", "sleeps", "diapers", "supplements", "timers"]

    var summary: String { Self.kinds.map { "\($0)=\(rows[$0]?.count ?? 0)" }.joined(separator: " ") }

    /// Per-kind counts and an MD5 of every live `id>session` pair, sorted by
    /// id — the same string `protocol-e2e.sh` builds with SQL.
    var databaseLine: String {
        "\(summary) feed_links=\(Self.digest(feedPairs)) sleep_links=\(Self.digest(sleepPairs))"
    }

    private static func digest(_ pairs: [(UUID, UUID?)]) -> String {
        let text = pairs
            .map { ($0.0.uuidString.lowercased(), $0.1?.uuidString.lowercased() ?? "-") }
            .sorted { $0.0 < $1.0 }
            .map { "\($0.0)>\($0.1)" }
            .joined(separator: ",")
        return Insecure.MD5.hash(data: Data(text.utf8)).map { String(format: "%02x", $0) }.joined()
    }
}

private func ms(_ date: Date?) -> String {
    date.map { String(Int64(($0.timeIntervalSince1970 * 1000).rounded())) } ?? "-"
}

private func id(_ value: UUID?) -> String { value?.uuidString ?? "-" }

private func component(_ component: FeedComponent) -> String {
    switch component {
    case let .breast(id, side, seconds, start, end):
        "\(id) breast \(side.rawValue) \(seconds)s \(ms(start))–\(ms(end))"
    case let .bottle(id, volume, contents, seconds, start, end):
        "\(id) bottle \(volume) \(contents.rawValue) \(seconds.map(String.init) ?? "-")s \(ms(start))–\(ms(end))"
    case let .solids(id, foods, amount):
        "\(id) solids \(foods) \(amount.rawValue)"
    }
}

/// One `#expect` per differing kind, naming exactly which rows differ.
private func expectSame(_ actual: FamilyLedger, _ expected: FamilyLedger, _ who: String) {
    for kind in FamilyLedger.kinds {
        let mine = Set(actual.rows[kind] ?? [])
        let server = Set(expected.rows[kind] ?? [])
        #expect(mine == server, "\(who): \(kind) differ — only on the phone: \(mine.subtracting(server).sorted()); only on the server: \(server.subtracting(mine).sorted())")
    }
}

/// Nothing waiting to go, nothing the server refused.
@MainActor
private func expectClean(_ phone: Phone) throws {
    let rows = try phone.store.context.fetch(FetchDescriptor<SyncOutboxEntry>())
    let stuck = rows.filter { $0.deliveryState != .acknowledged }
    #expect(stuck.isEmpty, "\(phone.name) has outbox rows not acknowledged: \(stuck.map { "\($0.entityKind.rawValue) \($0.operationRaw) \($0.deliveryState) \($0.supersededCode ?? "-")" })")
    #expect(try phone.store.nextDispatchBatch(limit: 100).isEmpty, "\(phone.name) still has mutations to send")
}

/// The import this store finished, if any.
@MainActor
private func completedImportID(in store: BabyStore) throws -> UUID? {
    try store.context.fetch(FetchDescriptor<ImportCheckpoint>())
        .filter { $0.key.hasPrefix("import-state|") }
        .compactMap { try? JSONDecoder().decode(ImportCheckpointState.self, from: $0.payload) }
        .first { $0.phase == .complete }?.checkpointID
}

/// The entities this store holds as server tombstones.
///
/// Read from the canonical bases, not the visible rows. Pulls deliver each
/// entity's latest state (PostgreSQL authority, Phase 2), so an entity
/// created and deleted before a pull from cursor 0 arrives only as its
/// tombstone: `applyCanonical` stores it as a base with a null payload and
/// creates no row for it, exactly as a snapshot page's tombstone always has.
/// The legacy journal sent the create first, so a tombstoned row existed too.
@MainActor
private func tombstonedIDs(in store: BabyStore) throws -> Set<UUID> {
    let realm = store.activeRealm.encodedKey
    return Set(try store.context.fetch(FetchDescriptor<CanonicalEntityBase>()).filter {
        $0.realmKey == realm
            && (try? JSONSerialization.jsonObject(with: $0.payload, options: .fragmentsAllowed)) is NSNull
    }.map(\.entityID))
}

/// The server's answer, read raw, to chunk 0 of an empty-store import under
/// `importID` — the request an owner's empty store would send. Raw because
/// the client maps a code it does not know to `service_unavailable`.
@MainActor
private func resolveEmptyImport(_ importID: UUID, live: LiveServer, phone: Phone, key: FamilySessionKey) async throws -> (status: Int, code: String?) {
    let body = ImportResolveRequest(
        protocolVersion: "v1", capabilities: ImportCoordinator.capabilities, importID: importID,
        importMode: ImportMode.preserveLocalIDs.rawValue, normalizationVersion: ImportCoordinator.normalizationVersion,
        babies: [], records: [], totalRecordCount: 0, chunkIndex: 0, finalize: true
    )
    var request = URLRequest(url: URL(string: live.endpoint.origin.canonicalString + "/v1/family/import/resolve")!)
    request.httpMethod = "POST"
    request.setValue("Bearer \(try phone.credentials(key).accessToken)", forHTTPHeaderField: "Authorization")
    request.setValue("v1", forHTTPHeaderField: "X-Baby-Tracker-Protocol")
    request.setValue(ImportCoordinator.capabilities.joined(separator: ","), forHTTPHeaderField: "X-Baby-Tracker-Capabilities")
    request.setValue("application/json", forHTTPHeaderField: "Content-Type")
    request.httpBody = try HTTPImportResolverClient.canonicalizedJSON(body)
    let (data, response) = try await live.session.data(for: request)
    return ((response as? HTTPURLResponse)?.statusCode ?? 0, ServerErrorEnvelope.decode(data))
}

private func cursorList(_ cursors: [UInt64?]) -> String {
    "[" + cursors.map { $0.map(String.init) ?? "null" }.joined(separator: ",") + "]"
}

private func proof(_ step: Int, _ line: String) {
    print("owner-reinstall proof: step \(step): \(line)")
}
