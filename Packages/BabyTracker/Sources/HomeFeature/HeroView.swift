import SwiftUI
import BabyCore
import BabyUI

/// The one large number. Running parts and breaks count seconds; time awake
/// counts minutes. Both tick from the hero's stored dates, never a reload.
struct HeroView: View {
    let hero: Hero
    /// Opens the running timer's editor. Nil for a caregiver who may not
    /// write, and for states with nothing running.
    let onEdit: (() -> Void)?

    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        switch hero {
        case .feedRunning(let icon, let title, let startedAt):
            editable {
                running(icon: icon, title: title, tint: Palette.feed, clockFrom: startedAt, since: startedAt)
            }
        case .sleepRunning(let clockFrom, let since):
            editable {
                running(icon: .sleep, title: HomeCopy.asleep, tint: Palette.sleep, clockFrom: clockFrom, since: since)
            }
        case .feedBreak(let since, let parts):
            feedBreak(since: since, parts: parts)
                .accessibilityIdentifier("home.hero")
        case .awake(let since):
            awake(since: since)
                .accessibilityIdentifier("home.hero")
        }
    }

    @ViewBuilder
    private func editable(@ViewBuilder _ content: () -> some View) -> some View {
        if let onEdit {
            Button(action: onEdit) { content() }
                .buttonStyle(.plain)
                .accessibilityHint(HomeCopy.editStartTime)
                .accessibilityIdentifier("home.hero")
        } else {
            content()
                .accessibilityIdentifier("home.hero")
        }
    }

    private func running(icon: BabyIcon, title: String, tint: Color, clockFrom: Date, since: Date) -> some View {
        TimelineView(.periodic(from: .now, by: 1)) { context in
            let seconds = context.date.timeIntervalSince(clockFrom)
            VStack(spacing: 2) {
                HStack(spacing: 6) {
                    Circle()
                        .frame(width: 7, height: 7)
                        // The pulse rides the seconds tick that is already
                        // running, rather than a repeat-forever animation.
                        .opacity(reduceMotion || Int(seconds) % 2 == 0 ? 1 : 0.35)
                        .animation(reduceMotion ? nil : .easeInOut(duration: 0.8), value: Int(seconds))
                    BabyIconView(icon, size: 18)
                    Text(title)
                        .font(.subheadline.weight(.bold))
                }
                .foregroundStyle(tint)
                Text(timerInterval: clockFrom...Date.distantFuture, countsDown: false)
                    .numeralStyle(.hero)
                    .lineLimit(1)
                    .minimumScaleFactor(0.5)
                sinceLabel(since, tint: tint)
            }
            .frame(maxWidth: .infinity)
            .contentShape(Rectangle())
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(title)
            .accessibilityValue(HomeCopy.spokenDuration(seconds, includeSeconds: true) + ", " + HomeCopy.since(since))
        }
    }

    /// When the start can be edited, the start time is drawn as a tinted chip
    /// with a pencil, so the hero's tap target is visible. The whole hero
    /// stays the button; the chip is its cue.
    @ViewBuilder
    private func sinceLabel(_ since: Date, tint: Color) -> some View {
        if onEdit != nil {
            Chip(HomeCopy.since(since), systemImage: "pencil", tint: tint, size: .small)
                .padding(.top, Spacing.s)
        } else {
            Text(HomeCopy.since(since))
                .font(.footnote)
                .foregroundStyle(Palette.secondaryText)
        }
    }

    private func feedBreak(since: Date, parts: [PartSummary]) -> some View {
        TimelineView(.periodic(from: .now, by: 1)) { context in
            VStack(spacing: 6) {
                HStack(spacing: 6) {
                    BabyIconView(.feed, size: 14)
                    Text(HomeCopy.feedBreak)
                        .font(.subheadline.weight(.semibold))
                }
                .foregroundStyle(Palette.feed)
                Text(timerInterval: since...Date.distantFuture, countsDown: false)
                    .numeralStyle(.hero)
                    .lineLimit(1)
                    .minimumScaleFactor(0.5)
                PartsRow(parts.map(\.partsRowItem))
            }
            .frame(maxWidth: .infinity)
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(HomeCopy.feedBreak)
            .accessibilityValue(([HomeCopy.spokenDuration(context.date.timeIntervalSince(since), includeSeconds: true)]
                                 + parts.map(HomeCopy.spokenPart)).joined(separator: "; "))
        }
    }

    private func awake(since: Date?) -> some View {
        TimelineView(.everyMinute) { context in
            VStack(spacing: 2) {
                HStack(spacing: 6) {
                    BabyIconView(.sleep, size: 14)
                    Text(HomeCopy.awake)
                        .font(.subheadline.weight(.semibold))
                }
                .foregroundStyle(Palette.sleep)
                Text(since.map { HomeCopy.elapsed(since: $0, now: context.date) } ?? "—")
                    .numeralStyle(.hero)
                    .lineLimit(1)
                    .minimumScaleFactor(0.5)
            }
            .frame(maxWidth: .infinity)
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(HomeCopy.awake)
            .accessibilityValue(since.map { HomeCopy.spokenDuration(context.date.timeIntervalSince($0)) } ?? "—")
        }
    }
}

extension PartSummary {
    /// A finished part in the kit's `PartsRow`: a sleep in the sleep colour,
    /// a breast or bottle in the feed colour.
    var partsRowItem: PartsRow.Item {
        PartsRow.Item(icon: icon, tint: icon == .sleep ? Palette.sleep : Palette.feed,
                      text: HomeCopy.partTime(seconds), accessibilityLabel: HomeCopy.spokenPart(self))
    }
}
