import BabyProtocol
import BabyStore
import Foundation
import Testing
@testable import BabySync

/// The App Group ledger: versions only ever go up, across processes; tokens
/// never land in it; and a ledger that does not belong to this installation,
/// account or epoch is discarded rather than trusted.
struct LiveActivityPushRegistrationStoreTests {
    private let identity = LiveActivityRegistrationIdentity(familyKey: PushFixture.familyKey())

    @Test func twoIndependentStoresNeverHandOutTheSameVersion() async throws {
        let directory = PushFixture.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let keychain = InMemoryKeychainBackend()
        let identity = identity
        let installation = try LiveActivityRegistrationStore(directory: directory, keychain: keychain).bind(identity)
        // Two store objects stand in for the app and an intent process: no
        // shared memory, only the file and its lock.
        let first = LiveActivityRegistrationStore(directory: directory, keychain: keychain)
        let second = LiveActivityRegistrationStore(directory: directory, keychain: keychain)
        let allocated = try await withThrowingTaskGroup(of: [Int64].self) { group in
            for store in [first, second, first, second] {
                group.addTask {
                    var mine: [Int64] = []
                    for _ in 0..<25 {
                        mine.append(try store.update { ledger in
                            ledger.registrationVersion += 1
                            ledger.pendingRemovals.append(.init(familyID: PushFixture.familyID, installationID: installation, version: ledger.registrationVersion))
                            return ledger.registrationVersion
                        })
                    }
                    return mine
                }
            }
            var all: [Int64] = []
            for try await batch in group { all += batch }
            return all
        }
        #expect(allocated.count == 100)
        #expect(Set(allocated).count == 100)
        let final = try first.read()
        #expect(final.registrationVersion == 100)
        // Not one removal was lost to an interleaved read-modify-write.
        #expect(final.pendingRemovals.count == 100)
        #expect(Set(final.pendingRemovals.map(\.version)) == Set(1...100))
    }

    @Test func tokensLiveInTheKeychainAndNeverInTheLedgerFile() throws {
        let directory = PushFixture.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let keychain = InMemoryKeychainBackend()
        let store = LiveActivityRegistrationStore(directory: directory, keychain: keychain)
        let installation = try store.bind(identity)
        try store.update { ledger in
            try store.saveToken(PushFixture.startToken, for: .pushToStart(installation))
            try store.saveToken(PushFixture.activityToken, for: .activity(installation, "activity-1"))
            ledger.registrationVersion = 1
        }
        #expect(try store.token(for: .pushToStart(installation)) == PushFixture.startToken)
        #expect(try store.token(for: .activity(installation, "activity-1")) == PushFixture.activityToken)
        let bytes = try Data(contentsOf: store.ledgerURL)
        let text = String(decoding: bytes, as: UTF8.self)
        #expect(!text.contains(PushFixture.startToken))
        #expect(!text.contains(PushFixture.activityToken))
        #expect(text.contains(installation.uuidString))
    }

    @Test func sameIdentityKeepsTheInstallationAcrossRelaunch() throws {
        let directory = PushFixture.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let keychain = InMemoryKeychainBackend()
        let first = try LiveActivityRegistrationStore(directory: directory, keychain: keychain).bind(identity)
        try LiveActivityRegistrationStore(directory: directory, keychain: keychain).update { $0.registrationVersion = 9 }
        let relaunched = LiveActivityRegistrationStore(directory: directory, keychain: keychain)
        #expect(try relaunched.bind(identity) == first)
        #expect(try relaunched.read().registrationVersion == 9)
    }

    @Test func anotherAccountRotatesTheInstallationAndClearsScopedState() throws {
        let directory = PushFixture.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let keychain = InMemoryKeychainBackend()
        let store = LiveActivityRegistrationStore(directory: directory, keychain: keychain)
        let first = try store.bind(identity)
        try store.update { ledger in
            ledger.registrationVersion = 4
            ledger.pendingRemovals = [.init(familyID: PushFixture.familyID, installationID: first, version: 4)]
            try store.saveToken(PushFixture.startToken, for: .pushToStart(first))
        }
        let other = LiveActivityRegistrationIdentity(familyKey: PushFixture.familyKey(accountID: PushFixture.otherAccountID))
        let rotated = try store.bind(other)
        #expect(rotated != first)
        let ledger = try store.read()
        #expect(ledger.identity == other)
        #expect(ledger.registrationVersion == 0)
        // The old account's removal cannot run under the new account.
        #expect(ledger.pendingRemovals.isEmpty)
        #expect(try store.token(for: .pushToStart(first)) == nil)
    }

    @Test func aNewRecoveryEpochIsANewInstallation() throws {
        let directory = PushFixture.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let store = LiveActivityRegistrationStore(directory: directory, keychain: InMemoryKeychainBackend())
        let first = try store.bind(identity)
        let next = try store.bind(LiveActivityRegistrationIdentity(familyKey: PushFixture.familyKey(epoch: PushFixture.epoch + 1)))
        #expect(first != next)
    }

    @Test func aFamilySwitchKeepsTheInstallationAndItsVersions() throws {
        let directory = PushFixture.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let store = LiveActivityRegistrationStore(directory: directory, keychain: InMemoryKeychainBackend())
        let first = try store.bind(identity)
        try store.update { $0.registrationVersion = 6 }
        // Same account and device: the server fences the old family by a
        // newer installation version, so the counter must carry on.
        let switched = try store.bind(LiveActivityRegistrationIdentity(familyKey: PushFixture.familyKey(familyID: PushFixture.otherFamilyID)))
        #expect(switched == first)
        #expect(try store.read().registrationVersion == 6)
    }

    @Test func aRestoredLedgerWithoutItsKeychainBindingIsRejected() throws {
        let directory = PushFixture.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let original = InMemoryKeychainBackend()
        let first = try LiveActivityRegistrationStore(directory: directory, keychain: original).bind(identity)
        try LiveActivityRegistrationStore(directory: directory, keychain: original).update { $0.registrationVersion = 12 }
        // A device-restore copies App Group files but not this-device-only
        // Keychain items: the ledger arrives without its binding.
        let restored = LiveActivityRegistrationStore(directory: directory, keychain: InMemoryKeychainBackend())
        let installation = try restored.bind(identity)
        #expect(installation != first)
        #expect(try restored.read().registrationVersion == 0)
    }

    @Test func aGarbledLedgerIsReplacedNotTrusted() throws {
        let directory = PushFixture.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let keychain = InMemoryKeychainBackend()
        let store = LiveActivityRegistrationStore(directory: directory, keychain: keychain)
        let first = try store.bind(identity)
        try Data("{\"schema\":1,\"registrationVersion\":\"lots\"".utf8).write(to: store.ledgerURL)
        let second = try store.bind(identity)
        #expect(second != first)
        #expect(try store.read().identity == identity)
    }
}
