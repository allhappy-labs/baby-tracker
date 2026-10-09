import Foundation
import Testing
import BabyCore
import BabyProtocol
@testable import BabyStore

@Suite("Live Activity canonical projection") @MainActor
struct LiveActivityProjectionTests {
    struct Fixture: Decodable {
        let name: String
        let now: String
        let minutes: Int
        let baby: CanonicalEntity
        let timers: [CanonicalEntity]
        let feeds: [CanonicalEntity]
        /// Completed sleep parts; a running sleep that continues one of their
        /// sessions counts their time asleep. Older cases carry none.
        let sleeps: [CanonicalEntity]?
        let expected: [TimerActivityContentState]
    }
    /// Running cards go through `LiveActivityController.running(_:in:)`, the
    /// builder every card the app writes uses, so the server's projection is
    /// held to what the phone actually shows — the whole sleep included.
    @Test func canonicalFixturesMatchActualPresentation() throws {
        let path = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            .appending(path: "Server/api/fixtures/live-activity-v1/projection.json")
        let fixtures = try JSONDecoder().decode([Fixture].self, from: Data(contentsOf: path))
        for f in fixtures {
            let clock = TestDateProvider(try instant(f.now))
            let store = try BabyStore.inMemory(dateProvider: clock)
            store.breakLimits = { BreakLimits(feed: TimeInterval(f.minutes * 60), sleep: 900) }
            var actual: [TimerActivityContentState] = []
            if f.baby.meta.tombstonedAt == nil {
                for entity in f.sleeps ?? [] {
                    let p = try fields(entity)
                    let sleep = SleepEntry(id: entity.meta.id, babyID: f.baby.meta.id,
                        startedAt: try instant(string(p, "started_at")), timeZoneID: "UTC")
                    sleep.endedAt = try (p["ended_at"] as? String).map(instant)
                    sleep.sleepSessionID = (p["sleep_session_id"] as? String).flatMap(UUID.init(uuidString:))
                    sleep.isTombstoned = entity.meta.tombstonedAt != nil
                    store.context.insert(sleep)
                }
                for entity in f.feeds {
                    let p = try fields(entity)
                    let feed = FeedEntry(id: entity.meta.id, babyID: f.baby.meta.id,
                        kind: try #require(FeedKind(rawValue: try string(p, "kind"))),
                        startedAt: try instant(string(p, "started_at")), timeZoneID: "UTC")
                    feed.endedAt = try (p["ended_at"] as? String).map(instant)
                    feed.feedingSessionID = (p["feeding_session_id"] as? String).flatMap(UUID.init(uuidString:))
                    feed.components = try WireFeedComponent.decodeComponents("components", of: #require(entity.payload)).map(BabyStore.localComponent)
                    store.context.insert(feed)
                }
                var timers: [ActiveTimer] = []
                for entity in f.timers {
                    let p = try fields(entity)
                    let timer = ActiveTimer(id: entity.meta.id, babyID: f.baby.meta.id,
                        kind: try #require(TimerKind(rawValue: try string(p, "timer_kind"))),
                        startedAt: try instant(string(p, "started_at")), timeZoneID: "UTC",
                        currentSide: (p["current_side"] as? String).flatMap(BreastSide.init(rawValue:)))
                    timer.currentBottleStartedAt = try (p["current_bottle_started_at"] as? String).map(instant)
                    timer.completedComponents = try WireFeedComponent.decodeComponents("completed_components", of: #require(entity.payload)).map(BabyStore.localComponent)
                    timer.sessionID = (p["session_id"] as? String).flatMap(UUID.init(uuidString:))
                    store.context.insert(timer)
                    timers.append(timer)
                }
                try store.context.save()
                for timer in timers {
                    actual.append(LiveActivityController.content(LiveActivityController.running(timer, in: store), in: store))
                }
                if let presentation = try LiveActivityController.breakPresentation(kind: .feed, babyID: f.baby.meta.id, in: store) {
                    actual.append(TimerActivityContentState(presentation))
                }
            }
            #expect(actual.count == f.expected.count, "\(f.name)")
            for (got, expected) in zip(actual, f.expected) {
                #expect(got.startedAt == expected.startedAt, "\(f.name)")
                #expect(got.activityKind == expected.activityKind, "\(f.name)")
                #expect(got.phase == expected.phase, "\(f.name)")
                #expect(got.breakEndsAt == expected.breakEndsAt, "\(f.name)")
                 #expect(got.omittedParts == expected.omittedParts, "\(f.name)")
                #expect(got.parts == expected.parts, "\(f.name)")
                #expect(got.suggestedSide == expected.suggestedSide, "\(f.name)")
                #expect(got.isActionable(now: clock.now), "\(f.name)")
            }
        }
    }
    @Test func summaryKeepsLastEightPartsAndOmittedCount() {
        let parts = (0..<19).map { PartLabel(side: $0 % 2 == 0 ? "left" : nil, seconds: $0) }
        let presentation = ActivityPresentation(startedAt: .now, sideLabel: "Break", activityKind: .timedBottle,
            phase: .feedBreak, breakEndsAt: nil, parts: parts, suggestedSide: "left")
        let state = TimerActivityContentState(presentation)
        #expect(state.parts == Array(parts.suffix(8)))
        #expect(state.omittedParts == 11)
    }
    private func fields(_ entity: CanonicalEntity) throws -> [String: Any] {
        try #require(JSONSerialization.jsonObject(with: JSONEncoder().encode(entity.payload)) as? [String: Any])
    }
    private func string(_ fields: [String: Any], _ key: String) throws -> String { try #require(fields[key] as? String) }
    private func instant(_ value: String) throws -> Date {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let date = formatter.date(from: value) { return date }
        formatter.formatOptions = [.withInternetDateTime]
        return try #require(formatter.date(from: value))
    }
}
