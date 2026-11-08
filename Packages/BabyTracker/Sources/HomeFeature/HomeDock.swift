import SwiftUI
import BabyCore
import BabyUI

/// The thumb zone. Start what the baby's order puts first while idle or in a
/// break; Stop or Wake up while a part runs. Every control is at least 44 pt
/// and every icon-only control has a spoken label.
struct HomeDock: View {
    let dock: Dock
    let onStartBreast: (BreastSide) -> Void
    let onStartSleep: () -> Void
    let onStopFeed: () -> Void
    let onWake: () -> Void
    let onAction: (HomeAction) -> Void

    @State private var isChoosingFeed = false
    @State private var isChoosingBreast = false

    var body: some View {
        switch dock {
        case .readOnly:
            Text(HomeCopy.readOnly)
                .font(.footnote)
                .multilineTextAlignment(.center)
                .foregroundStyle(Palette.secondaryText)
                .padding(.horizontal, 32)
                .accessibilityIdentifier("home.readOnly")
        case let .start(suggested, big, tiles):
            GlassDock {
                VStack(spacing: Spacing.s) {
                    if big == .breast {
                        HStack(spacing: Spacing.s) {
                            side(.left, suggested: suggested == .left)
                            side(.right, suggested: suggested == .right)
                        }
                    } else {
                        bigAction(big)
                    }
                    HStack(spacing: Spacing.s) {
                        ForEach(tiles, id: \.self) { actionTile($0) }
                        more
                    }
                }
            }
        case .feedRunning(_, _, let tiles):
            GlassDock {
                VStack(spacing: Spacing.s) {
                    primary(HomeCopy.stop, "home.stop", action: onStopFeed)
                    HStack(spacing: Spacing.s) {
                        ForEach(tiles, id: \.self) { actionTile($0) }
                        more
                    }
                }
            }
        case .sleepRunning(_, let tiles):
            GlassDock {
                VStack(spacing: Spacing.s) {
                    primary(HomeCopy.wakeUp, "home.wake", action: onWake)
                    HStack(spacing: Spacing.s) {
                        ForEach(tiles, id: \.self) { slot in
                            switch slot {
                            case .feed: feedChooser
                            case .action(let kind): actionTile(kind)
                            }
                        }
                        more
                    }
                }
            }
        }
    }

    /// A baby can fall asleep at the breast, so a feed may start while the
    /// sleep keeps running.
    private var feedChooser: some View {
        tile(.feed, Palette.feed, Tracker.feed.title, "home.feed") { isChoosingFeed = true }
            .confirmationDialog(Tracker.feed.title, isPresented: $isChoosingFeed) {
                Button(HomeL10n.breast(.left)) { onStartBreast(.left) }
                Button(HomeL10n.breast(.right)) { onStartBreast(.right) }
                Button(HomeCopy.bottle) { onAction(.bottle) }
            }
    }

    /// One action as a small tile. Breast has two sides, so as a tile it
    /// asks which.
    @ViewBuilder
    private func actionTile(_ kind: HomeActionKind) -> some View {
        let button = tile(kind.icon, kind.color, kind.title, "home.\(kind.rawValue)") { run(kind) }
        if kind == .breast {
            button.confirmationDialog(kind.title, isPresented: $isChoosingBreast) {
                Button(HomeL10n.breast(.left)) { onStartBreast(.left) }
                Button(HomeL10n.breast(.right)) { onStartBreast(.right) }
            }
        } else {
            button
        }
    }

    /// The order's first action when it is not Breast: one wide button with
    /// its name, the height of Left and Right.
    private func bigAction(_ kind: HomeActionKind) -> some View {
        Button { run(kind) } label: {
            HStack(spacing: Spacing.s) {
                BabyIconView(kind.icon, size: 24)
                Text(kind.title)
                    .font(.title3.weight(.bold))
                    .lineLimit(1)
                    .minimumScaleFactor(0.7)
            }
        }
        .buttonStyle(.tile(kind.color, size: .large))
        .accessibilityIdentifier("home.\(kind.rawValue)")
    }

    private func run(_ kind: HomeActionKind) {
        switch kind {
        case .breast: isChoosingBreast = true
        case .sleep: onStartSleep()
        case .bottle: onAction(.bottle)
        case .diaper: onAction(.diaper)
        case .solids: onAction(.solids)
        case .meds: onAction(.meds)
        case .growth: onAction(.growth)
        case .vaccine: onAction(.vaccine)
        case .pump: onAction(.pump)
        }
    }

    private var more: some View {
        tile(.more, Palette.secondaryText, HomeCopy.more, "home.more") { onAction(.more) }
    }

    /// A large Left or Right. The suggested side is the active tile: a
    /// stronger fill, and "Suggested" for VoiceOver.
    private func side(_ side: BreastSide, suggested: Bool) -> some View {
        let icon: BabyIcon = side == .left ? .breastLeft : .breastRight
        return Button { onStartBreast(side) } label: {
            HStack(spacing: Spacing.s) {
                BabyIconView(icon, size: 24)
                Text(icon.letter ?? "")
                    .font(.title3.weight(.bold))
            }
        }
        .buttonStyle(.tile(Palette.feed, size: .large, isActive: suggested))
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
        .buttonStyle(.tile(tint, size: .regular))
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
        .buttonStyle(.primary(.dock))
        .accessibilityIdentifier(id)
    }
}
