import BabyStore
import Foundation
import Testing
@testable import BabySync

@Suite("InvitationCodeStore keeps invitation links per realm")
struct InvitationCodeStoreTests {
    private static let familyA = RealmID.shared(
        canonicalOrigin: "https://sync.example.test", serverInstanceID: UUID(), familyID: UUID()
    )
    private static let familyB = RealmID.shared(
        canonicalOrigin: "https://sync.example.test", serverInstanceID: UUID(), familyID: UUID()
    )

    private func store(realm: RealmID = familyA, backend: InMemoryKeychainBackend = InMemoryKeychainBackend()) -> (InvitationCodeStore, InMemoryKeychainBackend) {
        (InvitationCodeStore(backend: backend, realm: realm), backend)
    }

    @Test func savesAndReadsBackTheLink() throws {
        let (store, _) = store()
        let id = UUID()
        let link = try #require(URL(string: "https://sync.example.test/invite#token=abcdefghijklmnop0123456789"))

        try store.save(link, for: id)

        #expect(store.shareURL(for: id) == link)
    }

    @Test func aNeverSavedIDHasNoLink() {
        let (store, _) = store()
        #expect(store.shareURL(for: UUID()) == nil)
    }

    @Test func removeForgetsExactlyThatOne() throws {
        let (store, _) = store()
        let keep = UUID()
        let drop = UUID()
        let link = try #require(URL(string: "https://sync.example.test/invite#token=abcdefghijklmnop0123456789"))
        try store.save(link, for: keep)
        try store.save(link, for: drop)

        try store.remove(drop)

        #expect(store.shareURL(for: keep) == link)
        #expect(store.shareURL(for: drop) == nil)
    }

    /// A revoked or expired invitation the server no longer lists should not
    /// leave an orphaned code behind forever.
    @Test func pruneDropsEverythingNotKept() throws {
        let (store, _) = store()
        let keep = UUID()
        let stale = UUID()
        let link = try #require(URL(string: "https://sync.example.test/invite#token=abcdefghijklmnop0123456789"))
        try store.save(link, for: keep)
        try store.save(link, for: stale)

        try store.prune(keeping: [keep])

        #expect(store.shareURL(for: keep) == link)
        #expect(store.shareURL(for: stale) == nil)
    }

    @Test func pruneToEverythingKeepsEverything() throws {
        let (store, _) = store()
        let first = UUID()
        let second = UUID()
        let link = try #require(URL(string: "https://sync.example.test/invite#token=abcdefghijklmnop0123456789"))
        try store.save(link, for: first)
        try store.save(link, for: second)

        try store.prune(keeping: [first, second])

        #expect(store.shareURL(for: first) == link)
        #expect(store.shareURL(for: second) == link)
    }

    /// Two families sharing one device's Keychain must never see each
    /// other's invitation codes.
    @Test func twoRealmsStayIsolated() throws {
        let backend = InMemoryKeychainBackend()
        let (familyA, _) = store(realm: Self.familyA, backend: backend)
        let (familyB, _) = store(realm: Self.familyB, backend: backend)
        let id = UUID()
        let linkA = try #require(URL(string: "https://sync.example.test/invite#token=aaaaaaaaaaaaaaaaaaaaaaaaaaa"))
        let linkB = try #require(URL(string: "https://sync.example.test/invite#token=bbbbbbbbbbbbbbbbbbbbbbbbbbb"))

        try familyA.save(linkA, for: id)
        try familyB.save(linkB, for: id)

        #expect(familyA.shareURL(for: id) == linkA)
        #expect(familyB.shareURL(for: id) == linkB)

        try familyA.removeAll()

        #expect(familyA.shareURL(for: id) == nil)
        #expect(familyB.shareURL(for: id) == linkB)
    }

    /// `RealmID.local` carries no family id at all — the store must still be
    /// able to name a slot for it, and that slot must stay as isolated from a
    /// shared realm as any two shared realms are from each other.
    @Test func theLocalRealmIsIsolatedToo() throws {
        let backend = InMemoryKeychainBackend()
        let (local, _) = store(realm: .local, backend: backend)
        let (shared, _) = store(realm: Self.familyA, backend: backend)
        let id = UUID()
        let localLink = try #require(URL(string: "https://sync.example.test/invite#token=llllllllllllllllllllllllll"))
        let sharedLink = try #require(URL(string: "https://sync.example.test/invite#token=ssssssssssssssssssssssssss"))

        try local.save(localLink, for: id)
        try shared.save(sharedLink, for: id)

        #expect(local.shareURL(for: id) == localLink)
        #expect(shared.shareURL(for: id) == sharedLink)
    }

    @Test func removeAllClearsEveryKeptCode() throws {
        let (store, _) = store()
        let first = UUID()
        let second = UUID()
        let link = try #require(URL(string: "https://sync.example.test/invite#token=abcdefghijklmnop0123456789"))
        try store.save(link, for: first)
        try store.save(link, for: second)

        try store.removeAll()

        #expect(store.shareURL(for: first) == nil)
        #expect(store.shareURL(for: second) == nil)

        // And leaves nothing to re-prune against: a subsequent prune with an
        // empty keep set must not throw on an already-empty index.
        try store.prune(keeping: [])
    }
}
