import Foundation
import FoundationModels
import BabyCore
import BabyStore

public enum AskAvailability: Equatable, Sendable {
    case available
    case unavailable(String)
}

/// One finished answer and what it read.
public struct AskReply: Equatable, Sendable {
    public let text: String
    public let sources: [AskSource]
    public let queriedRange: AskWindow?
    public let toolNames: [String]

    public var usedTools: Bool { !toolNames.isEmpty }

    public init(text: String, sources: [AskSource] = [], queriedRange: AskWindow? = nil, toolNames: [String] = []) {
        self.text = text
        self.sources = sources
        self.queriedRange = queriedRange
        self.toolNames = toolNames
    }
}

@MainActor
public protocol AskModel {
    func availability(for locale: Locale) -> AskAvailability
    /// Answers `question` in the context of `conversation`. `onPartial`
    /// receives the growing answer while it streams; an empty string means
    /// the answer restarted (for example, a retry with fewer earlier turns).
    func answer(question: String, conversation: AskConversation, profile: AskProfile, volumeUnit: VolumeUnit,
                store: BabyStore, onPartial: @escaping @MainActor (String) -> Void) async throws -> AskReply
    /// `conversation` with its older turns folded into memory, or nil when nothing needs folding.
    func compacted(_ conversation: AskConversation) async throws -> AskConversation?
}

/// The on-device Apple Foundation Model. A planning call picks the log reads,
/// the app runs them, and a second call answers in plain text over the result.
@MainActor
public final class AppleAskModel: AskModel {
    private let model = SystemLanguageModel.default
    /// Answers are plain text over the parent's own log. Default guardrails
    /// reject ordinary breastfeeding wording ("eats breasts") as unsafe.
    private let answerModel = SystemLanguageModel(guardrails: .permissiveContentTransformations)

    public init() {}

    public func availability(for locale: Locale) -> AskAvailability {
        guard model.isAvailable else {
            return .unavailable(String(localized: "Apple Intelligence is unavailable on this device.", bundle: .module))
        }
        guard model.supportsLocale(locale) else {
            return .unavailable(String(localized: "This language is not supported by the on-device model.", bundle: .module))
        }
        return .available
    }

    public func answer(question: String, conversation: AskConversation, profile: AskProfile, volumeUnit: VolumeUnit,
                       store: BabyStore, onPartial: @escaping @MainActor (String) -> Void) async throws -> AskReply {
        let now = store.now
        let text = AskText(timeZone: .current)
        let running = try store.activeTimers(for: profile.babyID).filter { $0.startedAt <= now }
        let timers = AskSessionBuilder.liveTimers(running, volumeUnit: volumeUnit)
        let pump = try store.runningPump().map { AskLivePump(startedAt: $0.startedAt, side: $0.side) }
        var history = AskSessionBuilder.history(
            conversation, budget: historyBudget - AskSessionBuilder.estimatedTokens(conversation.memory ?? ""),
            tokens: AskSessionBuilder.estimatedTokens)
        let log = AskCareLog(store: store, babyID: profile.babyID, now: now, timeZone: .current, volumeUnit: volumeUnit)
        do {
            // The model picks the reads with guided generation; the app runs
            // them, so an answer about the log always has the log in front of it.
            let planner = LanguageModelSession(model: model, instructions: AskSessionBuilder.planInstructions(
                profile: profile, now: now, timers: timers, text: text, pump: pump))
            let reads: [AskRead]
            do {
                reads = try await planner.respond(to: AskSessionBuilder.planPrompt(question: question, history: history),
                                                  generating: AskPlan.self, options: Self.greedy).content.uniqueReads
            } catch where !(error is CancellationError) && !Task.isCancelled {
                // A blocked or failed plan still gets an answer; the answer
                // instructions say no log data was read.
                AppLog.ui.error("Ask plan failed: \(AskErrorKind(error).rawValue, privacy: .public)")
                reads = []
            }
            let logData = reads.prefix(3).map(log.run)
            let instructions = AskSessionBuilder.instructions(profile: profile, now: now, timers: timers, text: text,
                                                              memory: conversation.memory, logData: Array(logData), pump: pump)
            let prompt = AskSessionBuilder.prompt(question: question)
            var retriedOverflow = false
            while true {
                do {
                    onPartial("")
                    let session = LanguageModelSession(model: answerModel, transcript: Self.transcript(
                        instructions: instructions, history: history))
                    var streamed = ""
                    for try await snapshot in session.streamResponse(to: prompt) {
                        streamed = snapshot.content
                        onPartial(streamed)
                    }
                    let answer = streamed.trimmingCharacters(in: .whitespacesAndNewlines)
                    guard !answer.isEmpty else { throw AskFailure.failed }
                    AppLog.ui.notice("Ask answered using \(log.record.toolNames.joined(separator: ","), privacy: .public)")
                    return AskReply(text: answer, sources: log.record.sources,
                                    queriedRange: log.record.queriedRange, toolNames: log.record.toolNames)
                } catch where AskErrorKind(error) == .contextOverflow && !retriedOverflow && !history.isEmpty {
                    // The question and its log data matter more than earlier turns.
                    retriedOverflow = true
                    history = []
                }
            }
        } catch {
            if Task.isCancelled || error is CancellationError { throw error }
            if let failure = error as? AskFailure { throw failure }
            let kind = AskErrorKind(error)
            AppLog.ui.error("Ask failed: \(kind.rawValue, privacy: .public)")
            throw kind.failure
        }
    }

    /// The same question over the same log gets the same reads.
    private static let greedy = GenerationOptions(samplingMode: .greedy)

    public func compacted(_ conversation: AskConversation) async throws -> AskConversation? {
        let fold = AskCompaction.turnsToFold(conversation)
        guard let last = fold.last else { return nil }
        let session = LanguageModelSession(model: model,
                                           instructions: AskCompaction.summaryInstructions(wordLimit: 120))
        let prompt = AskCompaction.summaryPrompt(memory: conversation.memory, folding: fold,
                                                 text: AskText(timeZone: .current))
        let memory = try await session.respond(to: prompt).content.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !memory.isEmpty, memory.count <= AskCompaction.maxMemoryCharacters else { return nil }
        var updated = conversation
        updated.memory = memory
        updated.memoryCoversThrough = last.id
        return updated
    }

    static func transcript(instructions: String, history: [AskTurn]) -> Transcript {
        var entries: [Transcript.Entry] = [
            .instructions(Transcript.Instructions(
                segments: [.text(Transcript.TextSegment(content: instructions))], toolDefinitions: [])),
        ]
        for turn in history {
            guard let answer = turn.answer else { continue }
            entries.append(.prompt(Transcript.Prompt(segments: [.text(Transcript.TextSegment(content: turn.question))])))
            entries.append(.response(Transcript.Response(assetIDs: [],
                                                         segments: [.text(Transcript.TextSegment(content: answer))])))
        }
        return Transcript(entries: entries)
    }

    /// Tokens that memory plus verbatim turns may use: a share of the
    /// window, leaving room for instructions, tools, their output and the reply.
    private var historyBudget: Int {
        let contextSize: Int = if #available(iOS 26.4, macOS 26.4, *) { model.contextSize } else { 4_096 }
        return Int(Double(contextSize) * AskCompaction.historyShare)
    }
}
