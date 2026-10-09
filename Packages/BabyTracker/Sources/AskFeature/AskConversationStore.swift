import Foundation

/// A display transcript in the app's private container, never in BabyStore's App Group.
@MainActor
public final class AskConversationStore {
    private let root: URL
    private let files = FileManager.default

    public init(root: URL) {
        self.root = root
    }

    struct Envelope: Codable {
        let version: Int
        let turns: [AskTurn]
        var memory: String?
        var memoryCoversThrough: UUID?
    }

    func fileURL(identity: AskIdentity, babyID: UUID) -> URL {
        root.appending(path: identity.directoryName, directoryHint: .isDirectory)
            .appending(path: "\(babyID.uuidString.lowercased()).json")
    }

    public func loadConversation(identity: AskIdentity, babyID: UUID) throws -> AskConversation {
        let file = fileURL(identity: identity, babyID: babyID)
        guard files.fileExists(atPath: file.path) else { return AskConversation() }
        let envelope = try JSONDecoder().decode(Envelope.self, from: Data(contentsOf: file))
        // Version 1 files predate memory and load with none.
        guard envelope.version == 1 || envelope.version == 2 else { throw AskConversationError.unsupportedVersion }
        return AskConversation(turns: envelope.turns, memory: envelope.memory,
                               memoryCoversThrough: envelope.memoryCoversThrough)
    }

    public func load(identity: AskIdentity, babyID: UUID) throws -> [AskTurn] {
        try loadConversation(identity: identity, babyID: babyID).turns
    }

    public func save(_ conversation: AskConversation, identity: AskIdentity, babyID: UUID) throws {
        let file = fileURL(identity: identity, babyID: babyID)
        try files.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
        try markPrivate(file.deletingLastPathComponent())
        let envelope = Envelope(version: 2, turns: conversation.turns, memory: conversation.memory,
                                memoryCoversThrough: conversation.memoryCoversThrough)
        try JSONEncoder().encode(envelope).write(to: file, options: .atomic)
        try markPrivate(file)
    }

    /// A conversation with no memory yet.
    func save(_ turns: [AskTurn], identity: AskIdentity, babyID: UUID) throws {
        try save(AskConversation(turns: turns), identity: identity, babyID: babyID)
    }

    public func clear(identity: AskIdentity, babyID: UUID) throws {
        let file = fileURL(identity: identity, babyID: babyID)
        if files.fileExists(atPath: file.path) { try files.removeItem(at: file) }
    }

    /// Discover chats for the active identity so a tombstone received while
    /// the app was closed can be cleaned up on the next launch.
    public func babyIDs(identity: AskIdentity) throws -> [UUID] {
        let directory = root.appending(path: identity.directoryName, directoryHint: .isDirectory)
        guard files.fileExists(atPath: directory.path) else { return [] }
        return try files.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil)
            .compactMap { file -> UUID? in
                guard file.pathExtension == "json" else { return nil }
                return UUID(uuidString: file.deletingPathExtension().lastPathComponent)
            }
    }

    public func removeBaby(_ babyID: UUID) throws {
        guard files.fileExists(atPath: root.path) else { return }
        for directory in try files.contentsOfDirectory(at: root, includingPropertiesForKeys: nil) {
            let file = directory.appending(path: "\(babyID.uuidString.lowercased()).json")
            if files.fileExists(atPath: file.path) { try files.removeItem(at: file) }
        }
    }

    public func removeIdentity(_ identity: AskIdentity) throws {
        let directory = root.appending(path: identity.directoryName)
        if files.fileExists(atPath: directory.path) { try files.removeItem(at: directory) }
    }

    public func promote(localID: UUID, accountID: UUID, babyID: UUID) throws {
        let source = AskIdentity.local(localID)
        let destination = AskIdentity.account(accountID)
        let conversation = try loadConversation(identity: source, babyID: babyID)
        guard !conversation.turns.isEmpty else { return }
        let existing = try loadConversation(identity: destination, babyID: babyID)
        if existing == conversation {
            // Save succeeded before an interrupted source deletion. Finish it
            // on retry without duplicating or exposing the local transcript.
            try clear(identity: source, babyID: babyID)
            return
        }
        guard existing.turns.isEmpty else { throw AskConversationError.destinationNotEmpty }
        try save(conversation, identity: destination, babyID: babyID)
        try clear(identity: source, babyID: babyID)
    }

    private func markPrivate(_ url: URL) throws {
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        var target = url
        try target.setResourceValues(values)
        #if os(iOS)
        try files.setAttributes(
            [.protectionKey: FileProtectionType.completeUntilFirstUserAuthentication],
            ofItemAtPath: url.path
        )
        #endif
    }
}

public enum AskConversationError: Error {
    case unsupportedVersion
    case destinationNotEmpty
}
