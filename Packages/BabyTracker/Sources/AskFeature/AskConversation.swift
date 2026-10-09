import Foundation

public enum AskIdentity: Hashable, Codable, Sendable {
    case local(UUID)
    case account(UUID)

    var directoryName: String {
        switch self {
        case .local(let id): "local-\(id.uuidString.lowercased())"
        case .account(let id): "account-\(id.uuidString.lowercased())"
        }
    }
}

public enum AskSource: Hashable, Codable, Sendable {
    case entry(kind: String, id: UUID)
    case runningTimer(kind: String, id: UUID)
    /// Kept for conversations saved before source ranges were persisted.
    case insight(key: String)
    case insightRange(key: String, days: Int)
}

public enum AskAnswerKind: String, Codable, Sendable, Equatable {
    case recorded
    case general
}

public struct AskWindow: Codable, Equatable, Sendable {
    public let start: Date
    public let end: Date

    public init(start: Date, end: Date) {
        self.start = start
        self.end = end
    }
}

public struct AskTurn: Identifiable, Codable, Equatable, Sendable {
    public let id: UUID
    public let question: String
    public let answer: String?
    public let askedAt: Date
    public let answeredAt: Date?
    public let sources: [AskSource]
    public let answerKind: AskAnswerKind?
    /// The span the tools read when the answer is too broad for entry links.
    public let queriedRange: AskWindow?

    public init(id: UUID, question: String, answer: String?, askedAt: Date,
                answeredAt: Date?, sources: [AskSource], answerKind: AskAnswerKind? = nil,
                queriedRange: AskWindow? = nil) {
        self.id = id
        self.question = question
        self.answer = answer
        self.askedAt = askedAt
        self.answeredAt = answeredAt
        self.sources = sources
        self.answerKind = answerKind
        self.queriedRange = queriedRange
    }
}

/// One child's chat: the full display history, plus a rolling summary of
/// its older part so the model can follow a conversation for months.
public struct AskConversation: Equatable, Sendable {
    public var turns: [AskTurn]
    /// Durable notes folded from older turns; nil until the first compaction.
    public var memory: String?
    /// The last turn the memory covers. Later turns reach the model verbatim.
    public var memoryCoversThrough: UUID?

    public init(turns: [AskTurn] = [], memory: String? = nil, memoryCoversThrough: UUID? = nil) {
        self.turns = turns
        self.memory = memory
        self.memoryCoversThrough = memoryCoversThrough
    }

    public var verbatimTurns: [AskTurn] {
        guard let through = memoryCoversThrough,
              let index = turns.firstIndex(where: { $0.id == through }) else { return turns }
        return Array(turns[turns.index(after: index)...])
    }
}
