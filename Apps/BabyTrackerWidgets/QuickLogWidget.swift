import WidgetKit
import SwiftUI
import AppIntents
import BabyCore
import BabyUI

/// L · R · Sleep · Diaper — Home's most common taps, each one a direct write
/// through the same store calls as the app, none of which opens it.
///
/// L and R start that breast part (joining the feed in its break, as on
/// Home); Sleep toggles the nap; Diaper logs a wet diaper.
struct QuickLogWidget: Widget {
    var body: some WidgetConfiguration {
        AppIntentConfiguration(kind: "QuickLogWidget", intent: BabyWidgetConfiguration.self, provider: StatusProvider()) { entry in
            VStack(spacing: 9) {
                Text(entry.message ?? entry.summary.babyName)
                    .font(.caption.weight(.bold))
                    .frame(maxWidth: .infinity, alignment: .leading)

                if let babyID = entry.babyID {
                    HStack(spacing: 8) {
                        Button(intent: StartBreastIntent(babyID: babyID, side: BreastSide.left.rawValue)) {
                            QuickLogTile(icon: .breastLeft, title: String(localized: "Left"), tint: Palette.feed)
                        }
                        .accessibilityLabel(String(localized: "Start left breast"))

                        Button(intent: StartBreastIntent(babyID: babyID, side: BreastSide.right.rawValue)) {
                            QuickLogTile(icon: .breastRight, title: String(localized: "Right"), tint: Palette.feed)
                        }
                        .accessibilityLabel(String(localized: "Start right breast"))

                        Button(intent: ToggleSleepIntent(babyID: babyID)) {
                            QuickLogTile(icon: .sleep, title: String(localized: "Sleep"), tint: Palette.sleep)
                        }

                        Button(intent: LogDiaperIntent(babyID: babyID)) {
                            QuickLogTile(icon: .diaper, title: String(localized: "Diaper"), tint: Palette.diaper)
                        }
                    }
                    .buttonStyle(.plain)
                }
            }
            .containerBackground(.fill.tertiary, for: .widget)
        }
        .configurationDisplayName("Quick log")
        .description("Log a feed, sleep, or diaper without opening the app.")
        .supportedFamilies([.systemMedium])
    }
}

/// One button: the glyph over a short label, on the tracker's tint at about
/// 14% — the dock's tile.
private struct QuickLogTile: View {
    let icon: BabyIcon
    let title: String
    let tint: Color

    var body: some View {
        let shape = RoundedRectangle(cornerRadius: 14, style: .continuous)
        VStack(spacing: 4) {
            BabyIconView(icon, size: 22)
                .foregroundStyle(tint)
            Text(title)
                .font(.caption2.weight(.semibold))
                .lineLimit(1)
                .minimumScaleFactor(0.7)
        }
        .frame(maxWidth: .infinity, minHeight: 56)
        .background(shape.fill(tint.opacity(0.14)))
        .contentShape(shape)
    }
}
