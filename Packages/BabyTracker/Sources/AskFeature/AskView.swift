import Foundation
import SwiftUI
import BabyCore
import BabyUI

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
    private let babyAvatar: BabyAvatar
    private let onSwitchBaby: (() -> Void)?
    private let birthDate: Date
    private let volumeUnit: VolumeUnit
    private let onOpenSource: (AskSource) -> Void
    @State private var confirmingNewChat = false
    @State private var loadError: String?
    @State private var responseTask: Task<Void, Never>?
    @FocusState private var questionFocused: Bool

    public init(model: AskViewModel, babyName: String, babyAvatar: BabyAvatar, birthDate: Date,
                volumeUnit: VolumeUnit, onSwitchBaby: (() -> Void)? = nil,
                onOpenSource: @escaping (AskSource) -> Void) {
        _model = State(wrappedValue: model)
        self.babyName = babyName
        self.babyAvatar = babyAvatar
        self.onSwitchBaby = onSwitchBaby
        self.birthDate = birthDate
        self.volumeUnit = volumeUnit
        self.onOpenSource = onOpenSource
    }

    public var body: some View {
        NavigationStack {
            ScreenScaffold(
                header: .baby(name: babyName, avatar: babyAvatar, switcherID: "ask.babySwitcher",
                              headingID: "ask.babyName", onSwitch: onSwitchBaby),
                scrolls: false,
                trailing: { newChatButton },
                content: { conversation }
            )
            .navigationTitle(AskCopy.title)
            #if os(iOS)
            // The header is drawn by the scaffold, so the bar would leave a
            // tall empty band above it.
            .toolbar(.hidden, for: .navigationBar)
            #endif
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
        .confirmDelete(
            isPresented: $confirmingNewChat,
            title: String(localized: "Delete this chat?", bundle: .module),
            deleteTitle: String(localized: "Delete Chat", bundle: .module),
            keepTitle: String(localized: "Cancel", bundle: .module),
            identifierPrefix: "ask",
            message: String(localized: "This removes the conversation for this child from this device.", bundle: .module)
        ) {
            responseTask?.cancel()
            do { try model.confirmNewChat() }
            catch { loadError = String(localized: "Could not delete this chat.", bundle: .module) }
        }
    }

    private var newChatButton: some View {
        CircleIconButton(
            systemImage: "square.and.pencil",
            label: AskCopy.newChat,
            identifier: "ask.newChat"
        ) { confirmingNewChat = true }
        .disabled(model.turns.isEmpty && model.pendingQuestion == nil)
    }

    private var conversation: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: Spacing.xl) {
                    if model.turns.isEmpty && model.pendingQuestion == nil {
                        emptyState
                    }
                    ForEach(Array(model.turns.enumerated()), id: \.element.id) { index, turn in
                        VStack(alignment: .leading, spacing: Spacing.l) {
                            userMessage(turn.question, id: "ask.message.user.\(index)")
                            assistantMessage(turn, index: index)
                        }
                    }
                    if let pending = model.pendingQuestion {
                        userMessage(pending, id: "ask.message.pending")
                        HStack(alignment: .top, spacing: Spacing.m) {
                            assistantIcon
                            if let streaming = model.streamingAnswer {
                                Text(AskViewModel.displayText(streaming))
                                    .textRole(.body)
                                    .frame(maxWidth: .infinity, alignment: .leading)
                                    .accessibilityIdentifier("ask.streaming")
                            } else {
                                ProgressView()
                                Text(String(localized: "Thinking…", bundle: .module))
                                    .textRole(.caption)
                            }
                        }
                        .accessibilityIdentifier("ask.loading")
                    }
                    Color.clear.frame(height: 1).id("ask.bottom")
                }
                .frame(maxWidth: 680)
                .frame(maxWidth: .infinity)
                .padding(.horizontal, Spacing.l)
                .padding(.vertical, Spacing.xl)
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
    }

    private var emptyState: some View {
        VStack(spacing: Spacing.l) {
            EmptyState(
                systemImage: "sparkles",
                title: String(localized: "What's on your mind?", bundle: .module),
                message: String(format: String(localized: "Ask about %@’s care, or ask a general parenting question.", bundle: .module), babyName)
            )
            VStack(alignment: .leading, spacing: Spacing.m) {
                suggestion(AskCopy.handoff, id: "ask.suggestion.handoff")
                suggestion(AskCopy.overnight, id: "ask.suggestion.overnight")
                suggestion(AskCopy.insights, id: "ask.suggestion.insights")
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.top, Spacing.xl)
    }

    private func suggestion(_ text: String, id: String) -> some View {
        Button {
            responseTask = Task { await model.submit(text) }
        } label: {
            HStack(spacing: Spacing.m) {
                Text(text)
                    .textRole(.body)
                    .multilineTextAlignment(.leading)
                Spacer(minLength: Spacing.s)
                Image(systemName: "arrow.up.left")
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(Palette.secondaryText)
            }
            .padding(.horizontal, Spacing.l)
            .padding(.vertical, Spacing.m)
            .background(Surface.card, in: RoundedRectangle(cornerRadius: Radius.card))
        }
        .buttonStyle(.plain)
        .accessibilityIdentifier(id)
    }

    private func userMessage(_ question: String, id: String) -> some View {
        HStack {
            Spacer(minLength: 44)
            Text(question)
                .textRole(.body)
                .padding(.horizontal, Spacing.l)
                .padding(.vertical, Spacing.m)
                .background(Surface.tint(Palette.sleep, .soft), in: RoundedRectangle(cornerRadius: Radius.card))
                .accessibilityIdentifier(id)
        }
    }

    private var assistantIcon: some View {
        Image(systemName: "sparkles")
            .font(.caption.weight(.semibold))
            .foregroundStyle(Palette.feed)
            .frame(width: 28, height: 28)
            .background(Surface.card, in: Circle())
            .accessibilityHidden(true)
    }

    private func assistantMessage(_ turn: AskTurn, index: Int) -> some View {
        HStack(alignment: .top, spacing: Spacing.m) {
            assistantIcon
            VStack(alignment: .leading, spacing: Spacing.m) {
                if turn.answerKind == .general {
                    Text(String(localized: "General information · not from the log", bundle: .module))
                        .textRole(.caption)
                }
                Text(AskViewModel.displayText(turn.answer ?? ""))
                    .textRole(.body)
                    .textSelection(.enabled)
                    .accessibilityIdentifier("ask.answer.\(index)")
                let links = AskViewModel.linkedSources(turn.sources)
                if !links.isEmpty {
                    ScrollView(.horizontal) {
                        HStack(spacing: Spacing.s) {
                            ForEach(Array(links.enumerated()), id: \.offset) { sourceIndex, source in
                                Button {
                                    onOpenSource(source)
                                } label: {
                                    Chip(model.sourceLabel(source), systemImage: "arrow.up.right", size: .small)
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
                        .textRole(.caption)
                        .accessibilityIdentifier("ask.range.\(index)")
                }
                if let answered = turn.answeredAt {
                    Text(answered.formatted(date: .abbreviated, time: .shortened))
                        .textRole(.caption)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private var composer: some View {
        VStack(spacing: Spacing.s) {
            if let error = model.errorMessage ?? loadError {
                HStack {
                    InlineError(error)
                        .accessibilityIdentifier("ask.error")
                    Spacer()
                    if !model.draft.isEmpty {
                        Button(String(localized: "Retry", bundle: .module), action: send)
                    }
                }
                .padding(.horizontal, Spacing.l)
            }
            HStack(alignment: .bottom, spacing: Spacing.xs) {
                TextField(String(localized: "Ask a question", bundle: .module), text: $model.draft, axis: .vertical)
                    .lineLimit(1...5)
                    .textRole(.body)
                    .focused($questionFocused)
                    .submitLabel(.send)
                    .onSubmit(send)
                    .disabled(model.isLoading)
                    .accessibilityIdentifier("ask.question")
                    .padding(.leading, Spacing.m)
                    .padding(.vertical, Spacing.m)
                CircleIconButton(
                    systemImage: "arrow.up",
                    label: String(localized: "Send", bundle: .module),
                    identifier: "ask.send",
                    isProminent: canSend,
                    action: send
                )
                .disabled(!canSend)
                .padding(.trailing, Spacing.xs)
            }
            .background(Surface.card, in: RoundedRectangle(cornerRadius: Radius.card))
            .padding(.horizontal, Spacing.l)
        }
        .padding(.vertical, Spacing.s)
        .background(Palette.ground)
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
