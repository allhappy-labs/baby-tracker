import Foundation

/// An action Home can offer. The raw values are stored
/// (`TrackingPreferences.homeOrder`), so one is never renamed.
public enum HomeActionKind: String, CaseIterable, Hashable, Sendable {
    case breast, bottle, sleep, diaper, solids, meds, growth, vaccine, pump

    /// Home as it was before the order could be changed: L and R, then
    /// Bottle, Sleep and Diaper, with the rest behind ···.
    public static let defaultOrder: [HomeActionKind] = allCases

    /// A complete order from stored raw values: the kinds this version knows,
    /// once each in their stored sequence, then every kind the stored value
    /// lacks in default order. A kind a later version adds therefore lands at
    /// the end, and a value a later version wrote still reads.
    public static func order(fromStored raw: [String]) -> [HomeActionKind] {
        var seen = Set<HomeActionKind>()
        let stored = raw.compactMap(HomeActionKind.init(rawValue:)).filter { seen.insert($0).inserted }
        return stored + defaultOrder.filter { !seen.contains($0) }
    }
}

/// What an order means on each surface: the dock, the ··· sheet and the
/// Quick Log widget. Pure, so the three never disagree.
public struct HomeLayout: Equatable, Sendable {
    /// The small tiles beside ···.
    public static let tileCount = 3

    public enum Role: Equatable, Sendable { case big, tile, overflow }

    /// A tile in a running dock. `feed` is the L / R / Bottle chooser a
    /// running sleep offers.
    public enum RunningTile: Hashable, Sendable {
        case feed
        case action(HomeActionKind)
    }

    public enum WidgetSlot: Hashable, Sendable {
        case breastLeft, breastRight
        case action(HomeActionKind)
    }

    /// Always every kind, once each.
    public let order: [HomeActionKind]

    public init(order: [HomeActionKind] = HomeActionKind.defaultOrder) {
        self.order = HomeActionKind.order(fromStored: order.map(\.rawValue))
    }

    public var big: HomeActionKind { order[0] }
    public var tiles: [HomeActionKind] { Array(order[1...Self.tileCount]) }
    /// The big action and the tiles, in order.
    public var docked: [HomeActionKind] { Array(order.prefix(Self.tileCount + 1)) }
    public var overflow: [HomeActionKind] { Array(order.dropFirst(Self.tileCount + 1)) }

    public static func role(at index: Int) -> Role {
        index == 0 ? .big : index <= tileCount ? .tile : .overflow
    }

    /// Actions with a timer of their own: none of them is a tile in a
    /// running dock, where Stop or Wake up is the one timer control.
    private static let timed: Set<HomeActionKind> = [.breast, .bottle, .sleep]

    public var tilesWhileFeedRuns: [HomeActionKind] {
        Array(docked.filter { !Self.timed.contains($0) }.prefix(Self.tileCount))
    }

    /// A baby can fall asleep at the breast, so a feed may start while the
    /// sleep keeps running — when a feed action is on the dock at all.
    public var tilesWhileSleepRuns: [RunningTile] {
        let feed: [RunningTile] = docked.contains(.breast) || docked.contains(.bottle) ? [.feed] : []
        let rest = docked.filter { !Self.timed.contains($0) }.map(RunningTile.action)
        return Array((feed + rest).prefix(Self.tileCount))
    }

    /// What the ··· sheet offers. A running feed leaves out every timed
    /// action; a running sleep leaves out only Sleep. A docked action the
    /// running dock had no tile left for comes first, so nothing is ever
    /// out of reach while a timer runs.
    public func overflow(feedRunning: Bool, sleepRunning: Bool) -> [HomeActionKind] {
        let untimed = docked.filter { !Self.timed.contains($0) }
        if feedRunning {
            let shown = tilesWhileFeedRuns
            return untimed.filter { !shown.contains($0) } + overflow.filter { !Self.timed.contains($0) }
        }
        if sleepRunning {
            let shown = tilesWhileSleepRuns
            return untimed.filter { !shown.contains(.action($0)) } + overflow.filter { $0 != .sleep }
        }
        return overflow
    }

    /// Fills `count` slots by walking the order. Breast takes two, L and R;
    /// an action that does not fit the slots left is skipped.
    public func widgetSlots(_ count: Int = 4) -> [WidgetSlot] {
        var slots: [WidgetSlot] = []
        for kind in order where slots.count < count {
            if kind == .breast {
                if slots.count + 2 <= count { slots += [.breastLeft, .breastRight] }
            } else {
                slots.append(.action(kind))
            }
        }
        return slots
    }
}
