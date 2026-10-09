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

    /// When the start can be edited, the start time is drawn as a tinted pill
    /// with a pencil, so the hero's tap target is visible. The whole hero
    /// stays the button; the pill is its cue.
    @ViewBuilder
    private func sinceLabel(_ since: Date, tint: Color) -> some View {
        if onEdit != nil {
            HStack(spacing: 5) {
                Text(HomeCopy.since(since))
                Image(systemName: "pencil")
                    .imageScale(.small)
            }
            .font(.footnote.weight(.semibold))
            .foregroundStyle(tint)
            .padding(.horizontal, 12)
            .padding(.vertical, 6)
            .background(tint.opacity(0.14), in: Capsule())
            .overlay(Capsule().strokeBorder(tint.opacity(0.25), lineWidth: 1))
            .padding(.top, 6)
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
                PartsRow(parts: parts)
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

/// The finished parts of a session: "L 9:02  R 4:10".
struct PartsRow: View {
    let parts: [PartSummary]
    var iconSize: CGFloat = 16

    var body: some View {
        HStack(spacing: 12) {
            ForEach(Array(parts.enumerated()), id: \.offset) { _, part in
                HStack(spacing: 4) {
                    BabyIconView(part.icon, size: iconSize)
                        .foregroundStyle(part.icon == .sleep ? Palette.sleep : Palette.feed)
                    Text(HomeCopy.partTime(part.seconds))
                        .font(.footnote.weight(.semibold).monospacedDigit())
                        .foregroundStyle(Palette.primaryText)
                        // A time is never broken across lines: in the join
                        // chip, two parts wrapped "9:02" into "9:0" over "2".
                        .lineLimit(1)
                        .fixedSize()
                }
            }
        }
    }
}
