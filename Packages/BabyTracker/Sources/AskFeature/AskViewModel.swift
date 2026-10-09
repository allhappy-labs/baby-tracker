import Foundation
import Observation
import os
import BabyCore
import BabyStore

@MainActor
@Observable
public final class AskViewModel {
    private let store: BabyStore
    private let conversationStore: AskConversationStore
    private let model: any AskModel
    private let identity: AskIdentity
    public let babyID: UUID
    public private(set) var babyName: String
    private var birthDate: Date
    private var volumeUnit: VolumeUnit
    private var generation = 0
    private var conversation = AskConversation()

    public var turns: [AskTurn] { conversation.turns }
    public var draft = ""
    public private(set) var pendingQuestion: String?
    /// The answer as it streams in; nil until the first words arrive.
    public private(set) var streamingAnswer: String?
    public private(set) var availability: AskAvailability = .available
    public private(set) var isLoading = false
    public private(set) var errorMessage: String?

    public init(store: BabyStore, conversationStore: AskConversationStore, model: any AskModel,
                identity: AskIdentity, babyID: UUID, babyName: String, birthDate: Date, volumeUnit: VolumeUnit) {
        self.store = store
        self.conversationStore = conversationStore
        self.model = model
        self.identity = identity
        self.babyID = babyID
        self.babyName = babyName
        self.birthDate = birthDate
        self.volumeUnit = volumeUnit
        self.availability = model.availability(for: .current)
    }

    public func load() throws {
        conversation = try conversationStore.loadConversation(identity: identity, babyID: babyID)
        refreshAvailability()
    }

    public func refreshAvailability() {
        availability = model.availability(for: .current)
    }

    public func updatePresentation(babyName: String, birthDate: Date, volumeUnit: VolumeUnit) {
        self.babyName = babyName
        self.birthDate = birthDate
        self.volumeUnit = volumeUnit
    }

    /// The sources a turn shows as links. The current Ask only cites log
    /// entries; `.insight` / `.insightRange` survive in chats saved by the
    /// earlier Ask and are decoded but not shown — Insights has no section
    /// to open any more (spec amendment 2026-10-06).
    public nonisolated static func linkedSources(_ sources: [AskSource]) -> [AskSource] {
        sources.filter {
            switch $0 {
            case .entry, .runningTimer: true
            case .insight, .insightRange: false
            }
        }
    }

    public func sourceLabel(_ source: AskSource) -> String {
        switch source {
        case .runningTimer(let kind, let id):
            guard let timer = try? store.activeTimer(id: id), timer.babyID == babyID else {
                let title = kind == "sleep" ? String(localized: "Sleep", bundle: .module)
                                            : String(localized: "Feed", bundle: .module)
                return "\(title) · \(String(localized: "unavailable", bundle: .module))"
            }
            return timer.sideLabel
        case .entry(let kind, let id):
            let title: String = switch kind {
            case "feed": String(localized: "Feed", bundle: .module)
            case "sleep": String(localized: "Sleep", bundle: .module)
            case "diaper": String(localized: "Diaper", bundle: .module)
            case "supplement": String(localized: "Med", bundle: .module)
            default: String(localized: "Entry", bundle: .module)
            }
            guard let item = try? store.entry(id: id, for: babyID) else {
                return "\(title) · \(String(localized: "unavailable", bundle: .module))"
            }
            let formatter = DateFormatter()
            formatter.locale = .current
            formatter.timeStyle = .short
            return "\(title) · \(formatter.string(from: item.startedAt))"
        case .insight, .insightRange: return ""  // never shown: see linkedSources
        }
    }

    /// Answer text with inline Markdown (such as **bold**) rendered: the
    /// on-device model adds it despite the instructions. Text that doesn't
    /// parse is shown as written.
    public nonisolated static func displayText(_ answer: String) -> AttributedString {
        let options = AttributedString.MarkdownParsingOptions(interpretedSyntax: .inlineOnlyPreservingWhitespace)
        return (try? AttributedString(markdown: answer, options: options)) ?? AttributedString(answer)
    }

    public func rangeCaption(_ range: AskWindow) -> String {
        let style = Date.FormatStyle.dateTime.day().month(.abbreviated)
        let start = range.start.formatted(style)
        // The end is exclusive: a range ending at midnight ends the day before.
        let lastMoment = Calendar.current.startOfDay(for: range.end) == range.end
            ? range.end.addingTimeInterval(-1) : range.end
        let end = lastMoment.formatted(style)
        let span = start == end ? start : "\(start) – \(end)"
        return String(format: String(localized: "From the log · %@", bundle: .module), span)
    }

    public func submit(_ raw: String) async {
        let question = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !question.isEmpty, !isLoading else { return }
        refreshAvailability()
        guard case .available = availability else {
            draft = question
            return
        }
        draft = ""
        pendingQuestion = question
        streamingAnswer = nil
        isLoading = true
        errorMessage = nil
        let requestGeneration = generation
        let askedAt = Date()
        var answered = false
        do {
            let profile = AskProfile(babyID: babyID, name: babyName, birthDate: birthDate)
            let reply = try await model.answer(question: question, conversation: conversation, profile: profile,
                                               volumeUnit: volumeUnit, store: store) { [weak self] partial in
                guard let self, requestGeneration == self.generation else { return }
                self.streamingAnswer = partial.isEmpty ? nil : partial
            }
            guard requestGeneration == generation, !Task.isCancelled else { return }
            let turn = AskTurn(id: UUID(), question: question, answer: reply.text, askedAt: askedAt,
                               answeredAt: Date(), sources: reply.sources,
                               answerKind: reply.usedTools ? .recorded : .general,
                               queriedRange: reply.queriedRange)
            var updated = conversation
            updated.turns.append(turn)
            try conversationStore.save(updated, identity: identity, babyID: babyID)
            conversation = updated
            pendingQuestion = nil
            streamingAnswer = nil
            answered = true
        } catch {
            guard requestGeneration == generation else { return }
            let failure = error as NSError
            AppLog.ui.error("Ask failed: \(failure.domain, privacy: .public) code \(failure.code, privacy: .public)")
            draft = question
            pendingQuestion = nil
            streamingAnswer = nil
            errorMessage = Self.message(for: error)
        }
        if requestGeneration == generation { isLoading = false }
        if answered { await compactIfNeeded(generation: requestGeneration) }
    }

    public func confirmNewChat() throws {
        generation += 1
        try conversationStore.clear(identity: identity, babyID: babyID)
        conversation = AskConversation()
        draft = ""
        pendingQuestion = nil
        streamingAnswer = nil
        errorMessage = nil
        isLoading = false
    }

    public func cancelPending() {
        generation += 1
        if let pendingQuestion, draft.isEmpty { draft = pendingQuestion }
        pendingQuestion = nil
        streamingAnswer = nil
        isLoading = false
    }

    /// Folds older turns into memory after an answer. A failure only means
    /// the next request leaves out the oldest turns; the chat is unchanged.
    private func compactIfNeeded(generation requestGeneration: Int) async {
        guard let compacted = try? await model.compacted(conversation),
              requestGeneration == generation,
              let through = compacted.memoryCoversThrough,
              conversation.turns.contains(where: { $0.id == through }) else { return }
        var updated = conversation
        updated.memory = compacted.memory
        updated.memoryCoversThrough = through
        do {
            try conversationStore.save(updated, identity: identity, babyID: babyID)
            conversation = updated
        } catch {
            AppLog.ui.error("Ask memory could not be saved")
        }
    }

    private static func message(for error: any Error) -> String {
        switch error as? AskFailure {
        case .blocked: String(localized: "Ask can't help with that one.", bundle: .module)
        case .unsupportedLanguage: String(localized: "This language isn't supported.", bundle: .module)
        case .timedOut: String(localized: "That took too long. Try again.", bundle: .module)
        case .failed, nil: String(localized: "Could not answer. Try again.", bundle: .module)
        }
    }
}
