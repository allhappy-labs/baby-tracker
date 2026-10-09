import SwiftUI
import BabyCore
import BabyUI

/// Time since the activities that are not the hero: an icon and a number
/// each, the same in every state. A number beside an icon means time since.
struct StatusLineView: View {
    let items: [StatusItem]

    var body: some View {
        TimelineView(.everyMinute) { context in
            HStack(spacing: 14) {
                ForEach(items, id: \.tracker) { item in
                    HStack(spacing: 5) {
                        BabyIconView(item.tracker.icon, size: 14)
                            .foregroundStyle(item.tracker.color)
                        Text(HomeCopy.elapsed(since: item.since, now: context.date))
                            .font(.footnote.weight(.semibold).monospacedDigit())
                            .foregroundStyle(Palette.primaryText)
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
        TimelineView(.periodic(from: .now, by: 1)) { context in
            HStack(spacing: 6) {
                BabyIconView(.sleep, size: 14)
                    .foregroundStyle(Palette.sleep)
                Text(timerInterval: clockFrom...Date.distantFuture, countsDown: false)
                    .font(.footnote.weight(.semibold).monospacedDigit())
                    .foregroundStyle(Palette.primaryText)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 6)
            .background(Palette.sleep.opacity(0.14), in: Capsule())
            .accessibilityElement(children: .ignore)
            .accessibilityAddTraits(.isStaticText)
            .accessibilityLabel(HomeCopy.asleep)
            .accessibilityValue(HomeCopy.spokenDuration(context.date.timeIntervalSince(clockFrom)))
            .accessibilityIdentifier("home.sleepChip")
        }
    }
}
