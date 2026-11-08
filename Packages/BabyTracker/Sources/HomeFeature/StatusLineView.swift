import SwiftUI
import BabyCore
import BabyUI

/// Time since the activities that are not the hero: an icon and "23m ago"
/// each, the same in every state.
struct StatusLineView: View {
    let items: [StatusItem]

    var body: some View {
        TimelineView(.everyMinute) { context in
            HStack(spacing: 14) {
                ForEach(items, id: \.tracker) { item in
                    HStack(spacing: 5) {
                        BabyIconView(item.tracker.icon, size: 14)
                            .foregroundStyle(item.tracker.color)
                        Text(Self.styled(HomeCopy.elapsed(since: item.since, now: context.date)))
                    }
                }
            }
            .accessibilityElement(children: .ignore)
            // Read, never tapped: without the trait the audit took the
            // element for a control and measured it against 44 pt.
            .accessibilityAddTraits(.isStaticText)
            .accessibilityLabel(Self.spoken(items, now: context.date))
            .accessibilityIdentifier("home.statusLine")
        }
    }

    /// "23m ago": the time carries the weight, "ago" is smaller and quieter
    /// wherever the language puts it.
    static func styled(_ elapsed: String) -> AttributedString {
        var text = AttributedString(HomeCopy.ago(elapsed))
        text.font = .caption2
        text.foregroundColor = Palette.secondaryText
        if let time = text.range(of: elapsed) {
            text[time].font = .footnote.weight(.semibold).monospacedDigit()
            text[time].foregroundColor = Palette.primaryText
        }
        return text
    }

    /// "Feed 2 hours, 10 minutes, Diaper 1 hour, 5 minutes". The sleep item
    /// counts from the end of the last sleep, so it is spoken as time awake.
    static func spoken(_ items: [StatusItem], now: Date) -> String {
        items.map { item in
            let name = item.tracker == .sleep ? HomeCopy.awake : item.tracker.title
            return name + " " + HomeCopy.spokenDuration(now.timeIntervalSince(item.since))
        }
        .joined(separator: ", ")
    }
}

/// A running sleep, while a feed is the hero: a moon and the live time asleep.
struct SleepChipView: View {
    let clockFrom: Date

    var body: some View {
        // Ticks on the sleep's own seconds, as `Text(timerInterval:)` did.
        TimelineView(.periodic(from: clockFrom, by: 1)) { context in
            let seconds = context.date.timeIntervalSince(clockFrom)
            Chip(HomeCopy.partTime(Int(seconds)), icon: .sleep, tint: Palette.sleep, size: .small,
                 monospacedDigits: true)
                .accessibilityElement(children: .ignore)
                .accessibilityAddTraits(.isStaticText)
                .accessibilityLabel(HomeCopy.asleep)
                .accessibilityValue(HomeCopy.spokenDuration(seconds))
                .accessibilityIdentifier("home.sleepChip")
        }
    }
}
