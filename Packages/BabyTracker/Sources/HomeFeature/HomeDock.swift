import SwiftUI
import BabyCore
import BabyUI

/// The thumb zone. Start a part while idle or in a break; Stop or Wake up
/// while one runs. Every control is at least 44 pt and every icon-only
/// control has a spoken label.
struct HomeDock: View {
    let dock: Dock
    let onStartBreast: (BreastSide) -> Void
    let onStartSleep: () -> Void
    let onStopFeed: () -> Void
    let onWake: () -> Void
    let onAction: (HomeAction) -> Void

    @State private var isChoosingFeed = false

    var body: some View {
        switch dock {
        case .readOnly:
            Text(HomeCopy.readOnly)
                .font(.footnote)
                .multilineTextAlignment(.center)
                .foregroundStyle(Palette.secondaryText)
                .padding(.horizontal, 32)
                .accessibilityIdentifier("home.readOnly")
        case .start(let suggested):
            GlassDock {
                VStack(spacing: 8) {
                    HStack(spacing: 8) {
                        side(.left, suggested: suggested == .left)
                        side(.right, suggested: suggested == .right)
                    }
                    HStack(spacing: 8) {
                        tile(.bottle, Palette.feed, HomeCopy.bottle, "home.bottle") { onAction(.bottle) }
                        tile(.sleep, Palette.sleep, Tracker.sleep.title, "home.sleep", action: onStartSleep)
                        diaper
                        more
                    }
                }
            }
        case .feedRunning:
            GlassDock {
                VStack(spacing: 8) {
                    primary(HomeCopy.stop, "home.stop", action: onStopFeed)
                    HStack(spacing: 8) {
                        diaper
                        more
                    }
                }
            }
        case .sleepRunning:
            GlassDock {
                VStack(spacing: 8) {
                    primary(HomeCopy.wakeUp, "home.wake", action: onWake)
                    HStack(spacing: 8) {
                        tile(.feed, Palette.feed, Tracker.feed.title, "home.feed") { isChoosingFeed = true }
                            // A baby can fall asleep at the breast, so a feed
                            // may start while the sleep keeps running.
                            .confirmationDialog(Tracker.feed.title, isPresented: $isChoosingFeed) {
                                Button(HomeL10n.breast(.left)) { onStartBreast(.left) }
                                Button(HomeL10n.breast(.right)) { onStartBreast(.right) }
                                Button(HomeCopy.bottle) { onAction(.bottle) }
                            }
                        diaper
                        more
                    }
                }
            }
        }
    }

    private var diaper: some View {
        tile(.diaper, Palette.diaper, Tracker.diaper.title, "home.diaper") { onAction(.diaper) }
    }

    private var more: some View {
        tile(.more, Palette.secondaryText, HomeCopy.more, "home.more") { onAction(.more) }
    }

    /// A large Left or Right. The suggested side is outlined.
    private func side(_ side: BreastSide, suggested: Bool) -> some View {
        let icon: BabyIcon = side == .left ? .breastLeft : .breastRight
        return Button { onStartBreast(side) } label: {
            HStack(spacing: 8) {
                BabyIconView(icon, size: 24)
                Text(icon.letter ?? "")
                    .font(.title3.weight(.bold))
                    .foregroundStyle(Palette.primaryText)
            }
        }
        .buttonStyle(DockTileStyle(tint: Palette.feed, height: 64, outlined: suggested))
        .accessibilityLabel(HomeCopy.startBreast(side))
        .accessibilityValue(suggested ? HomeCopy.suggested : "")
        .accessibilityIdentifier(side == .left ? "home.start.left" : "home.start.right")
    }

    /// An icon-only control: its label is spoken, never shown.
    private func tile(_ icon: BabyIcon, _ tint: Color, _ label: String, _ id: String,
                      action: @escaping () -> Void) -> some View {
        Button(action: action) {
            BabyIconView(icon, size: 24)
        }
        .buttonStyle(DockTileStyle(tint: tint, height: 52))
        .accessibilityLabel(label)
        .accessibilityIdentifier(id)
    }

    /// Stop and Wake up: a light fill with dark text, the one primary action.
    private func primary(_ title: String, _ id: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Text(title)
                .font(.title3.weight(.bold))
                .lineLimit(1)
                .minimumScaleFactor(0.7)
        }
        .buttonStyle(.primaryFill)
        .accessibilityIdentifier(id)
    }
}

/// A dock tile: the item colour at about 14% opacity behind a full-colour
/// glyph, as the spec asks, with the suggested side outlined in 2 pt.
private struct DockTileStyle: ButtonStyle {
    let tint: Color
    let height: CGFloat
    var outlined = false

    func makeBody(configuration: Configuration) -> some View {
        let shape = RoundedRectangle(cornerRadius: 20, style: .continuous)
        return configuration.label
            .foregroundStyle(tint)
            .frame(maxWidth: .infinity, minHeight: height)
            .background(shape.fill(tint.opacity(0.14)))
            .overlay {
                if outlined { shape.strokeBorder(Palette.feed, lineWidth: 2) }
            }
            .contentShape(shape)
            .scaleEffect(configuration.isPressed ? 0.97 : 1)
            .animation(.easeOut(duration: 0.12), value: configuration.isPressed)
    }
}
