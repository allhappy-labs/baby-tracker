import Foundation
import SwiftUI
import BabyCore

public enum AskCopy {
    public static let title = String(localized: "Ask", bundle: .module)
    public static let newChat = String(localized: "New Chat", bundle: .module)
    public static let handoff = String(localized: "Catch me up", bundle: .module)
    public static let overnight = String(localized: "What happened overnight?", bundle: .module)
    public static let insights = String(localized: "Explain this week’s Insights", bundle: .module)
}

public struct AskView: View {
    @State private var model: AskViewModel
    private let babyName: String
    private let birthDate: Date
    private let volumeUnit: VolumeUnit
    private let onOpenSource: (AskSource) -> Void
    @State private var confirmingNewChat = false
    @State private var loadError: String?
    @State private var responseTask: Task<Void, Never>?
    @FocusState private var questionFocused: Bool

    public init(model: AskViewModel, babyName: String, birthDate: Date, volumeUnit: VolumeUnit,
                onOpenSource: @escaping (AskSource) -> Void) {
        _model = State(wrappedValue: model)
        self.babyName = babyName
        self.birthDate = birthDate
        self.volumeUnit = volumeUnit
        self.onOpenSource = onOpenSource
    }

    public var body: some View {
        NavigationStack {
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 24) {
                        if model.turns.isEmpty && model.pendingQuestion == nil {
                            emptyState
                        }
                        ForEach(Array(model.turns.enumerated()), id: \.element.id) { index, turn in
                            VStack(alignment: .leading, spacing: 16) {
                                userMessage(turn.question, id: "ask.message.user.\(index)")
                                assistantMessage(turn, index: index)
                            }
                        }
                        if let pending = model.pendingQuestion {
                            userMessage(pending, id: "ask.message.pending")
                            HStack(alignment: .top, spacing: 10) {
                                assistantIcon
                                if let streaming = model.streamingAnswer {
                                    Text(AskViewModel.displayText(streaming))
                                        .font(.body)
                                        .frame(maxWidth: .infinity, alignment: .leading)
                                        .accessibilityIdentifier("ask.streaming")
                                } else {
                                    ProgressView()
                                    Text(String(localized: "Thinking…", bundle: .module))
                                        .foregroundStyle(.secondary)
                                }
                            }
                            .accessibilityIdentifier("ask.loading")
                        }
                        Color.clear.frame(height: 1).id("ask.bottom")
                    }
                    .frame(maxWidth: 680)
                    .frame(maxWidth: .infinity)
                    .padding(.horizontal, 16)
                    .padding(.vertical, 20)
                }
                .scrollDismissesKeyboard(.interactively)
                .onChange(of: model.turns.count) { _, _ in
                    withAnimation(.easeOut(duration: 0.2)) { proxy.scrollTo("ask.bottom", anchor: .bottom) }
                }
                .onChange(of: model.streamingAnswer) { _, _ in
                    proxy.scrollTo("ask.bottom", anchor: .bottom)
                }
                .onChange(of: model.pendingQuestion) { _, pending in
                    if pending != nil {
                        withAnimation(.easeOut(duration: 0.2)) { proxy.scrollTo("ask.bottom", anchor: .bottom) }
                    }
                }
            }
            .safeAreaInset(edge: .bottom, spacing: 0) { composer }
            .navigationTitle(AskCopy.title)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                Button { confirmingNewChat = true } label: {
                    Image(systemName: "square.and.pencil")
                }
                .accessibilityLabel(AskCopy.newChat)
                .accessibilityIdentifier("ask.newChat")
                .disabled(model.turns.isEmpty && model.pendingQuestion == nil)
            }
        }
        .task {
            model.updatePresentation(babyName: babyName, birthDate: birthDate, volumeUnit: volumeUnit)
            do { try model.load() }
            catch { loadError = String(localized: "Could not load this chat.", bundle: .module) }
        }
        .onChange(of: babyName) { _, name in
            model.updatePresentation(babyName: name, birthDate: birthDate, volumeUnit: volumeUnit)
        }
        .onChange(of: birthDate) { _, date in
            model.updatePresentation(babyName: babyName, birthDate: date, volumeUnit: volumeUnit)
        }
        .onChange(of: volumeUnit) { _, unit in
            model.updatePresentation(babyName: babyName, birthDate: birthDate, volumeUnit: unit)
        }
        .onDisappear {
            responseTask?.cancel()
            model.cancelPending()
        }
        .alert(String(localized: "Delete this chat?", bundle: .module), isPresented: $confirmingNewChat) {
            Button(String(localized: "Delete Chat", bundle: .module), role: .destructive) {
                responseTask?.cancel()
                do { try model.confirmNewChat() }
                catch { loadError = String(localized: "Could not delete this chat.", bundle: .module) }
            }
            .accessibilityIdentifier("ask.confirmNewChat")
            Button(String(localized: "Cancel", bundle: .module), role: .cancel) {}
                .accessibilityIdentifier("ask.cancelNewChat")
        } message: {
            Text(String(localized: "This removes the conversation for this child from this device.", bundle: .module))
        }
    }

    private var emptyState: some View {
        VStack(alignment: .leading, spacing: 18) {
            Image(systemName: "sparkles")
                .font(.title)
                .foregroundStyle(.tint)
                .accessibilityHidden(true)
            Text(String(localized: "What's on your mind?", bundle: .module))
                .font(.title2.weight(.semibold))
            Text(String(format: String(localized: "Ask about %@’s care, or ask a general parenting question.", bundle: .module), babyName))
                .foregroundStyle(.secondary)
            VStack(alignment: .leading, spacing: 10) {
                suggestion(AskCopy.handoff, id: "ask.suggestion.handoff")
                suggestion(AskCopy.overnight, id: "ask.suggestion.overnight")
                suggestion(AskCopy.insights, id: "ask.suggestion.insights")
            }
            .padding(.top, 8)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.top, 36)
    }

    private func suggestion(_ text: String, id: String) -> some View {
        Button {
            responseTask = Task { await model.submit(text) }
        } label: {
            HStack(spacing: 12) {
                Text(text)
                    .multilineTextAlignment(.leading)
                Spacer(minLength: 8)
                Image(systemName: "arrow.up.left")
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(.secondary)
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 13)
            .background(.thinMaterial, in: RoundedRectangle(cornerRadius: 16))
        }
        .buttonStyle(.plain)
        .accessibilityIdentifier(id)
    }

    private func userMessage(_ question: String, id: String) -> some View {
        HStack {
            Spacer(minLength: 44)
            Text(question)
                .font(.body)
                .padding(.horizontal, 16)
                .padding(.vertical, 12)
                .background(Color.accentColor.opacity(0.22), in: RoundedRectangle(cornerRadius: 20))
                .accessibilityIdentifier(id)
        }
    }

    private var assistantIcon: some View {
        Image(systemName: "sparkles")
            .font(.caption.weight(.semibold))
            .foregroundStyle(.tint)
            .frame(width: 28, height: 28)
            .background(.thinMaterial, in: Circle())
            .accessibilityHidden(true)
    }

    private func assistantMessage(_ turn: AskTurn, index: Int) -> some View {
        HStack(alignment: .top, spacing: 10) {
            assistantIcon
            VStack(alignment: .leading, spacing: 10) {
                if turn.answerKind == .general {
                    Text(String(localized: "General information · not from the log", bundle: .module))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                Text(AskViewModel.displayText(turn.answer ?? ""))
                    .font(.body)
                    .textSelection(.enabled)
                    .accessibilityIdentifier("ask.answer.\(index)")
                let links = AskViewModel.linkedSources(turn.sources)
                if !links.isEmpty {
                    ScrollView(.horizontal) {
                        HStack(spacing: 8) {
                            ForEach(Array(links.enumerated()), id: \.offset) { sourceIndex, source in
                                Button {
                                    onOpenSource(source)
                                } label: {
                                    Label(model.sourceLabel(source), systemImage: "arrow.up.right")
                                        .font(.caption.weight(.medium))
                                        .padding(.horizontal, 10)
                                        .padding(.vertical, 7)
                                        .background(.thinMaterial, in: Capsule())
                                }
                                .buttonStyle(.plain)
                                .accessibilityIdentifier("ask.source.\(sourceIndex)")
                            }
                        }
                    }
                    .scrollIndicators(.hidden)
                }
                if links.isEmpty, let range = turn.queriedRange {
                    Text(model.rangeCaption(range))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .accessibilityIdentifier("ask.range.\(index)")
                }
                if let answered = turn.answeredAt {
                    Text(answered.formatted(date: .abbreviated, time: .shortened))
                        .font(.caption2)
                        .foregroundStyle(.tertiary)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private var composer: some View {
        VStack(spacing: 7) {
            if let error = model.errorMessage ?? loadError {
                HStack {
                    Text(error)
                        .font(.caption)
                        .foregroundStyle(.red)
                        .accessibilityIdentifier("ask.error")
                    Spacer()
                    if !model.draft.isEmpty {
                        Button(String(localized: "Retry", bundle: .module), action: send)
                    }
                }
                .padding(.horizontal, 16)
            }
            HStack(alignment: .bottom, spacing: 6) {
                TextField(String(localized: "Ask a question", bundle: .module), text: $model.draft, axis: .vertical)
                    .lineLimit(1...5)
                    .font(.body)
                    .focused($questionFocused)
                    .submitLabel(.send)
                    .onSubmit(send)
                    .disabled(model.isLoading)
                    .accessibilityIdentifier("ask.question")
                    .padding(.leading, 12)
                    .padding(.vertical, 10)
                Button(action: send) {
                    Image(systemName: "arrow.up")
                        .font(.body.weight(.bold))
                        .foregroundStyle(canSend ? .white : .secondary)
                        .frame(width: 38, height: 38)
                        .background(canSend ? Color.accentColor : Color.secondary.opacity(0.2), in: Circle())
                }
                .buttonStyle(.plain)
                .frame(width: 44, height: 44)
                .accessibilityLabel(String(localized: "Send", bundle: .module))
                .accessibilityIdentifier("ask.send")
                .disabled(!canSend)
            }
            .background(.thinMaterial, in: RoundedRectangle(cornerRadius: 22))
            .padding(.horizontal, 12)
        }
        .padding(.top, 8)
        .padding(.bottom, 8)
        .background(.bar)
    }

    private var canSend: Bool {
        !model.isLoading && !model.draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    private func send() {
        guard canSend else { return }
        let question = model.draft
        questionFocused = false
        responseTask = Task { await model.submit(question) }
    }
}
