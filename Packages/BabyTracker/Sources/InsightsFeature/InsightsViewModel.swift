import Foundation
import Observation
import BabyCore
import BabyStore
import GrowthFeature

@MainActor
@Observable
public final class InsightsViewModel {
    private let store: BabyStore
    private let babyID: UUID
    /// Growth keeps its own model: its readings are not day-bucketed and it
    /// ignores the range.
    public let growth: GrowthViewModel
    /// The baby's vaccinations: all time, whatever the range.
    public let immunizations: ImmunizationsViewModel
    /// A preference that can change while the screen is alive; the view sets
    /// it and reloads.
    public var volumeUnit: VolumeUnit
    /// Set by the range picker; the view reloads on change.
    public var range: InsightRange = .week
    private(set) var summary = InsightsSummary.empty
    var sections: [InsightSection] { InsightsPresentation(summary: summary, volumeUnit: volumeUnit).sections }

    public init(store: BabyStore, babyID: UUID, volumeUnit: VolumeUnit = .millilitres) {
        self.store = store
        self.babyID = babyID
        self.volumeUnit = volumeUnit
        self.growth = GrowthViewModel(store: store, babyID: babyID)
        self.immunizations = ImmunizationsViewModel(store: store, babyID: babyID)
    }

    /// One fetch per kind over the period and the period before it (or all
    /// history for All), then the calculator. Sessions come from the same
    /// projections Home and History use, so a joined feed or sleep counts
    /// once, on the day its first part started.
    public func load() throws {
        do {
            try growth.load()
            try immunizations.load()
            let zone = TimeZone.current
            let now = store.now
            var calendar = Calendar(identifier: .gregorian)
            calendar.timeZone = zone
            let today = calendar.startOfDay(for: now)
            let end = calendar.date(byAdding: .day, value: 1, to: today) ?? now
            // One extra day so the period's first feed has the feed before it.
            let start = range.days.flatMap { calendar.date(byAdding: .day, value: -(2 * $0 + 1), to: today) } ?? .distantPast

            var feedsByID: [UUID: FeedEntry] = [:]
            var sleepsByID: [UUID: SleepEntry] = [:]
            var input = InsightsInput()
            for item in try store.entries(for: babyID, from: start, to: end) {
                switch item {
                case .feed(let entry): feedsByID[entry.id] = entry
                case .sleep(let entry): sleepsByID[entry.id] = entry
                case .diaper(let entry): input.diapers.append(entry.startedAt)
                case .supplement: break
                case .immunization: break
                case .pump: break
                }
            }
            input.feeds = try store.feedingSessions(for: babyID, from: start, to: end).map { session in
                let components = session.feedIDs.compactMap { feedsByID[$0] }.flatMap(\.components)
                return FeedSessionInput(
                    startedAt: session.startedAt,
                    hasBreast: components.contains { $0.breastSide != nil },
                    hasBottle: components.contains { $0.bottleContents != nil },
                    leftSeconds: components.filter { $0.breastSide == .left }.compactMap(\.durationSeconds).reduce(0, +),
                    rightSeconds: components.filter { $0.breastSide == .right }.compactMap(\.durationSeconds).reduce(0, +)
                )
            }
            input.bottles = try store.measuredBottleSteps(for: babyID, from: start, to: end)
                .map { BottleInput(at: $0.at, volumeML: $0.volumeML) }
            input.sleeps = try store.sleepSessions(for: babyID, from: start, to: end).map { session in
                let parts = session.sleepIDs.compactMap { sleepsByID[$0] }
                return SleepSessionInput(
                    startedAt: session.startedAt,
                    endedAt: session.endedAt,
                    asleepSeconds: session.asleepSeconds,
                    kind: parts.first?.kind ?? .nap,
                    partSeconds: parts.compactMap(\.durationSeconds)
                )
            }
            summary = InsightsCalculator(timeZone: zone).summary(input, range: range, now: now)
        } catch {
            // A realm switch or failed read must not leave another family's
            // previous feeding/sleep/diaper stats presented as current data.
            summary = .empty
            immunizations.reset()
            throw error
        }
    }
}
