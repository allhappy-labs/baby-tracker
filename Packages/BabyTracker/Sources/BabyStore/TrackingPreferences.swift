import Foundation
import BabyCore

/// What a session is made of, for the purpose of deciding whether a new
/// part joins the one before it rather than starting a session of its own.
public enum SessionKind: Hashable, Sendable {
    case feed, sleep
}

/// How long a gap between parts may last before the next one starts a new
/// session instead of joining the one before it.
public struct BreakLimits: Equatable, Sendable {
    public var feed: TimeInterval
    public var sleep: TimeInterval

    public init(feed: TimeInterval, sleep: TimeInterval) {
        self.feed = feed
        self.sleep = sleep
    }

    public static let standard = BreakLimits(feed: 1_800, sleep: 900)

    public func seconds(for kind: SessionKind) -> TimeInterval {
        kind == .feed ? feed : sleep
    }
}

/// How this phone tracks: the break limits, and the order of each baby's Home
/// buttons.
///
/// Per device, in the App Group: a widget or an intent decides whether a tap
/// joins or starts fresh using the same limits the app shows in Settings,
/// without round-tripping through sync.
public struct TrackingPreferences {
    public static let feedKey = "tracking.feedBreakMinutes"
    public static let sleepKey = "tracking.sleepBreakMinutes"
    public static let range = 5...120

    private let defaults: UserDefaults

    public init(defaults: UserDefaults? = nil) {
        self.defaults = defaults
            ?? UserDefaults(suiteName: BabySchema.appGroupID)
            ?? .standard
    }

    /// How long a gap may last before the next feed part starts a new feed.
    /// Clamped to 5...120 and rounded to a multiple of 5 on every write, so a
    /// value written by a future version with a wider range cannot be read
    /// back as something this version's stepper cannot represent.
    public var feedMinutes: Int {
        get { read(Self.feedKey, fallback: 30) }
        nonmutating set { defaults.set(Self.clamp(newValue), forKey: Self.feedKey) }
    }

    /// How long a gap may last before the next sleep part starts a new sleep.
    public var sleepMinutes: Int {
        get { read(Self.sleepKey, fallback: 15) }
        nonmutating set { defaults.set(Self.clamp(newValue), forKey: Self.sleepKey) }
    }

    public var limits: BreakLimits {
        BreakLimits(feed: TimeInterval(feedMinutes * 60), sleep: TimeInterval(sleepMinutes * 60))
    }

    // MARK: - Home buttons

    public static func homeOrderKey(for babyID: UUID) -> String {
        "tracking.homeOrder.\(babyID.uuidString)"
    }

    /// The order of Home's actions for one baby, as this phone arranged it.
    /// Always complete — see `HomeActionKind.order(fromStored:)`.
    public func homeOrder(for babyID: UUID) -> [HomeActionKind] {
        HomeActionKind.order(fromStored: defaults.stringArray(forKey: Self.homeOrderKey(for: babyID)) ?? [])
    }

    /// The default order is stored as nothing at all, so a baby left at the
    /// default follows whatever the default becomes.
    public func setHomeOrder(_ order: [HomeActionKind], for babyID: UUID) {
        let complete = HomeLayout(order: order).order
        guard complete != HomeActionKind.defaultOrder else { return resetHomeOrder(for: babyID) }
        defaults.set(complete.map(\.rawValue), forKey: Self.homeOrderKey(for: babyID))
    }

    public func resetHomeOrder(for babyID: UUID) {
        defaults.removeObject(forKey: Self.homeOrderKey(for: babyID))
    }

    static func clamp(_ minutes: Int) -> Int {
        let bounded = min(max(minutes, range.lowerBound), range.upperBound)
        return bounded - bounded % 5
    }

    private func read(_ key: String, fallback: Int) -> Int {
        defaults.object(forKey: key) == nil ? fallback : Self.clamp(defaults.integer(forKey: key))
    }
}
