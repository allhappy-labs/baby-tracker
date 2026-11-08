import Foundation
import Testing
import BabyCore
import BabyStore
@testable import AskFeature

@MainActor
private final class FakeAskModel: AskModel {
    var state: AskAvailability = .available
    var failure: (any Error)?
    var suspend = false
    var answerStarted: CheckedContinuation<Void, Never>?
    var answerRelease: CheckedContinuation<Void, Never>?
    var calls = 0
    var reply = AskReply(text: "The log has one wet diaper.", toolNames: ["careLog"])
    var partials = ["The log", "The log has"]
    var seenConversation: AskConversation?
    var seenProfile: AskProfile?
    var seenUnit: VolumeUnit?
    var compactResult: (@MainActor (AskConversation) -> AskConversation?)?

    func availability(for locale: Locale) -> AskAvailability { state }

    func answer(question: String, conversation: AskConversation, profile: AskProfile, volumeUnit: VolumeUnit,
                store: BabyStore, onPartial: @escaping @MainActor (String) -> Void) async throws -> AskReply {
        calls += 1
        seenConversation = conversation
        seenProfile = profile
        seenUnit = volumeUnit
        for partial in partials { onPartial(partial) }
        if suspend {
            answerStarted?.resume()
            answerStarted = nil
            await withCheckedContinuation { answerRelease = $0 }
        }
        if let failure { throw failure }
        return reply
    }

    func compacted(_ conversation: AskConversation) async throws -> AskConversation? {
        compactResult?(conversation)
    }

    enum FakeError: Error { case failed }
}

private let birth = Date(timeIntervalSince1970: 1_790_000_000)

@MainActor
private func setup() throws -> (BabyStore, UUID, AskConversationStore, URL, FakeAskModel) {
    let now = Date(timeIntervalSince1970: 1_800_000_000)
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(now))
    let baby = try store.addBaby(name: "Mira", birthDate: birth, referenceSex: .girls, expectedDueDate: nil)
    _ = try store.logDiaper(.wet, for: baby.id, at: now.addingTimeInterval(-1))
    let root = FileManager.default.temporaryDirectory.appending(path: "ask-view-tests-\(UUID().uuidString)")
    return (store, baby.id, AskConversationStore(root: root), root, FakeAskModel())
}

@MainActor
private func makeModel(_ store: BabyStore, _ baby: UUID, _ chats: AskConversationStore, _ fake: FakeAskModel,
                       identity: AskIdentity = .local(UUID())) -> AskViewModel {
    AskViewModel(store: store, conversationStore: chats, model: fake, identity: identity, babyID: baby,
                 babyName: "Mira", birthDate: birth, volumeUnit: .millilitres)
}

@MainActor
@Test func completedAnswerPersistsAcrossViewModelRecreation() async throws {
    let (store, baby, chats, root, fake) = try setup()
    defer { try? FileManager.default.removeItem(at: root) }
    let identity = AskIdentity.local(UUID())
    let first = makeModel(store, baby, chats, fake, identity: identity)
    try first.load()
    await first.submit("Catch me up")
    let reopened = makeModel(store, baby, chats, fake, identity: identity)
    try reopened.load()
    #expect(reopened.turns.map(\.answer) == ["The log has one wet diaper."])
    #expect(reopened.turns[0].answerKind == .recorded)
}

@MainActor
@Test func updatedPresentationReachesTheNextAnswer() async throws {
    let (store, baby, chats, root, fake) = try setup()
    defer { try? FileManager.default.removeItem(at: root) }
    let model = makeModel(store, baby, chats, fake)
    let corrected = Date(timeIntervalSince1970: 1_791_000_000)
    model.updatePresentation(babyName: "Nina", birthDate: corrected, volumeUnit: .ounces)
    await model.submit("How much did she drink?")
    #expect(fake.seenProfile == AskProfile(babyID: baby, name: "Nina", birthDate: corrected))
    #expect(fake.seenUnit == .ounces)
}

@MainActor
@Test func failedQuestionStaysDraftAndRetryCreatesOneTurn() async throws {
    let (store, baby, chats, root, fake) = try setup()
    defer { try? FileManager.default.removeItem(at: root) }
    let model = makeModel(store, baby, chats, fake)
    fake.failure = FakeAskModel.FakeError.failed
    await model.submit("Catch me up")
    #expect(model.turns.isEmpty)
    #expect(model.draft == "Catch me up")
    #expect(model.errorMessage == "Could not answer. Try again.")
    fake.failure = nil
    await model.submit(model.draft)
    #expect(model.turns.count == 1)
    #expect(fake.calls == 2)
}

@MainActor
@Test func failureKindsShowSpecificMessages() async throws {
    let (store, baby, chats, root, fake) = try setup()
    defer { try? FileManager.default.removeItem(at: root) }
    let model = makeModel(store, baby, chats, fake)
    let cases: [(AskFailure, String)] = [
        (.blocked, "Ask can't help with that one."),
        (.unsupportedLanguage, "This language isn't supported."),
        (.timedOut, "That took too long. Try again."),
        (.failed, "Could not answer. Try again."),
    ]
    for (failure, message) in cases {
        fake.failure = failure
        await model.submit("Question")
        #expect(model.errorMessage == message)
        #expect(model.draft == "Question")
    }
    #expect(model.turns.isEmpty)
}

@MainActor
@Test func unavailableModelKeepsSavedChatReadable() async throws {
    let (store, baby, chats, root, fake) = try setup()
    defer { try? FileManager.default.removeItem(at: root) }
    let identity = AskIdentity.local(UUID())
    let model = makeModel(store, baby, chats, fake, identity: identity)
    await model.submit("Catch me up")
    fake.state = .unavailable("Not ready")
    let reopened = makeModel(store, baby, chats, fake, identity: identity)
    try reopened.load()
    await reopened.submit("What changed?")
    #expect(reopened.turns.count == 1)
    #expect(reopened.draft == "What changed?")
}

@MainActor
@Test func newChatCannotBeRecreatedByLateModelResponse() async throws {
    let (store, baby, chats, root, fake) = try setup()
    defer { try? FileManager.default.removeItem(at: root) }
    let identity = AskIdentity.local(UUID())
    let model = makeModel(store, baby, chats, fake, identity: identity)
    fake.suspend = true
    var request: Task<Void, Never>!
    await withCheckedContinuation { started in
        fake.answerStarted = started
        request = Task { await model.submit("Catch me up") }
    }
    try model.confirmNewChat()
    fake.answerRelease?.resume()
    await request.value
    #expect(model.turns.isEmpty)
    #expect(try chats.loadConversation(identity: identity, babyID: baby) == AskConversation())
}

@MainActor
@Test func leavingAskDuringSuggestionKeepsQuestionAsDraft() async throws {
    let (store, baby, chats, root, fake) = try setup()
    defer { try? FileManager.default.removeItem(at: root) }
    let model = makeModel(store, baby, chats, fake)
    fake.suspend = true
    var request: Task<Void, Never>!
    await withCheckedContinuation { started in
        fake.answerStarted = started
        request = Task { await model.submit("Catch me up") }
    }
    model.cancelPending()
    fake.answerRelease?.resume()
    await request.value
    #expect(model.turns.isEmpty)
    #expect(model.draft == "Catch me up")
}

@MainActor
@Test func followUpReceivesTheWholeConversation() async throws {
    let (store, baby, chats, root, fake) = try setup()
    defer { try? FileManager.default.removeItem(at: root) }
    let model = makeModel(store, baby, chats, fake)
    await model.submit("Is he eating better?")
    await model.submit("What about yesterday?")
    #expect(model.turns.count == 2)
    #expect(fake.seenConversation?.turns.map(\.question) == ["Is he eating better?"])
}

@MainActor
@Test func streamingTextShowsWhileGeneratingAndFailureRestoresDraft() async throws {
    let (store, baby, chats, root, fake) = try setup()
    defer { try? FileManager.default.removeItem(at: root) }
    let model = makeModel(store, baby, chats, fake)
    model.draft = "Is he eating better?"
    // Review focus 3: an attempt streams, restarts after a retry, and streams again.
    fake.partials = ["First words", "", "Retry"]
    fake.suspend = true
    fake.failure = FakeAskModel.FakeError.failed
    var request: Task<Void, Never>!
    await withCheckedContinuation { started in
        fake.answerStarted = started
        request = Task { await model.submit(model.draft) }
    }
    #expect(model.pendingQuestion == "Is he eating better?")
    #expect(model.draft.isEmpty)
    #expect(model.streamingAnswer == "Retry")
    fake.answerRelease?.resume()
    await request.value
    #expect(model.pendingQuestion == nil)
    #expect(model.streamingAnswer == nil)
    #expect(model.draft == "Is he eating better?")
    #expect(model.turns.isEmpty)
}

@MainActor
@Test func sourceLabelsNameTheRecordAndItsTime() throws {
    let (store, baby, chats, root, fake) = try setup()
    defer { try? FileManager.default.removeItem(at: root) }
    let diaper = try store.logDiaper(.wet, for: baby, at: store.now.addingTimeInterval(-120))
    let model = makeModel(store, baby, chats, fake)
    let source = AskSource.entry(kind: "diaper", id: diaper.id)
    #expect(model.sourceLabel(source).contains("Diaper ·"))
    try store.softDelete(diaper)
    #expect(model.sourceLabel(source).contains("unavailable"))
}

/// Vaccinations are months apart: a source names the day, not a clock time.
@MainActor
@Test func aVaccineSourceNamesItsDay() throws {
    let (store, baby, chats, root, fake) = try setup()
    defer { try? FileManager.default.removeItem(at: root) }
    let mira = try #require(try store.baby(baby))
    try store.updateBabyAndGrowthProfile(mira, name: mira.name, birthDate: store.now.addingTimeInterval(-200 * 86_400),
                                         referenceSex: .girls, expectedDueDate: nil)
    let given = store.now.addingTimeInterval(-40 * 86_400)
    let shot = try store.logImmunization(vaccine: "MMR", dose: 1, for: baby, at: given)
    let model = makeModel(store, baby, chats, fake)
    let day = given.formatted(Date.FormatStyle(timeZone: TimeZone(identifier: shot.timeZoneID) ?? .current)
        .day().month(.abbreviated).year())
    #expect(model.sourceLabel(.entry(kind: "immunization", id: shot.id)) == "Vaccine · \(day)")
}

@MainActor
@Test func oldInsightsLinksInSavedChatsAreNotShown() {
    let entry = AskSource.entry(kind: "diaper", id: UUID())
    let saved: [AskSource] = [.insight(key: "feeds"), entry, .insightRange(key: "sleep", days: 7)]
    #expect(AskViewModel.linkedSources(saved) == [entry])
}

@MainActor
@Test func answersWithoutToolsAreGeneralAndBroadReadsGetARangeCaption() async throws {
    let (store, baby, chats, root, fake) = try setup()
    defer { try? FileManager.default.removeItem(at: root) }
    let model = makeModel(store, baby, chats, fake)
    fake.reply = AskReply(text: "Many babies take a small top-up after breastfeeding.")
    await model.submit("How much should I prepare?")
    #expect(model.turns[0].answerKind == .general)
    let range = AskWindow(start: Date(timeIntervalSince1970: 1_799_400_000), end: Date(timeIntervalSince1970: 1_800_000_000))
    fake.reply = AskReply(text: "Twelve feeds were logged.", queriedRange: range, toolNames: ["careTotals"])
    await model.submit("How many feeds this week?")
    #expect(model.turns[1].answerKind == .recorded)
    #expect(model.turns[1].queriedRange == range)
    #expect(model.rangeCaption(range).hasPrefix("From the log · "))
}

@MainActor
@Test func compactionSavesMemoryThatSurvivesReopening() async throws {
    let (store, baby, chats, root, fake) = try setup()
    defer { try? FileManager.default.removeItem(at: root) }
    let identity = AskIdentity.local(UUID())
    fake.compactResult = { conversation in
        var folded = conversation
        folded.memory = "4 Oct: prefers ml."
        folded.memoryCoversThrough = conversation.turns.first?.id
        return folded
    }
    let model = makeModel(store, baby, chats, fake, identity: identity)
    await model.submit("Q1")
    let reopened = makeModel(store, baby, chats, fake, identity: identity)
    try reopened.load()
    fake.compactResult = nil
    await reopened.submit("Q2")
    #expect(fake.seenConversation?.memory == "4 Oct: prefers ml.")
    #expect(fake.seenConversation?.memoryCoversThrough == reopened.turns.first?.id)
}

@MainActor
@Test func compactionAfterNewChatIsDropped() async throws {
    let (store, baby, chats, root, fake) = try setup()
    defer { try? FileManager.default.removeItem(at: root) }
    let identity = AskIdentity.local(UUID())
    let model = makeModel(store, baby, chats, fake, identity: identity)
    // Review focus 4: the chat is deleted while its memory is being written.
    fake.compactResult = { [weak model] conversation in
        try? model?.confirmNewChat()
        var folded = conversation
        folded.memory = "stale"
        folded.memoryCoversThrough = conversation.turns.first?.id
        return folded
    }
    await model.submit("Q1")
    #expect(model.turns.isEmpty)
    #expect(try chats.loadConversation(identity: identity, babyID: baby) == AskConversation())
}

@MainActor
@Test func rangeCaptionNamesOnlyTheDaysRead() throws {
    let (store, baby, chats, root, fake) = try setup()
    defer { try? FileManager.default.removeItem(at: root) }
    let model = makeModel(store, baby, chats, fake)
    let calendar = Calendar.current
    let day = calendar.startOfDay(for: Date(timeIntervalSince1970: 1_800_000_000))
    let nextDay = calendar.date(byAdding: .day, value: 1, to: day)!
    let style = Date.FormatStyle.dateTime.day().month(.abbreviated)
    // An exclusive end at midnight belongs to the day before.
    #expect(model.rangeCaption(AskWindow(start: day, end: nextDay)) == "From the log · \(day.formatted(style))")
    let weekStart = calendar.date(byAdding: .day, value: -6, to: day)!
    #expect(model.rangeCaption(AskWindow(start: weekStart, end: nextDay))
        == "From the log · \(weekStart.formatted(style)) – \(day.formatted(style))")
}

@Test func answerTextRendersInlineMarkdownWithoutMarkers() {
    // The on-device model adds **bold** despite the instructions.
    #expect(String(AskViewModel.displayText("Prepare **60 ml** now.").characters) == "Prepare 60 ml now.")
    #expect(String(AskViewModel.displayText("Line one\nLine two").characters) == "Line one\nLine two")
    #expect(String(AskViewModel.displayText("Unclosed **bold").characters) == "Unclosed **bold")
}
