import Foundation
import Testing
import BabyCore
import BabyStore
@testable import AskFeature

private let zurich = TimeZone(identifier: "Europe/Zurich")!

private func local(_ year: Int, _ month: Int, _ day: Int, _ hour: Int = 0, _ minute: Int = 0) -> Date {
    var calendar = Calendar(identifier: .gregorian)
    calendar.timeZone = zurich
    return calendar.date(from: DateComponents(year: year, month: month, day: day, hour: hour, minute: minute))!
}

private func turn(_ question: String, at date: Date = Date(timeIntervalSince1970: 0)) -> AskTurn {
    AskTurn(id: UUID(), question: question, answer: "A to \(question)", askedAt: date, answeredAt: date, sources: [])
}

@Test func instructionsNameTheChildAgeAndCarryMemory() {
    let text = AskText(timeZone: zurich)
    let profile = AskProfile(babyID: UUID(), name: "Marc", birthDate: local(2026, 5, 26))
    let now = local(2026, 10, 5, 4, 28)
    let plain = AskSessionBuilder.instructions(profile: profile, now: now, timers: [], text: text, memory: nil, logData: [])
    #expect(plain.contains("The selected child is Marc, 4 months 1 week old (born 26 May 2026)."))
    #expect(plain.contains("Use only the log data above"))
    #expect(plain.contains("Right now:\nNow: Mon 5 Oct 2026, 04:28 (today is 2026-10-05).\nNo sleep or feed timer is running."))
    #expect(!plain.contains("Earlier in this chat"))
    #expect(!plain.contains("Europe/Zurich"))
    let remembered = AskSessionBuilder.instructions(profile: profile, now: now, timers: [], text: text,
                                                    memory: "4 Oct: pediatrician suggested 60–90 ml top-ups.",
                                                    logData: [])
    #expect(remembered.hasSuffix("Earlier in this chat (summary):\n4 Oct: pediatrician suggested 60–90 ml top-ups."))
}

@MainActor
@Test func liveStateGivesLocalNowAndRunningTimers() throws {
    let start = local(2026, 9, 29, 2, 10)
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(start))
    let baby = try store.addBaby(name: "Marc", birthDate: local(2026, 5, 26), referenceSex: .girls, expectedDueDate: nil)
    _ = try store.startSleep(for: baby.id)
    let timers = AskSessionBuilder.liveTimers(try store.activeTimers(for: baby.id), volumeUnit: .millilitres)
    #expect(timers == [AskLiveTimer(kind: .sleep, startedAt: start, currentStep: nil, completedSteps: [])])
    let feed = AskLiveTimer(kind: .feed, startedAt: local(2026, 9, 29, 3, 50), currentStep: "right breast",
                            completedSteps: ["left breast 12 min", "formula bottle 60 ml"])
    let text = AskText(timeZone: zurich)
    let live = """
        Now: Tue 29 Sep 2026, 04:28 (today is 2026-09-29).
        Sleep timer running since Tue 29 Sep, 02:10 (2 h 18 min): logged as asleep.
        Feed timer running since Tue 29 Sep, 03:50 (38 min): logged as feeding; now right breast; done: left breast 12 min, formula bottle 60 ml.
        """
    #expect(AskSessionBuilder.header(now: local(2026, 9, 29, 4, 28), timers: timers + [feed], text: text) == live)
    let profile = AskProfile(babyID: baby.id, name: "Marc", birthDate: local(2026, 5, 26))
    #expect(AskSessionBuilder.instructions(profile: profile, now: local(2026, 9, 29, 4, 28),
                                           timers: timers + [feed], text: text, memory: nil, logData: [])
        .contains("Right now:\n" + live))
    #expect(AskSessionBuilder.header(now: start, timers: [], text: text)
        == "Now: Tue 29 Sep 2026, 02:10 (today is 2026-09-29).\nNo sleep or feed timer is running.")
    // Review focus 2: a pasted wall of text is cut before it can fill the window.
    let long = String(repeating: "a", count: 5_000)
    #expect(AskSessionBuilder.prompt(question: long) == String(repeating: "a", count: 1_500))
    // The cap is in UTF-8 bytes, so scripts with more bytes per character are cut sooner.
    #expect(AskSessionBuilder.prompt(question: String(repeating: "日", count: 1_000)) == String(repeating: "日", count: 500))
    #expect(AskSessionBuilder.prompt(question: "Is he asleep?") == "Is he asleep?")
}

@Test func historyKeepsTheNewestTurnsThatFit() {
    let turns = (1...5).map { turn("Q\($0)") }
    let kept = AskSessionBuilder.history(AskConversation(turns: turns), budget: 25, tokens: { _ in 10 })
    #expect(kept.map(\.question) == ["Q4", "Q5"])
    let many = (1...6).map { turn("Q\($0)") }
    #expect(AskSessionBuilder.history(AskConversation(turns: many), budget: 1_000, tokens: { _ in 10 })
        .map(\.question) == ["Q3", "Q4", "Q5", "Q6"])
}

@Test func historyStartsAfterTheSummarizedTurn() {
    let turns = (1...5).map { turn("Q\($0)") }
    let conversation = AskConversation(turns: turns, memory: "notes", memoryCoversThrough: turns[1].id)
    #expect(AskSessionBuilder.history(conversation, budget: 1_000, tokens: { _ in 10 }).map(\.question)
        == ["Q3", "Q4", "Q5"])
}

@Test func compactionFoldsEveryTurnHistoryNoLongerShows() {
    let turns = (1...6).map { turn("Q\($0)") }
    // Turns older than the newest four reach the model only through memory.
    #expect(AskCompaction.turnsToFold(AskConversation(turns: turns)).map(\.question) == ["Q1", "Q2"])
    #expect(AskCompaction.turnsToFold(AskConversation(turns: Array(turns.prefix(4)))).isEmpty)
    // A backlog after failed compactions folds a bounded batch at a time.
    let backlog = (1...14).map { turn("Q\($0)") }
    #expect(AskCompaction.turnsToFold(AskConversation(turns: backlog)).map(\.question)
        == ["Q1", "Q2", "Q3", "Q4", "Q5", "Q6"])
}

@Test func historySkipsATurnTooLongToFitAndKeepsNewerOnes() {
    let turns = [turn("Q1"), turn(String(repeating: "x", count: 500)), turn("Q3")]
    let kept = AskSessionBuilder.history(AskConversation(turns: turns), budget: 25,
                                         tokens: { $0.count > 100 ? 1_000 : 10 })
    #expect(kept.map(\.question) == ["Q1", "Q3"])
}

@Test func summaryPromptDatesExchangesAndKeepsExistingNotes() {
    let text = AskText(timeZone: zurich)
    let asked = turn("He spits up after feeds", at: local(2026, 10, 4, 21, 10))
    #expect(AskCompaction.summaryPrompt(memory: "3 Oct: prefers ml.", folding: [asked], text: text)
        == "Existing notes:\n3 Oct: prefers ml.\n\nNew exchanges:\n[Sun 4 Oct, 21:10] Parent: He spits up after feeds\nAssistant: A to He spits up after feeds")
    #expect(AskCompaction.summaryPrompt(memory: nil, folding: [asked], text: text).hasPrefix("Existing notes:\nnone\n\n"))
    #expect(AskCompaction.summaryInstructions(wordLimit: 120).contains("at most 120 words"))
    let pasted = turn(String(repeating: "x", count: 1_000))
    let bounded = AskCompaction.summaryPrompt(memory: nil, folding: [pasted], text: text)
    #expect(bounded.contains(String(repeating: "x", count: 600)))
    #expect(!bounded.contains(String(repeating: "x", count: 601)))
}

@Test func planAndAnswerPromptsCarryTheConversationAndLogData() {
    let text = AskText(timeZone: zurich)
    let profile = AskProfile(babyID: UUID(), name: "Marc", birthDate: local(2026, 5, 26))
    let earlier = [turn("How many feeds yesterday?")]
    #expect(AskSessionBuilder.planInstructions(profile: profile, now: local(2026, 10, 5, 4, 28), timers: [], text: text)
        .contains("today is 2026-10-05"))
    #expect(AskSessionBuilder.planInstructions(profile: profile, now: local(2026, 10, 5, 4, 28), timers: [], text: text)
        .contains("Yesterday is 2026-10-04 and the day before yesterday is 2026-10-03."))
    #expect(AskSessionBuilder.planPrompt(question: "And the day before?", history: earlier)
        == "Earlier conversation:\nParent: How many feeds yesterday?\nAsk: A to How many feeds yesterday?\n\nLatest question: And the day before?")
    #expect(AskSessionBuilder.planPrompt(question: "Hi", history: []) == "Earlier conversation: none\n\nLatest question: Hi")
    let now = local(2026, 10, 5, 4, 28)
    #expect(AskSessionBuilder.instructions(profile: profile, now: now, timers: [], text: text, memory: nil,
                                           logData: ["[totals, today]\n2 feeds"])
        .contains("Log data for the latest question:\n[totals, today]\n2 feeds"))
    #expect(AskSessionBuilder.instructions(profile: profile, now: now, timers: [], text: text, memory: nil, logData: [])
        .contains("Log data for the latest question: none was read."))
}
