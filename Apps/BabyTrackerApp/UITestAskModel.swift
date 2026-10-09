#if DEBUG
import Foundation
import AskFeature
import BabyCore
import BabyStore

@MainActor
final class UITestAskModel: AskModel {
    private let available: Bool

    init(available: Bool = true) { self.available = available }

    func availability(for locale: Locale) -> AskAvailability {
        available ? .available : .unavailable("Apple Intelligence is unavailable on this device.")
    }

    func answer(question: String, conversation: AskConversation, profile: AskProfile, volumeUnit: VolumeUnit,
                store: BabyStore, onPartial: @escaping @MainActor (String) -> Void) async throws -> AskReply {
        if question == "How can I organize a handoff note?" {
            return AskReply(text: "Group the note by feeding, sleep, and anything the next caregiver needs to know.")
        }
        let now = store.now
        let week = AskWindow(start: now.addingTimeInterval(-7 * 86_400), end: now)
        onPartial("\(profile.name) had")
        let feed = try store.entries(for: profile.babyID, from: week.start, to: week.end).first { item in
            if case .feed = item { return true }
            return false
        }
        if let feed {
            return AskReply(text: "\(profile.name) had logged feeds this week; the latest started at \(feed.startedAt.formatted(date: .omitted, time: .shortened)).",
                            sources: [.entry(kind: "feed", id: feed.id)], toolNames: ["careLog"])
        }
        return AskReply(text: "Nothing was logged for \(profile.name) this week.", queriedRange: week,
                        toolNames: ["careLog"])
    }

    func compacted(_ conversation: AskConversation) async throws -> AskConversation? { nil }
}
#endif
