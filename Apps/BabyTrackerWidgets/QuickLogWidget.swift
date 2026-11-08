import WidgetKit
import SwiftUI
import AppIntents
import BabyCore
import BabyStore
import BabyUI

/// The first four slots of the baby's Home order — the same order the dock
/// follows — each a direct write through the same store calls as the app
/// where one tap is the whole entry.
///
/// L and R start that breast part (joining the feed in its break, as on
/// Home); Sleep toggles the nap; Diaper logs a wet diaper; Bottle starts a
/// timed bottle. None of those opens the app. Solids, Meds and Growth need
/// more than a tap, so they open the app at their panel.
struct QuickLogWidget: Widget {
    var body: some WidgetConfiguration {
        AppIntentConfiguration(kind: "QuickLogWidget", intent: BabyWidgetConfiguration.self, provider: StatusProvider()) { entry in
            VStack(spacing: Spacing.s) {
                Text(entry.message ?? entry.summary.babyName)
                    .font(.caption.weight(.bold))
                    .frame(maxWidth: .infinity, alignment: .leading)

                if let babyID = entry.babyID, let baby = UUID(uuidString: babyID) {
                    HStack(spacing: Spacing.s) {
                        ForEach(HomeLayout(order: TrackingPreferences().homeOrder(for: baby)).widgetSlots(), id: \.self) { slot in
                            QuickLogButton(slot: slot, babyID: babyID, baby: baby)
                        }
                    }
                }
            }
            .containerBackground(.fill.tertiary, for: .widget)
        }
        .configurationDisplayName("Quick log")
        .description("Log with the first actions from your Home buttons.")
        .supportedFamilies([.systemMedium])
    }
}

/// One slot. The four that log in a tap are buttons on an intent; the rest
/// are links into the app.
private struct QuickLogButton: View {
    let slot: HomeLayout.WidgetSlot
    let babyID: String
    let baby: UUID

    var body: some View {
        switch slot {
        case .breastLeft:
            Button(intent: StartBreastIntent(babyID: babyID, side: BreastSide.left.rawValue)) {
                TileLabel(icon: .breastLeft, title: String(localized: "Left"))
            }
            .buttonStyle(.tile(Palette.feed, size: .widget))
            .accessibilityLabel(String(localized: "Start left breast"))
        case .breastRight:
            Button(intent: StartBreastIntent(babyID: babyID, side: BreastSide.right.rawValue)) {
                TileLabel(icon: .breastRight, title: String(localized: "Right"))
            }
            .buttonStyle(.tile(Palette.feed, size: .widget))
            .accessibilityLabel(String(localized: "Start right breast"))
        case .action(.sleep):
            Button(intent: ToggleSleepIntent(babyID: babyID)) { label(.sleep) }
                .buttonStyle(.tile(HomeActionKind.sleep.color, size: .widget))
        case .action(.diaper):
            Button(intent: LogDiaperIntent(babyID: babyID)) { label(.diaper) }
                .buttonStyle(.tile(HomeActionKind.diaper.color, size: .widget))
        case .action(.bottle):
            Button(intent: StartBottleIntent(babyID: baby)) { label(.bottle) }
                .buttonStyle(.tile(HomeActionKind.bottle.color, size: .widget))
        case .action(let kind):
            // A `Link` takes no `ButtonStyle`: it wears the tile's surface.
            Link(destination: LogLink(kind: kind, babyID: baby).url) {
                label(kind).tileSurface(tint: kind.color, size: .widget)
            }
        }
    }

    private func label(_ kind: HomeActionKind) -> TileLabel {
        TileLabel(icon: kind.icon, title: kind.title)
    }
}

/// The glyph over a short label, inside a `.tile` button or link. The glyph
/// takes the style's tint; the label stays in the primary text colour.
private struct TileLabel: View {
    let icon: BabyIcon
    let title: String

    var body: some View {
        VStack(spacing: Spacing.xs) {
            BabyIconView(icon, size: 22)
            Text(title)
                .font(.caption2.weight(.semibold))
                .foregroundStyle(Palette.primaryText)
                .lineLimit(1)
                .minimumScaleFactor(0.7)
        }
    }
}
