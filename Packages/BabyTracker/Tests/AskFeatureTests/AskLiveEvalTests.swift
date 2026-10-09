import Foundation
import Testing
import FoundationModels
import BabyCore
import BabyStore
@testable import AskFeature

private let questions = [
    "If we feed with breast in the same session how many milk should I prepare",
    "How many milk or formula Marc drinks when he also eats breasts",
    "How many feeds yesterday?",
    "And the day before?",
    "Is he asleep right now?",
    "When was the last diaper?",
    "How much bottle volume this week compared with last week?",
    "Is he sleeping more than last week?",
    "What happened overnight?",
    "Catch me up",
    "How long was his longest sleep in the last 3 days?",
    "How many dirty diapers in the last 48 hours?",
    "How can I make a handoff note for grandma?",
    "Is 4 wet diapers a day enough?",
    "Thanks!",
]

/// Runs the real on-device model against a seeded week and prints every
/// answer for reading. Opt in with `ASK_LIVE_EVAL=1`.
@MainActor
@Test(.enabled(if: ProcessInfo.processInfo.environment["ASK_LIVE_EVAL"] == "1"), .timeLimit(.minutes(20)))
func liveOnDeviceModelAnswersTheEvalQuestions() async throws {
    try #require(SystemLanguageModel.default.isAvailable, "Apple Intelligence is not available on this Mac")
    let now = Date()
    // The sleep timer starts 40 minutes before the questions are asked.
    let clock = TestDateProvider(now.addingTimeInterval(-40 * 60))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Marc", birthDate: now.addingTimeInterval(-132 * 86_400),
                                 referenceSex: .girls, expectedDueDate: nil)
    try seedWeek(store: store, babyID: baby.id, now: clock.now)
    _ = try store.startSleep(for: baby.id)
    clock.advance(by: 40 * 60)
    let model = AppleAskModel()
    let profile = AskProfile(babyID: baby.id, name: "Marc", birthDate: baby.birthDate)
    var conversation = AskConversation()
    var unanswered: [String] = []
    var tools: [String: [String]] = [:]
    for question in questions {
        let started = Date()
        do {
            let reply = try await model.answer(question: question, conversation: conversation, profile: profile,
                                               volumeUnit: .millilitres, store: store) { _ in }
            conversation.turns.append(AskTurn(id: UUID(), question: question, answer: reply.text, askedAt: started,
                                              answeredAt: Date(), sources: reply.sources,
                                              answerKind: reply.usedTools ? .recorded : .general,
                                              queriedRange: reply.queriedRange))
            if let compacted = try? await model.compacted(conversation) { conversation = compacted }
            tools[question] = reply.toolNames
            print("Q: \(question)\nA: \(reply.text)\ntools: \(reply.toolNames) \(String(format: "%.1f", Date().timeIntervalSince(started)))s\n")
        } catch {
            unanswered.append(question)
            print("Q: \(question)\nERROR: \(error)\n")
        }
    }
    print("memory: \(conversation.memory ?? "none")")
    #expect(unanswered.count <= 1, "Unanswered: \(unanswered)")
    #expect(tools["How many feeds yesterday?"]?.isEmpty == false)
    #expect(tools["When was the last diaper?"]?.isEmpty == false)
}

@MainActor
private func seedWeek(store: BabyStore, babyID: UUID, now: Date) throws {
    let calendar = Calendar.current
    let today = calendar.startOfDay(for: now)
    func at(_ day: Int, _ hour: Int, _ minute: Int = 0) -> Date {
        calendar.date(byAdding: .minute, value: hour * 60 + minute,
                      to: calendar.date(byAdding: .day, value: -day, to: today)!)!
    }
    func past(_ end: Date, _ write: () throws -> Void) rethrows {
        if end < now { try write() }
    }
    for day in (0...7).reversed() {
        try past(at(day, 1, 50)) { _ = try store.logSleep(from: at(day, 0, 30), to: at(day, 1, 50), for: babyID) }
        try past(at(day, 2)) { _ = try store.logBottle(volumeML: 90, contents: .formula, for: babyID, at: at(day, 2)) }
        try past(at(day, 5, 30)) { _ = try store.logSleep(from: at(day, 2, 30), to: at(day, 5, 30), for: babyID) }
        try past(at(day, 6, 12)) { _ = try store.logBreast(side: .left, from: at(day, 6), to: at(day, 6, 12), for: babyID) }
        try past(at(day, 6, 15)) { _ = try store.logBottle(volumeML: 60, contents: .breastMilk, for: babyID, at: at(day, 6, 15)) }
        try past(at(day, 6, 30)) { _ = try store.logDiaper(.wet, for: babyID, at: at(day, 6, 30)) }
        try past(at(day, 9, 30)) { _ = try store.logSleep(from: at(day, 8), to: at(day, 9, 30), for: babyID) }
        try past(at(day, 9, 45)) { _ = try store.logSupplement(name: "Vitamin D", for: babyID, at: at(day, 9, 45)) }
        try past(at(day, 10)) { _ = try store.logBottle(volumeML: 120, contents: .formula, for: babyID, at: at(day, 10)) }
        try past(at(day, 10, 15)) { _ = try store.logDiaper(.dirty, for: babyID, at: at(day, 10, 15)) }
        try past(at(day, 13, 45)) { _ = try store.logSleep(from: at(day, 12, 30), to: at(day, 13, 45), for: babyID) }
        try past(at(day, 14, 15)) { _ = try store.logBreast(side: .right, from: at(day, 14), to: at(day, 14, 15), for: babyID) }
        try past(at(day, 14, 30)) { _ = try store.logDiaper(.wet, for: babyID, at: at(day, 14, 30)) }
        try past(at(day, 17)) { _ = try store.logBottle(volumeML: 120, contents: .formula, for: babyID, at: at(day, 17)) }
        try past(at(day, 18, 10)) { _ = try store.logSleep(from: at(day, 17, 40), to: at(day, 18, 10), for: babyID) }
        try past(at(day, 19)) { _ = try store.logDiaper(.wet, for: babyID, at: at(day, 19)) }
        try past(at(day, 20, 30)) { _ = try store.logBottle(volumeML: 150, contents: .formula, for: babyID, at: at(day, 20, 30)) }
        try past(at(day, 23, 50)) { _ = try store.logSleep(from: at(day, 21, 30), to: at(day, 23, 50), for: babyID) }
    }
}
