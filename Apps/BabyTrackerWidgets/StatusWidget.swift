import WidgetKit
import SwiftUI
import AppIntents
import BabyCore
import BabyStore
import BabyUI

struct StatusEntry: TimelineEntry {
    let date: Date
    /// Every string the widget draws. The copy lives in the package, where it
    /// is testable, rather than in this view.
    let summary: WidgetStatusSummary
    let babyID: String?
    let message: String?

    init(date: Date, summary: WidgetStatusSummary, babyID: String? = nil, message: String? = nil) {
        self.date = date
        self.summary = summary
        self.babyID = babyID
        self.message = message
    }
}

struct StatusProvider: AppIntentTimelineProvider {
    /// The widget gallery's preview. Sample values are correct here — this is
    /// what the gallery is for — and are never used for a placed widget.
    func placeholder(in context: Context) -> StatusEntry {
        StatusEntry(
            date: .now,
            summary: WidgetStatusSummary(
                babyName: String(localized: "Baby"),
                feedAge: ElapsedFormatter.total(8_040),
                sleepText: String(format: String(localized: "Awake %@"), ElapsedFormatter.total(2_400)),
                diaperAge: ElapsedFormatter.total(3_300)
            )
        )
    }

    func snapshot(for configuration: BabyWidgetConfiguration, in context: Context) async -> StatusEntry {
        // `snapshot` is not gallery-only — WidgetKit also calls it for
        // transient renders of a *placed* widget, where the sample values
        // would be a fabrication just as they were in the timeline.
        guard !context.isPreview else { return placeholder(in: context) }
        return await Self.currentEntry(for: configuration)
    }

    func timeline(for configuration: BabyWidgetConfiguration, in context: Context) async -> Timeline<StatusEntry> {
        let entry = await Self.currentEntry(for: configuration)
        return Timeline(entries: [entry], policy: .after(entry.date.addingTimeInterval(300)))
    }

    @MainActor
    private static func currentEntry(for configuration: BabyWidgetConfiguration) -> StatusEntry {
        guard let chosen = configuration.baby else {
            return StatusEntry(date: .now, summary: .noData, message: String(localized: "Choose baby"))
        }
        do {
            let store = try WidgetStore.make()
            guard let baby = try WidgetStore.baby(chosen.id, in: store) else {
                return StatusEntry(date: .now, summary: .noData, message: String(localized: "Baby unavailable"))
            }
            let now = store.now
            return StatusEntry(
                date: now,
                summary: WidgetStatusSummary(
                    babyName: baby.name,
                    snapshot: try store.homeSnapshot(for: baby.id),
                    now: now
                ),
                babyID: baby.id.uuidString
            )
        } catch {
            return StatusEntry(date: .now, summary: .noData, message: String(localized: "Unable to load"))
        }
    }
}

struct StatusWidget: Widget {
    var body: some WidgetConfiguration {
        AppIntentConfiguration(kind: "StatusWidget", intent: BabyWidgetConfiguration.self, provider: StatusProvider()) { entry in
            VStack(alignment: .leading, spacing: 0) {
                if let message = entry.message {
                    Text(message)
                        .font(.headline)
                    Spacer()
                } else {
                    Text(entry.summary.babyName)
                        .font(.headline.weight(.bold))
                        .foregroundStyle(.primary)
                        .lineLimit(1)
                        .minimumScaleFactor(0.8)
                    Spacer(minLength: Spacing.xs)
                    Label {
                        Text(entry.summary.feedAge)
                    } icon: {
                        TrackerIcon(.feed)
                    }
                    .foregroundStyle(Tracker.feed.color)
                    .accessibilityLabel("Last feed: \(entry.summary.feedAge)")
                    Spacer(minLength: Spacing.xs)
                    Label {
                        Text(entry.summary.sleepText)
                    } icon: {
                        TrackerIcon(.sleep)
                    }
                    .foregroundStyle(Tracker.sleep.color)
                    .accessibilityLabel("Sleep: \(entry.summary.sleepText)")
                    Spacer(minLength: Spacing.xs)
                    Label {
                        Text(entry.summary.diaperAge)
                    } icon: {
                        TrackerIcon(.diaper)
                    }
                    .foregroundStyle(Tracker.diaper.color)
                    .accessibilityLabel("Last diaper: \(entry.summary.diaperAge)")
                }
            }
            .font(.subheadline.weight(.semibold))
            .lineLimit(1)
            .minimumScaleFactor(0.8)
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            .containerBackground(.fill.tertiary, for: .widget)
        }
        .configurationDisplayName("Status")
        .description("Time since the last feed, sleep, and diaper.")
        .supportedFamilies([.systemSmall])
    }
}
