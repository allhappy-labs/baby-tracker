import ActivityKit
import AppIntents
import WidgetKit
import SwiftUI
import BabyCore
import BabyStore
import BabyUI

/// Lock Screen and Dynamic Island presentation of a feed or a sleep. It
/// mirrors Home:
///
/// | Phase         | Shows                                     | Controls                          |
/// | ------------- | ----------------------------------------- | --------------------------------- |
/// | running feed  | part glyph, elapsed                       | Stop                              |
/// | feed break    | pause glyph, Break, time since, parts     | L, R (suggested outlined), Bottle |
/// | running sleep | moon, elapsed                             | Wake up                           |
///
/// A sleep card goes at Wake up; there is no time-awake card.
///
/// A card the server pushed is captioned from its typed phase and kind,
/// localized here, never from the server's words; an older card keeps its
/// saved label. A viewer's pushed card shows everything but its controls.
///
/// A break card's buttons go once its limit passes (`staleDate`, set to the
/// break's end, flips `context.isStale`): from then on the next part starts a
/// session of its own, which the card has no business offering. The app takes
/// the card down the next time it is opened.
///
/// Every clock is `Text(_:style: .timer)`: rendered by the system, counting
/// up from a past date with the app not running.
///
/// Every button is a `LiveActivityIntent` without `openAppWhenRun`, so it runs
/// on a locked iPhone — see `LogIntents.swift`. A card with a realm uses the
/// card intents instead (`LiveActivityCardIntents.swift`), each carrying the
/// card's own target (`LiveActivityActionTarget`): the intent checks it
/// against the family's current state before writing anything.
struct FeedActivityWidget: Widget {
    var body: some WidgetConfiguration {
        ActivityConfiguration(for: TimerActivityAttributes.self) { context in
            let live = isLive(context)
            VStack(alignment: .leading, spacing: Spacing.m) {
                HStack(spacing: Spacing.m) {
                    icon(for: context.state)
                        .font(.title2)

                    VStack(alignment: .leading, spacing: 2) {
                        Text(caption(for: context.state))
                            .font(.caption.weight(.semibold))
                            .foregroundStyle(Palette.secondaryText)
                        if live {
                            Text(context.state.startedAt, style: .timer)
                                .font(.title.weight(.bold).monospacedDigit())
                                .foregroundStyle(Palette.primaryText)
                        }
                    }

                    Spacer(minLength: 0)
                }

                // Its own line, under the clock: beside it, three parts left
                // each time a thumb's width and wrapped "4:28" as "4:2 / 8".
                partsRow(context.state)

                if offersControls(context) {
                    controls(context)
                }
            }
            .padding()
            .foregroundStyle(Palette.primaryText)
            .activityBackgroundTint(Palette.ground)
            .activitySystemActionForegroundColor(Palette.primaryText)
        } dynamicIsland: { context in
            DynamicIsland {
                DynamicIslandExpandedRegion(.leading) {
                    icon(for: context.state)
                }
                // The caption sits in the CENTRE, above the timer, not in the
                // trailing region. The trailing region of an expanded island
                // is about a thumb wide and rendered "Feeding · Right" as
                // "Fe / e / …" — three lines of nothing.
                DynamicIslandExpandedRegion(.center) {
                    VStack(spacing: 1) {
                        Text(caption(for: context.state))
                            .font(.caption.weight(.semibold))
                            .foregroundStyle(Palette.secondaryText)
                        if isLive(context) {
                            Text(context.state.startedAt, style: .timer)
                                .font(.title2.weight(.bold).monospacedDigit())
                                .foregroundStyle(Palette.primaryText)
                                .multilineTextAlignment(.center)
                        }
                    }
                }
                DynamicIslandExpandedRegion(.bottom) {
                    VStack(spacing: 8) {
                        partsRow(context.state)
                        if offersControls(context) {
                            controls(context)
                        }
                    }
                }
            } compactLeading: {
                // The only content in this slot: nothing else in the compact
                // presentation names what is running, so the glyph —
                // decorative everywhere else it appears beside its own label
                // — needs one of its own here.
                icon(for: context.state)
                    // `icon(for:)` is a `Group`, and a modifier applied to a
                    // `Group` with one child lands on that child directly
                    // rather than creating a new element around it. Without
                    // this line, `accessibilityLabel` landed on
                    // `BabyIconView` itself, where its own
                    // `accessibilityHidden(true)` wins and drops the element.
                    // `.ignore` makes this `Group` its own single element, so
                    // the label always lands somewhere.
                    .accessibilityElement(children: .ignore)
                    .accessibilityLabel(caption(for: context.state))
            } compactTrailing: {
                if !isLive(context) {
                    // A break past its limit: a clock still counting would
                    // claim the break is going on.
                    EmptyView()
                } else if context.state.phase == .running, context.state.activityKind == .sleep {
                    // Keep the narrow island legible after an hour without
                    // waking the app to publish a new elapsed-time value.
                    Text(
                        .durationOffset(to: context.state.startedAt),
                        format: .units(
                            allowed: [.hours, .minutes],
                            width: .narrow,
                            maximumUnitCount: 2,
                            fractionalPart: .hide(rounded: .down)
                        )
                    )
                    .font(.caption2.monospacedDigit())
                    .lineLimit(1)
                    .minimumScaleFactor(0.8)
                    .foregroundStyle(Palette.primaryText)
                    .frame(maxWidth: 52)
                } else {
                    Text(context.state.startedAt, style: .timer)
                        .monospacedDigit()
                        .foregroundStyle(Palette.primaryText)
                        .frame(maxWidth: 52)
                }
            } minimal: {
                // Same reasoning as `compactLeading`.
                icon(for: context.state)
                    .accessibilityElement(children: .ignore)
                    .accessibilityLabel(caption(for: context.state))
            }
        }
    }

    /// Whether the card's clock and buttons still mean anything: always while
    /// a part runs; in a break, until its limit.
    private func isLive(_ context: ActivityViewContext<TimerActivityAttributes>) -> Bool {
        // An earlier build's time-awake card: no clock, no Sleep, until the
        // app takes it down.
        context.state.phase != .sleepBreak && !context.isStale && context.state.isActionable(now: .now)
    }

    /// Whether the buttons show: the card is live and whoever sees it may
    /// write — a viewer's pushed card has none.
    private func offersControls(_ context: ActivityViewContext<TimerActivityAttributes>) -> Bool {
        isLive(context) && !context.state.isReadOnly
    }

    /// Typed and localized on this phone for a pushed card; an older card's
    /// running caption is the label it was saved with.
    private func caption(for state: TimerActivityAttributes.ContentState) -> String {
        switch state.phase {
        case .running: state.lockScreenCaption
        case .feedBreak: ActivityCopy.feedBreak
        case .sleepBreak: ActivityCopy.awake
        }
    }

    /// One control per state, as on Home's dock.
    ///
    /// Each control picks its intent by whether the card has a realm: the
    /// card intents carry the card's target, the plain ones are the widget's.
    /// `Button(intent:)` is generic over a concrete `AppIntent`, so the two
    /// cannot meet in one `any AppIntent` — each control is written as a
    /// two-way choice (`control`).
    ///
    /// Stop and Wake up are `.plain` with the shape drawn in the label: with
    /// no style, iOS 26 draws its own capsule behind the label, and the card
    /// showed a button inside a button. The break's tiles use `.tile`.
    @ViewBuilder
    private func controls(
        _ context: ActivityViewContext<TimerActivityAttributes>
    ) -> some View {
        let babyID = context.attributes.babyID
        // What the card shows, passed explicitly: its realm and — once the
        // server has pushed to it — its lifecycle and cursor. Nil for a card
        // with no realm, whose buttons keep today's path.
        let target = LiveActivityActionTarget.card(scope: context.attributes.scope, remote: context.state.remote)?.parameterValue
        switch context.state.phase {
        case .running:
            if context.attributes.kindRaw == TimerKind.sleep.rawValue {
                control(
                    card: target.map { WakeUpCardIntent(babyID: babyID, activityTarget: $0) },
                    plain: ToggleSleepIntent(babyID: babyID)
                ) {
                    PrimaryLabel(title: ActivityCopy.wakeUp)
                }
                .buttonStyle(.plain)
            } else {
                control(
                    card: target.map { StopFeedCardIntent(babyID: babyID, activityTarget: $0) },
                    plain: StopFeedIntent(babyID: babyID)
                ) {
                    PrimaryLabel(title: ActivityCopy.stop)
                }
                .buttonStyle(.plain)
            }
        case .feedBreak:
            HStack(spacing: Spacing.s) {
                control(
                    card: target.map { StartBreastCardIntent(babyID: babyID, side: .left, activityTarget: $0) },
                    plain: StartBreastIntent(babyID: babyID, side: .left)
                ) {
                    SideLabel(side: .left)
                }
                .buttonStyle(.tile(Palette.feed, size: .widget, isActive: context.state.suggestedSide == BreastSide.left.rawValue))
                .accessibilityLabel(ActivityCopy.startBreast(.left))
                .accessibilityValue(context.state.suggestedSide == BreastSide.left.rawValue ? ActivityCopy.suggested : "")

                control(
                    card: target.map { StartBreastCardIntent(babyID: babyID, side: .right, activityTarget: $0) },
                    plain: StartBreastIntent(babyID: babyID, side: .right)
                ) {
                    SideLabel(side: .right)
                }
                .buttonStyle(.tile(Palette.feed, size: .widget, isActive: context.state.suggestedSide == BreastSide.right.rawValue))
                .accessibilityLabel(ActivityCopy.startBreast(.right))
                .accessibilityValue(context.state.suggestedSide == BreastSide.right.rawValue ? ActivityCopy.suggested : "")

                control(
                    card: target.map { StartBottleCardIntent(babyID: babyID, activityTarget: $0) },
                    plain: StartBottleIntent(babyID: babyID)
                ) {
                    BottleLabel()
                }
                .buttonStyle(.tile(Palette.feed, size: .widget))
                .accessibilityLabel(ActivityCopy.startBottle)
            }
        case .sleepBreak:
            EmptyView()
        }
    }

    /// One control: the card intent when the card has a realm (`card` is
    /// non-nil exactly then), otherwise the plain intent — the same label.
    @ViewBuilder
    private func control<Card: AppIntent, Plain: AppIntent>(
        card: Card?, plain: Plain, @ViewBuilder label: () -> some View
    ) -> some View {
        if let card {
            Button(intent: card, label: label)
        } else {
            Button(intent: plain, label: label)
        }
    }

    /// The finished parts of the feed in its break — "L 9:02 · R 4:10" — as on
    /// Home's hero, in the kit's row: every time on one line, the oldest
    /// dropped first for width, and a "+N" for every earlier part left out
    /// (those the payload omitted and those dropped).
    @ViewBuilder
    private func partsRow(_ state: TimerActivityContentState) -> some View {
        PartsRow(
            state.parts.map { part in
                PartsRow.Item(
                    icon: Self.partIcon(part),
                    tint: Palette.feed,
                    text: ActivityCopy.duration(part.seconds),
                    accessibilityLabel: ActivityCopy.part(part)
                )
            },
            size: .widget,
            earlierCount: state.omittedParts,
            earlierBadge: TimerActivityContentState.earlierPartsBadge,
            earlierLabel: TimerActivityContentState.earlierPartsAccessibilityLabel
        )
    }

    private static func partIcon(_ part: PartLabel) -> BabyIcon {
        switch part.side {
        case BreastSide.left.rawValue: .breastLeft
        case BreastSide.right.rawValue: .breastRight
        default: .bottle
        }
    }

    @ViewBuilder
    private func icon(for state: TimerActivityAttributes.ContentState) -> some View {
        Group {
            switch state.phase {
            case .feedBreak:
                // Its own glyph, not the feed drop: in the compact island the
                // icon is all there is, and a drop reads as a feed running.
                BabyIconView(.pause)
            case .sleepBreak:
                BabyIconView(.sleep)
            case .running:
                BabyIconView(state.activityKind.icon)
            }
        }
        .foregroundStyle(state.activityKind == .sleep ? Palette.sleep : Palette.feed)
    }
}

/// L or R on the feed break card: glyph and letter, inside a `.tile` button
/// (the suggested side is the active, stronger tile).
private struct SideLabel: View {
    let side: BreastSide

    var body: some View {
        let icon: BabyIcon = side == .left ? .breastLeft : .breastRight
        HStack(spacing: Spacing.s) {
            BabyIconView(icon, size: 22)
            Text(icon.letter ?? "")
                .font(.headline.weight(.bold))
                .foregroundStyle(Palette.primaryText)
        }
    }
}

/// Bottle on the feed break card: the side tiles' shape, with the bottle glyph.
private struct BottleLabel: View {
    var body: some View {
        HStack(spacing: Spacing.s) {
            BabyIconView(.bottle, size: 22)
            Text(ActivityCopy.bottle)
                .font(.headline.weight(.bold))
                .foregroundStyle(Palette.primaryText)
                .lineLimit(1)
                .minimumScaleFactor(0.6)
        }
    }
}

/// The card's words. Named rather than inline for the same reason every other
/// screen's copy is: a string baked into a `View` cannot be read by a test.
enum ActivityCopy {
    static let stop = String(localized: "Stop")
    static let wakeUp = String(localized: "Wake up")
    static let feedBreak = String(localized: "Break")
    static let awake = String(localized: "Awake")
    static let suggested = String(localized: "Suggested")
    static let bottle = String(localized: "Bottle")
    static let startBottle = String(localized: "Start bottle")

    static func startBreast(_ side: BreastSide) -> String {
        side == .left ? String(localized: "Start left breast") : String(localized: "Start right breast")
    }

    /// "9:02" — minutes and seconds, as Home's parts row reads.
    static func duration(_ seconds: Int) -> String {
        Duration.seconds(seconds).formatted(.time(pattern: .minuteSecond))
    }

    /// What VoiceOver says for one part: which, then how long.
    static func part(_ part: PartLabel) -> String {
        let name = switch part.side {
        case BreastSide.left.rawValue: String(localized: "Left breast")
        case BreastSide.right.rawValue: String(localized: "Right breast")
        default: String(localized: "Bottle")
        }
        return "\(name) \(duration(part.seconds))"
    }
}
