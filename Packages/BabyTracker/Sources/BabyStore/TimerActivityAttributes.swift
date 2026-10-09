import Foundation
import BabyCore
import BabyUI
import BabyProtocol

/// The component currently represented by a running timer.
///
/// Kept independent of ActivityKit so the mapping remains testable on macOS,
/// where the package suite runs. Labels and symbols travel from this one
/// mapping to both Home and the Live Activity.
public enum FeedActivityKind: String, Codable, Hashable, Sendable {
    case breastLeft
    case breastRight
    case breastMilkBottle
    case formulaBottle
    case mixedBottle
    case timedBottle
    case paused
    case sleep

    public var label: String {
        switch self {
        case .breastLeft: String(localized: "Feeding · Left breast", bundle: .module)
        case .breastRight: String(localized: "Feeding · Right breast", bundle: .module)
        case .breastMilkBottle: String(localized: "Feeding · Breast milk bottle", bundle: .module)
        case .formulaBottle: String(localized: "Feeding · Formula bottle", bundle: .module)
        case .mixedBottle: String(localized: "Feeding · Mixed bottle", bundle: .module)
        case .timedBottle: String(localized: "Feeding · Bottle", bundle: .module)
        case .paused: String(localized: "Feeding paused", bundle: .module)
        case .sleep: String(localized: "Asleep", bundle: .module)
        }
    }

    /// Breast, bottle or sleep at a glance — the compact Dynamic Island shows
    /// this and nothing else to say what is running. A breast wears its side
    /// letter; every bottle wears a bottle, whatever is in it (the label says
    /// what). `.paused` wears the pause glyph a feed break shows.
    public var icon: BabyIcon {
        switch self {
        case .breastLeft: .breastLeft
        case .breastRight: .breastRight
        case .breastMilkBottle, .formulaBottle, .mixedBottle, .timedBottle: .bottle
        case .paused: .pause
        case .sleep: .sleep
        }
    }

    public static func resolve(
        kind: TimerKind,
        currentSide: BreastSide?,
        completedComponents: [FeedComponent],
        isBottleRunning: Bool = false
    ) -> Self {
        if kind == .sleep { return .sleep }
        if isBottleRunning { return .timedBottle }
        if let currentSide {
            return currentSide == .left ? .breastLeft : .breastRight
        }
        guard let latest = completedComponents.last else { return .paused }
        switch latest {
        case .breast:
            return .paused
        case .bottle(_, _, let contents, _, _, _):
            switch contents {
            case .breastMilk: return .breastMilkBottle
            case .formula: return .formulaBottle
            case .mixed: return .mixedBottle
            }
        case .solids:
            return .paused
        }
    }
}

/// Which state a Live Activity is showing.
///
/// A feed card does not vanish at Stop: it stays up through the break that
/// follows, offering the next part, until the break limit passes or the next
/// part starts — exactly what Home shows over the same span. A sleep card
/// goes at Wake up.
public enum ActivityPhase: String, Codable, Sendable {
    /// A feed part or a sleep is running: elapsed time and Stop or Wake up.
    case running
    /// A feed part just stopped: break time, the parts so far, and L / R /
    /// Bottle.
    case feedBreak
    /// Retired: the time-awake card an earlier build showed after a sleep.
    /// Kept so that build's last pushed state still decodes; nothing pushes
    /// it, and the app takes such a card down the next time it is opened.
    case sleepBreak
}

/// One finished part on the break card: which breast, or nil for a bottle,
/// and how long it ran.
public struct PartLabel: Codable, Hashable, Sendable {
    /// `"left"`, `"right"`, or nil for a bottle. A raw string rather than
    /// `BreastSide` so the card's payload stays a plain wire shape.
    public let side: String?
    public let seconds: Int

    public init(side: String?, seconds: Int) {
        self.side = side
        self.seconds = seconds
    }
}

/// Everything a Live Activity shows, derived in one place.
///
/// Platform-neutral on purpose. The ActivityKit state type only exists on iOS,
/// so when `start` and `update` each built it inline, the rule deciding what
/// the Lock Screen says was untestable in the package suite — and was written
/// out twice, free to drift. Every push now builds its content from this.
public struct ActivityPresentation: Sendable, Equatable {
    /// The instant the card's clock counts up from: the running part's start,
    /// or — in a break — the end of the last part, which is the break's start.
    public let startedAt: Date
    public let sideLabel: String
    public let activityKind: FeedActivityKind
    public let phase: ActivityPhase
    /// When the break's buttons stop meaning anything: the last part's end
    /// plus the break limit. Nil while running.
    public let breakEndsAt: Date?
    /// The finished parts of the feed in its break, in session order.
    public let parts: [PartLabel]
    /// `"left"` or `"right"`: the side the break card outlines.
    public let suggestedSide: String?

    init(
        startedAt: Date,
        sideLabel: String,
        activityKind: FeedActivityKind,
        phase: ActivityPhase,
        breakEndsAt: Date?,
        parts: [PartLabel],
        suggestedSide: String?
    ) {
        self.startedAt = startedAt
        self.sideLabel = sideLabel
        self.activityKind = activityKind
        self.phase = phase
        self.breakEndsAt = breakEndsAt
        self.parts = parts
        self.suggestedSide = suggestedSide
    }

    /// A running feed part or sleep. `earlierAsleepSeconds` is the time asleep
    /// in the earlier parts of the sleep it continues
    /// (`BabyStore.earlierAsleepSeconds(of:)`): the clock counts the whole
    /// sleep, as Home's hero does.
    @MainActor
    public init(timer: ActiveTimer, earlierAsleepSeconds: Int = 0) {
        let kind = FeedActivityKind.resolve(
            kind: timer.kind,
            currentSide: timer.currentSide,
            completedComponents: timer.completedComponents,
            isBottleRunning: timer.currentBottleStartedAt != nil
        )
        self.init(
            startedAt: (timer.currentBottleStartedAt ?? timer.startedAt)
                .addingTimeInterval(-TimeInterval(earlierAsleepSeconds)),
            sideLabel: kind.label,
            activityKind: kind,
            phase: .running,
            breakEndsAt: nil,
            parts: [],
            suggestedSide: nil
        )
    }

    /// A running feed part or sleep. The same as `init(timer:)`, under the
    /// name the break builders sit beside.
    @MainActor
    public static func running(timer: ActiveTimer) -> ActivityPresentation {
        ActivityPresentation(timer: timer)
    }

    /// The break after a feed part: counts from the session's end, offers
    /// the suggested side, and is actionable until `limit` has passed.
    ///
    /// `members` are the session's feeds (`HomeSnapshot.sessionMembers`),
    /// read for the parts row; without them the row is empty.
    @MainActor
    public static func feedBreak(
        session: FeedingSession,
        members: [FeedEntry] = [],
        limit: TimeInterval,
        suggested: BreastSide
    ) -> ActivityPresentation {
        let parts = Self.parts(of: members)
        // The last part's kind: the card's tint and, for a feed card from
        // an earlier build, what it was already showing. The break card
        // itself draws the feed glyph, not this.
        let lastKind: FeedActivityKind = switch parts.last?.side {
        case "left": .breastLeft
        case "right": .breastRight
        default: .timedBottle
        }
        return ActivityPresentation(
            startedAt: session.endedAt,
            sideLabel: String(localized: "Break", bundle: .module),
            activityKind: lastKind,
            phase: .feedBreak,
            breakEndsAt: session.endedAt.addingTimeInterval(limit),
            parts: parts,
            suggestedSide: suggested.rawValue
        )
    }

    /// Whether the card's buttons still mean what they say at `now`. A break
    /// that has passed its limit would start a new session, not continue
    /// this one — the card stops offering to. A gap exactly at the limit
    /// still joins, so the limit's own instant is still actionable.
    public func isActionable(now: Date) -> Bool {
        ActivityPhase.isActionable(breakEndsAt: breakEndsAt, now: now)
    }

    /// One label per milk part, in session order. Solids never join a milk
    /// session, so they have no part here. The same reading Home's parts row
    /// makes (`HomeScreen.parts(of:in:)`).
    @MainActor
    static func parts(of members: [FeedEntry]) -> [PartLabel] {
        members.flatMap { feed -> [PartLabel] in
            feed.components.compactMap { component in
                if let side = component.breastSide {
                    PartLabel(side: side.rawValue, seconds: component.durationSeconds ?? 0)
                } else if component.bottleContents != nil {
                    PartLabel(side: nil, seconds: component.bottleSeconds ?? 0)
                } else {
                    nil
                }
            }
        }
    }
}

public extension ActivityPhase {
    /// The one rule for whether a card's buttons are live — shared by the
    /// presentation the app builds and the state the widget renders.
    static func isActionable(breakEndsAt: Date?, now: Date) -> Bool {
        guard let breakEndsAt else { return true }
        return now <= breakEndsAt
    }
}

/// The Live Activity's payload, as ActivityKit encodes it.
///
/// Platform-neutral, unlike `TimerActivityAttributes`, so that decoding a
/// payload written by an earlier build is testable on macOS, where the
/// package suite runs: ActivityKit keeps the last pushed state and hands it to
/// the new build's widget after an update. Every field added after the first
/// shipped shape (`startedAt`, `sideLabel`, `activityKind`) is decoded with
/// `decodeIfPresent` and a default, and the retired `canSwitchSide` is
/// ignored.
public struct TimerActivityContentState: Codable, Hashable, Sendable {
    public let startedAt: Date
    public let sideLabel: String
    public let activityKind: FeedActivityKind
    public let phase: ActivityPhase
    public let breakEndsAt: Date?
    public let parts: [PartLabel]
    public let suggestedSide: String?
    public let omittedParts: Int
    public let remote: LiveActivityRevision?
    /// This phone may not write in the card's family, by the family's current
    /// authority at the time the app wrote the card. Set by the app on what
    /// it writes itself; a card the server pushed says the same through
    /// `remote.canWrite`. Either way the card offers no controls.
    public let readOnly: Bool

    public init(
        startedAt: Date,
        sideLabel: String,
        activityKind: FeedActivityKind,
        phase: ActivityPhase = .running,
        breakEndsAt: Date? = nil,
        parts: [PartLabel] = [],
        suggestedSide: String? = nil,
        omittedParts: Int = 0,
        remote: LiveActivityRevision? = nil,
        readOnly: Bool = false
    ) {
        self.startedAt = startedAt
        self.sideLabel = sideLabel
        self.activityKind = activityKind
        self.phase = phase
        self.breakEndsAt = breakEndsAt
        self.parts = parts
        self.suggestedSide = suggestedSide
        self.omittedParts = omittedParts
        self.remote = remote
        self.readOnly = readOnly
    }

    /// The one way the app builds this — from the shared presentation, so
    /// every push agrees about what the card says.
    ///
    /// `remote` is set only on a card the server already pushed to: the
    /// acknowledged revision the app's content was built from
    /// (`LiveActivityRevisionReader`). A card the app started itself carries
    /// none until the server's first push.
    ///
    /// `readOnly`: whether this phone may write in the card's family right
    /// now (`LiveActivityController.content`), so a viewer's token card
    /// offers no controls before the first push reaches it either.
    public init(_ presentation: ActivityPresentation, remote: LiveActivityRevision? = nil, readOnly: Bool = false) {
        self.init(
            startedAt: presentation.startedAt,
            sideLabel: presentation.sideLabel,
            activityKind: presentation.activityKind,
            phase: presentation.phase,
            breakEndsAt: presentation.breakEndsAt,
            parts: Array(presentation.parts.suffix(8)),
            suggestedSide: presentation.suggestedSide,
            omittedParts: max(0, presentation.parts.count - 8),
            remote: remote,
            readOnly: readOnly
        )
    }

    private enum CodingKeys: String, CodingKey {
        case startedAt, sideLabel, activityKind, phase, breakEndsAt, parts, suggestedSide, omittedParts, remote, readOnly
    }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        let omittedParts = try container.decodeIfPresent(Int.self, forKey: .omittedParts) ?? 0
        guard omittedParts >= 0, Int64(omittedParts) <= LiveActivityContract.maximumCounter else {
            throw DecodingError.dataCorruptedError(forKey: .omittedParts, in: container, debugDescription: "Invalid omitted part count")
        }
        self.init(
            startedAt: try container.decode(Date.self, forKey: .startedAt),
            sideLabel: try container.decode(String.self, forKey: .sideLabel),
            activityKind: try container.decode(FeedActivityKind.self, forKey: .activityKind),
            phase: try container.decodeIfPresent(ActivityPhase.self, forKey: .phase) ?? .running,
            breakEndsAt: try container.decodeIfPresent(Date.self, forKey: .breakEndsAt),
            parts: try container.decodeIfPresent([PartLabel].self, forKey: .parts) ?? [],
            suggestedSide: try container.decodeIfPresent(String.self, forKey: .suggestedSide),
            omittedParts: omittedParts,
            remote: try container.decodeIfPresent(LiveActivityRevision.self, forKey: .remote),
            readOnly: try container.decodeIfPresent(Bool.self, forKey: .readOnly) ?? false
        )
    }

    /// `readOnly` is written only when set: the server never writes it, and
    /// the wire shape of everything else stays exactly the server's.
    public func encode(to encoder: any Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(startedAt, forKey: .startedAt)
        try container.encode(sideLabel, forKey: .sideLabel)
        try container.encode(activityKind, forKey: .activityKind)
        try container.encode(phase, forKey: .phase)
        try container.encodeIfPresent(breakEndsAt, forKey: .breakEndsAt)
        try container.encode(parts, forKey: .parts)
        try container.encodeIfPresent(suggestedSide, forKey: .suggestedSide)
        try container.encode(omittedParts, forKey: .omittedParts)
        try container.encodeIfPresent(remote, forKey: .remote)
        if readOnly { try container.encode(true, forKey: .readOnly) }
    }

    /// New remote cards carry typed state rather than a server-localized label.
    /// Legacy cards retain their saved caption until they are reconciled.
    public var localizedCaption: String {
        guard remote != nil else { return sideLabel }
        switch phase {
        case .running: return activityKind.label
        case .feedBreak: return String(localized: "Break", bundle: .module)
        case .sleepBreak: return String(localized: "Awake", bundle: .module)
        }
    }

    public func isActionable(now: Date) -> Bool {
        ActivityPhase.isActionable(breakEndsAt: breakEndsAt, now: now)
    }

    /// What the card's caption says in each phase: the typed, on-device
    /// label for a pushed card; for an older card its saved running label.
    public var lockScreenCaption: String {
        switch phase {
        case .running: localizedCaption
        case .feedBreak: String(localized: "Break", bundle: .module)
        case .sleepBreak: String(localized: "Awake", bundle: .module)
        }
    }

    /// A viewer's card: it shows the feed or sleep but offers nothing to tap
    /// — whether the server pushed it (`remote.canWrite`) or this phone wrote
    /// it while the family's current authority said viewer (`readOnly`). A
    /// card with neither keeps today's controls.
    public var isReadOnly: Bool { readOnly || remote?.canWrite == false }

    /// Whether the card shows its buttons at `now`: a part runs or a break is
    /// still open, the card is not an earlier build's time-awake card, and
    /// whoever sees it may write.
    public func offersControls(now: Date) -> Bool {
        phase != .sleepBreak && !isReadOnly && isActionable(now: now)
    }

    /// How many earlier parts the card leaves out: those the payload omitted,
    /// plus the oldest it drops to fit the width.
    public func earlierParts(droppingForWidth dropped: Int) -> Int {
        omittedParts + max(0, min(dropped, parts.count))
    }

    /// "+3" ahead of the parts row; nil when nothing is left out.
    public static func earlierPartsBadge(_ count: Int) -> String? {
        count > 0 ? "+\(count)" : nil
    }

    /// What VoiceOver says for that badge.
    public static func earlierPartsAccessibilityLabel(_ count: Int) -> String {
        String(localized: "Earlier parts: \(count)", bundle: .module)
    }
}

// ActivityKit ships in the macOS SDK but every symbol in it is
// `@available(macOS, unavailable)`, so `canImport` is not a usable guard here.
// The package targets macOS so the 1000+ tests can run without a simulator,
// hence `os(iOS)`.
#if os(iOS)
import ActivityKit

/// The shape of the Lock Screen / Dynamic Island payload for a feed or sleep.
/// Lives in `BabyStore` rather than in either app target because both the app
/// (which requests and ends activities) and the widget extension (which
/// renders them) need it, and they share only the package.
public struct TimerActivityAttributes: ActivityAttributes {
    /// See `TimerActivityContentState`: the payload is platform-neutral so its
    /// decoding of an earlier build's JSON is tested on macOS.
    public typealias ContentState = TimerActivityContentState

    /// The baby whose timer this activity is for.
    ///
    /// Carried explicitly because the buttons run long after the app last
    /// had focus: without it the intent can only ask which baby is
    /// *selected*, and with two babies that is routinely the wrong one.
    public let babyID: UUID
    public let babyName: String
    public let kindRaw: String
    public let scope: LiveActivityScope?

    public init(babyID: UUID, babyName: String, kindRaw: String, scope: LiveActivityScope? = nil) {
        self.babyID = babyID
        self.babyName = babyName
        self.kindRaw = kindRaw
        self.scope = scope
    }
}
#endif
