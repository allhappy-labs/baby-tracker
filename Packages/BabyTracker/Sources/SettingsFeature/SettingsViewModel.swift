import Foundation
import Observation
import BabyCore
import BabyStore

/// Everything the settings screen shows, and the one preference it owns.
///
/// Held by the app environment and handed to the view, so the screen that
/// changes the unit and the screens that read it are looking at the same
/// object: a second instance would persist the change but nothing already on
/// screen would hear about it.
@MainActor
@Observable
public final class SettingsViewModel {
    private static let unitKey = "volumeUnit"
    private let defaults: UserDefaults
    private let tracking: TrackingPreferences

    /// The observation seam. `volumeUnit` is computed so it can write through
    /// to `UserDefaults` on every set, and `@Observable` only tracks STORED
    /// properties — so the stored one behind it is what SwiftUI actually
    /// watches. Reading `volumeUnit` reads this, which registers the
    /// dependency; writing it mutates this, which invalidates the readers.
    private var storedUnit: VolumeUnit

    /// Same seam as `storedUnit`, for the two break-limit steppers.
    private var storedFeedBreakMinutes: Int
    private var storedSleepBreakMinutes: Int

    /// The observation seam for the Home orders, as `storedUnit` is for
    /// the unit: the orders live in `TrackingPreferences`, which a view
    /// cannot watch. Reading an order reads this; writing one bumps it.
    private var homeOrderRevision = 0

    public init(defaults: UserDefaults? = nil) {
        let defaults = defaults
            ?? UserDefaults(suiteName: BabySchema.appGroupID)
            ?? .standard
        self.defaults = defaults
        let tracking = TrackingPreferences(defaults: defaults)
        self.tracking = tracking
        // An unrecognised stored value falls back rather than trapping: a
        // string written by a future version must not crash this one.
        self.storedUnit = defaults.string(forKey: Self.unitKey)
            .flatMap(VolumeUnit.init(rawValue:)) ?? .millilitres
        self.storedFeedBreakMinutes = tracking.feedMinutes
        self.storedSleepBreakMinutes = tracking.sleepMinutes
    }

    /// Stored in the App Group so it survives relaunch and is readable by the
    /// extension.
    public var volumeUnit: VolumeUnit {
        get { storedUnit }
        set {
            storedUnit = newValue
            defaults.set(newValue.rawValue, forKey: Self.unitKey)
        }
    }

    /// How long a gap may last before the next feed part starts a new feed,
    /// in minutes. Writes through to `TrackingPreferences`, which clamps to
    /// 5...120 and rounds to a multiple of 5 — the same rounding the stepper
    /// already enforces by stepping in 5s from a clamped starting value.
    public var feedBreakMinutes: Int {
        get { storedFeedBreakMinutes }
        set {
            tracking.feedMinutes = newValue
            storedFeedBreakMinutes = tracking.feedMinutes
        }
    }

    /// How long a gap may last before the next sleep part starts a new
    /// sleep, in minutes.
    public var sleepBreakMinutes: Int {
        get { storedSleepBreakMinutes }
        set {
            tracking.sleepMinutes = newValue
            storedSleepBreakMinutes = tracking.sleepMinutes
        }
    }

    // MARK: - Home buttons

    public func homeOrder(for babyID: UUID) -> [HomeActionKind] {
        _ = homeOrderRevision
        return tracking.homeOrder(for: babyID)
    }

    public func setHomeOrder(_ order: [HomeActionKind], for babyID: UUID) {
        tracking.setHomeOrder(order, for: babyID)
        homeOrderRevision += 1
    }

    /// One step up (`-1`) or down (`1`): VoiceOver's way to reorder.
    public func moveHomeAction(at index: Int, by offset: Int, for babyID: UUID) {
        var order = tracking.homeOrder(for: babyID)
        let target = index + offset
        guard order.indices.contains(index), order.indices.contains(target) else { return }
        order.swapAt(index, target)
        setHomeOrder(order, for: babyID)
    }

    /// Also what a deleted baby's order becomes: nothing.
    public func resetHomeOrder(for babyID: UUID) {
        tracking.resetHomeOrder(for: babyID)
        homeOrderRevision += 1
    }

    // MARK: - Copy

    public let title = String(localized: "Settings", bundle: .module)
    public let unitsHeader = String(localized: "Units", bundle: .module)
    public let unitLabel = String(localized: "Volume units", bundle: .module)
    public let unitFooter = String(localized: "Changes how volumes are shown. Nothing already logged is altered.", bundle: .module)

    /// One row of the unit picker. `displayName`, never `rawValue`: the stored
    /// string is a wire format, not a word anybody should read.
    public struct UnitOption: Identifiable, Sendable {
        public let value: VolumeUnit
        public let title: String
        public nonisolated var id: VolumeUnit { value }
    }

    public let babiesHeader = String(localized: "Babies", bundle: .module)
    public let babiesFooter = String(localized: "Tap a baby to correct their name or birth date.", bundle: .module)
    /// Shown once the last baby is gone. Closing Settings lands on the add
    /// screen, which is the only thing left to do.
    public let noBabiesFooter = String(localized: "Close Settings to add one.", bundle: .module)
    /// The row is a button into an edit sheet, which a chevron alone does not
    /// say out loud.
    public let babyRowHint = String(localized: "Opens name and birth date", bundle: .module)

    public var unitOptions: [UnitOption] {
        VolumeUnit.allCases.map { UnitOption(value: $0, title: $0.displayName) }
    }
}
