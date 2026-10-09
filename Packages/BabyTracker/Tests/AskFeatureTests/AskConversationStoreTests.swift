import Foundation
import Testing
@testable import AskFeature

@MainActor
private func fixture() -> (AskConversationStore, URL) {
    let root = FileManager.default.temporaryDirectory.appending(path: "ask-tests-\(UUID().uuidString)")
    return (AskConversationStore(root: root), root)
}

@MainActor
private func turn(_ text: String) -> AskTurn {
    AskTurn(id: UUID(), question: text, answer: "Answer", askedAt: Date(timeIntervalSince1970: 100),
            answeredAt: Date(timeIntervalSince1970: 101), sources: [])
}

@Test func existingSavedTurnWithoutAnswerKindStillDecodes() throws {
    let json = """
        {"id":"00000000-0000-0000-0000-000000000001","question":"Old question","answer":"Old answer","askedAt":0,"answeredAt":1,"sources":[]}
        """
    let decoded = try JSONDecoder().decode(AskTurn.self, from: Data(json.utf8))
    #expect(decoded.question == "Old question")
    #expect(decoded.answerKind == nil)
}

@MainActor
@Test func conversationSurvivesStoreRecreation() throws {
    let (store, root) = fixture()
    defer { try? FileManager.default.removeItem(at: root) }
    let identity = AskIdentity.local(UUID())
    let baby = UUID()
    try store.save([turn("What happened?")], identity: identity, babyID: baby)
    let reopened = AskConversationStore(root: root)
    #expect(try reopened.load(identity: identity, babyID: baby).map(\.question) == ["What happened?"])
}

@MainActor
@Test func conversationsAreScopedByBabyAndAccount() throws {
    let (store, root) = fixture()
    defer { try? FileManager.default.removeItem(at: root) }
    let one = AskIdentity.account(UUID())
    let two = AskIdentity.account(UUID())
    let babyA = UUID(), babyB = UUID()
    try store.save([turn("A")], identity: one, babyID: babyA)
    try store.save([turn("B")], identity: one, babyID: babyB)
    #expect(try store.load(identity: two, babyID: babyA).isEmpty)
    #expect(try store.load(identity: one, babyID: babyB).map(\.question) == ["B"])
}

@MainActor
@Test func clearDeletesOnlyTheSelectedConversation() throws {
    let (store, root) = fixture()
    defer { try? FileManager.default.removeItem(at: root) }
    let identity = AskIdentity.account(UUID())
    let babyA = UUID(), babyB = UUID()
    try store.save([turn("A")], identity: identity, babyID: babyA)
    try store.save([turn("B")], identity: identity, babyID: babyB)
    try store.clear(identity: identity, babyID: babyA)
    #expect(try store.load(identity: identity, babyID: babyA).isEmpty)
    #expect(try store.load(identity: identity, babyID: babyB).map(\.question) == ["B"])
}

@MainActor
@Test func savedBabyIDsCanBeCheckedForOfflineTombstonesPerIdentity() throws {
    let (store, root) = fixture()
    defer { try? FileManager.default.removeItem(at: root) }
    let one = AskIdentity.account(UUID()), two = AskIdentity.account(UUID())
    let deleted = UUID(), kept = UUID()
    try store.save([turn("old")], identity: one, babyID: deleted)
    try store.save([turn("current")], identity: one, babyID: kept)
    try store.save([turn("other caregiver")], identity: two, babyID: deleted)
    #expect(Set(try store.babyIDs(identity: one)) == Set([deleted, kept]))
    try store.clear(identity: one, babyID: deleted)
    #expect(Set(try store.babyIDs(identity: one)) == Set([kept]))
    #expect(try store.load(identity: two, babyID: deleted).count == 1)
}

@MainActor
@Test func localConversationCanMoveToTheSameCaregiverAccount() throws {
    let (store, root) = fixture()
    defer { try? FileManager.default.removeItem(at: root) }
    let localID = UUID(), accountID = UUID(), baby = UUID()
    try store.save([turn("Before joining")], identity: .local(localID), babyID: baby)
    try store.promote(localID: localID, accountID: accountID, babyID: baby)
    #expect(try store.load(identity: .local(localID), babyID: baby).isEmpty)
    #expect(try store.load(identity: .account(accountID), babyID: baby).map(\.question) == ["Before joining"])
}

@MainActor
@Test func interruptedPromotionFinishesDeletingLocalCopy() throws {
    let (store, root) = fixture()
    defer { try? FileManager.default.removeItem(at: root) }
    let localID = UUID(), accountID = UUID(), baby = UUID()
    let transcript = [turn("Private history")]
    try store.save(transcript, identity: .local(localID), babyID: baby)
    try store.save(transcript, identity: .account(accountID), babyID: baby)
    try store.promote(localID: localID, accountID: accountID, babyID: baby)
    #expect(try store.load(identity: .local(localID), babyID: baby).isEmpty)
    #expect(try store.load(identity: .account(accountID), babyID: baby) == transcript)
}

@MainActor
@Test func removalIsScopedToBabyOrIdentity() throws {
    let (store, root) = fixture()
    defer { try? FileManager.default.removeItem(at: root) }
    let one = AskIdentity.account(UUID()), two = AskIdentity.account(UUID())
    let babyA = UUID(), babyB = UUID()
    try store.save([turn("one A")], identity: one, babyID: babyA)
    try store.save([turn("one B")], identity: one, babyID: babyB)
    try store.save([turn("two A")], identity: two, babyID: babyA)
    try store.removeBaby(babyA)
    #expect(try store.load(identity: one, babyID: babyA).isEmpty)
    #expect(try store.load(identity: two, babyID: babyA).isEmpty)
    try store.removeIdentity(one)
    #expect(try store.load(identity: one, babyID: babyB).isEmpty)
}

@MainActor
@Test func corruptedChatDoesNotDeleteItsFile() throws {
    let (store, root) = fixture()
    defer { try? FileManager.default.removeItem(at: root) }
    let identity = AskIdentity.local(UUID()), baby = UUID()
    try store.save([turn("Good")], identity: identity, babyID: baby)
    let file = store.fileURL(identity: identity, babyID: baby)
    try Data("broken".utf8).write(to: file)
    #expect(throws: DecodingError.self) { try store.load(identity: identity, babyID: baby) }
    #expect(FileManager.default.fileExists(atPath: file.path))
}

@MainActor
@Test func chatFilesAreExcludedFromBackup() throws {
    let (store, root) = fixture()
    defer { try? FileManager.default.removeItem(at: root) }
    let identity = AskIdentity.local(UUID()), baby = UUID()
    try store.save([turn("Private")], identity: identity, babyID: baby)
    let file = store.fileURL(identity: identity, babyID: baby)
    #expect(try file.resourceValues(forKeys: [.isExcludedFromBackupKey]).isExcludedFromBackup == true)
}

@MainActor
@Test func versionOneChatLoadsWithEmptyMemory() throws {
    let (store, root) = fixture()
    defer { try? FileManager.default.removeItem(at: root) }
    let identity = AskIdentity.local(UUID()), baby = UUID()
    let file = store.fileURL(identity: identity, babyID: baby)
    try FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
    let json = """
        {"version":1,"turns":[{"id":"00000000-0000-0000-0000-000000000001","question":"Old?","answer":"Yes.","askedAt":0,"answeredAt":1,"sources":[]}]}
        """
    try Data(json.utf8).write(to: file)
    let loaded = try store.loadConversation(identity: identity, babyID: baby)
    #expect(loaded.turns.map(\.question) == ["Old?"])
    #expect(loaded.memory == nil)
    #expect(loaded.memoryCoversThrough == nil)
    #expect(loaded.turns[0].queriedRange == nil)
}

@MainActor
@Test func conversationRoundTripsMemoryAndQueriedRange() throws {
    let (store, root) = fixture()
    defer { try? FileManager.default.removeItem(at: root) }
    let identity = AskIdentity.local(UUID()), baby = UUID()
    let range = AskWindow(start: Date(timeIntervalSince1970: 100), end: Date(timeIntervalSince1970: 200))
    let first = turn("First")
    let second = AskTurn(id: UUID(), question: "Second", answer: "Answer", askedAt: Date(timeIntervalSince1970: 300),
                         answeredAt: Date(timeIntervalSince1970: 301), sources: [], answerKind: .recorded,
                         queriedRange: range)
    let conversation = AskConversation(turns: [first, second], memory: "3 Oct: prefers ml.",
                                       memoryCoversThrough: first.id)
    try store.save(conversation, identity: identity, babyID: baby)
    #expect(try AskConversationStore(root: root).loadConversation(identity: identity, babyID: baby) == conversation)
}

@MainActor
@Test func promotionCarriesMemoryToTheAccount() throws {
    let (store, root) = fixture()
    defer { try? FileManager.default.removeItem(at: root) }
    let localID = UUID(), accountID = UUID(), baby = UUID()
    let first = turn("Before joining")
    let conversation = AskConversation(turns: [first], memory: "Notes", memoryCoversThrough: first.id)
    try store.save(conversation, identity: .local(localID), babyID: baby)
    try store.promote(localID: localID, accountID: accountID, babyID: baby)
    #expect(try store.loadConversation(identity: .account(accountID), babyID: baby) == conversation)
    #expect(try store.load(identity: .local(localID), babyID: baby).isEmpty)
}

@MainActor
@Test func verbatimTurnsStartAfterTheSummarizedTurn() {
    let turns = ["A", "B", "C"].map(turn)
    #expect(AskConversation(turns: turns).verbatimTurns.map(\.question) == ["A", "B", "C"])
    #expect(AskConversation(turns: turns, memory: "m", memoryCoversThrough: turns[1].id)
        .verbatimTurns.map(\.question) == ["C"])
    #expect(AskConversation(turns: turns, memory: "m", memoryCoversThrough: UUID())
        .verbatimTurns.map(\.question) == ["A", "B", "C"])
}
