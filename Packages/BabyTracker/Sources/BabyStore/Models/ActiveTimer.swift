import Foundation
import SwiftData
import BabyCore

/// A running timer is data, not view state. This is what makes a feed survive
/// a force-quit, a reboot, and the Live Activity.
@Model
public final class ActiveTimer {
    public var id: UUID = UUID()
    public var realmKey: String = RealmID.local.encodedKey
    public var babyID: UUID = UUID()
    public var kindRaw: String = TimerKind.feed.rawValue
    public var startedAt: Date = Date()
    public var timeZoneID: String = "UTC"
    public var liveActivityID: String?

    /// Parts completed before the current one, for feed timers.
    public var completedComponents: [FeedComponent] = []
    /// Side currently running, for feed timers.
    public var currentSideRaw: String?
    /// When the current side started. Differs from startedAt after a switch.
    public var currentSideStartedAt: Date = Date()
    /// The active bottle interval, if any. Mutually exclusive with currentSide.
    /// Nil on older timers and after a bottle is finished.
    public var currentBottleStartedAt: Date?

    /// The feed or sleep session this part continues; nil means it starts its
    /// own. Synced as `session_id`.
    public var sessionID: UUID?
    /// Local only: what the running bottle holds, chosen in the Bottle sheet.
    public var bottleContentsRaw: String?

    public init(
        id: UUID = UUID(),
        babyID: UUID,
        kind: TimerKind,
        startedAt: Date,
        timeZoneID: String,
        currentSide: BreastSide? = nil,
        realm: RealmID = .local
    ) {
        self.id = id
        self.realmKey = realm.encodedKey
        self.babyID = babyID
        self.kindRaw = kind.rawValue
        self.startedAt = startedAt
        self.currentSideStartedAt = startedAt
        self.timeZoneID = timeZoneID
        self.currentSideRaw = currentSide?.rawValue
    }

    public var realmID: RealmID { RealmID(encodedKey: realmKey) ?? .local }

    public var kind: TimerKind {
        get { TimerKind(rawValue: kindRaw) ?? .feed }
        set { kindRaw = newValue.rawValue }
    }

    public var currentSide: BreastSide? {
        get { currentSideRaw.flatMap(BreastSide.init(rawValue:)) }
        set { currentSideRaw = newValue?.rawValue }
    }

    public var currentBreastSide: BreastSide? {
        get { currentSide }
        set { currentSide = newValue }
    }

    public var bottleContents: BottleContents? {
        get { bottleContentsRaw.flatMap(BottleContents.init(rawValue:)) }
        set { bottleContentsRaw = newValue?.rawValue }
    }

    /// The one phrasing of "what is running", shown identically on Home and
    /// on the Lock Screen. Defined here so the two can never drift.
    ///
    /// Cased correctly at the source, not by whoever renders it. Home's dial
    /// uppercases the whole string, so the raw `currentSide` looked fine
    /// there; the Live Activity does not, and it put "Feeding · left" on the
    /// Lock Screen — the one surface a user reads without unlocking.
    public var sideLabel: String {
        FeedActivityKind.resolve(
            kind: kind,
            currentSide: currentSide,
            completedComponents: completedComponents,
            isBottleRunning: currentBottleStartedAt != nil
        ).label
    }

    /// Always computed from stored timestamps, never accumulated in memory.
    public func elapsedSeconds(now: Date) -> Int {
        max(0, Int(now.timeIntervalSince(startedAt)))
    }

    public func currentSideSeconds(now: Date) -> Int {
        max(0, Int(now.timeIntervalSince(currentSideStartedAt)))
    }

    public func currentBottleSeconds(now: Date) -> Int? {
        currentBottleStartedAt.map { max(0, Int(now.timeIntervalSince($0))) }
    }
}
