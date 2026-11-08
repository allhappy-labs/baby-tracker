import Testing
import Foundation
@testable import BabyCore

@Suite struct HomeLayoutTests {
    @Test func theDefaultOrderIsTodaysHome() {
        let layout = HomeLayout()
        #expect(layout.big == .breast)
        #expect(layout.tiles == [.bottle, .sleep, .diaper])
        #expect(layout.overflow == [.solids, .meds, .growth, .vaccine, .pump])
        #expect(layout.tilesWhileFeedRuns == [.diaper])
        #expect(layout.tilesWhileSleepRuns == [.feed, .action(.diaper)])
        #expect(layout.overflow(feedRunning: false, sleepRunning: false) == [.solids, .meds, .growth, .vaccine, .pump])
        #expect(layout.overflow(feedRunning: true, sleepRunning: false) == [.solids, .meds, .growth, .vaccine, .pump])
    }

    @Test func vaccineIsTheLastDefaultActionAndNeverConflictsWithATimer() {
        #expect(HomeActionKind.defaultOrder.suffix(2) == [.vaccine, .pump])
        #expect(HomeActionKind.allCases.count == 9)
        #expect(HomeActionKind.vaccine.rawValue == "vaccine")
        // An order stored by the version before this one reads with it appended.
        let before = ["breast", "bottle", "sleep", "diaper", "solids", "meds", "growth"]
        #expect(HomeActionKind.order(fromStored: before) == HomeActionKind.defaultOrder)
        // On the dock it stays put while a feed or a sleep runs.
        let docked = HomeLayout(order: [.vaccine, .breast, .bottle, .sleep, .diaper, .solids, .meds, .growth])
        #expect(docked.big == .vaccine)
        #expect(docked.tilesWhileFeedRuns.contains(.vaccine))
        #expect(docked.tilesWhileSleepRuns.contains(.action(.vaccine)))
        // In More it stays available while either runs.
        #expect(HomeLayout().overflow(feedRunning: true, sleepRunning: false).contains(.vaccine))
        #expect(HomeLayout().overflow(feedRunning: false, sleepRunning: true).contains(.vaccine))
    }

    @Test func storedOrdersAreCompletedAndCleaned() {
        #expect(HomeActionKind.order(fromStored: []) == HomeActionKind.defaultOrder)
        // "bath" is a kind a later version may write; "bottle" is repeated.
        #expect(HomeActionKind.order(fromStored: ["bottle", "bath", "bottle", "solids"])
                == [.bottle, .solids, .breast, .sleep, .diaper, .meds, .growth, .vaccine, .pump])
    }

    @Test func anIncompleteOrderHandedToALayoutIsCompleted() {
        #expect(HomeLayout(order: [.solids]).order == [.solids, .breast, .bottle, .sleep, .diaper, .meds, .growth, .vaccine, .pump])
    }

    @Test func aNonBreastActionCanBeBig() {
        let layout = HomeLayout(order: [.bottle, .breast, .solids, .diaper, .sleep, .meds, .growth])
        #expect(layout.big == .bottle)
        #expect(layout.tiles == [.breast, .solids, .diaper])
        #expect(layout.overflow == [.sleep, .meds, .growth, .vaccine, .pump])
    }

    @Test func runningTilesDropWhatConflicts() {
        let layout = HomeLayout(order: [.solids, .diaper, .meds, .growth, .breast, .bottle, .sleep])
        #expect(layout.tilesWhileFeedRuns == [.solids, .diaper, .meds])
        // No feed action is docked, so no Feed tile; three tiles at most.
        #expect(layout.tilesWhileSleepRuns == [.action(.solids), .action(.diaper), .action(.meds)])
        // Growth lost its tile to the three-tile cap, so More carries it.
        #expect(layout.overflow(feedRunning: true, sleepRunning: false) == [.growth, .vaccine, .pump])
        #expect(layout.overflow(feedRunning: false, sleepRunning: true) == [.growth, .breast, .bottle, .vaccine, .pump])
    }

    @Test func theFeedTileLeadsWhileSleepRunsWhenAFeedActionIsDocked() {
        let layout = HomeLayout(order: [.bottle, .solids, .diaper, .growth, .breast, .sleep, .meds])
        #expect(layout.tilesWhileSleepRuns == [.feed, .action(.solids), .action(.diaper)])
    }

    @Test func rolesFollowPosition() {
        #expect(HomeLayout.role(at: 0) == .big)
        #expect(HomeLayout.role(at: 1) == .tile)
        #expect(HomeLayout.role(at: 3) == .tile)
        #expect(HomeLayout.role(at: 4) == .overflow)
        #expect(HomeLayout.role(at: 6) == .overflow)
        #expect(HomeLayout.role(at: 7) == .overflow)
        #expect(HomeLayout.role(at: 8) == .overflow)
    }

    @Test func widgetSlotsWalkTheOrderAndBreastTakesTwo() {
        #expect(HomeLayout().widgetSlots() == [.breastLeft, .breastRight, .action(.bottle), .action(.sleep)])
        #expect(HomeLayout(order: [.bottle, .breast, .sleep, .diaper, .solids, .meds, .growth]).widgetSlots()
                == [.action(.bottle), .breastLeft, .breastRight, .action(.sleep)])
        // Breast does not fit the one slot left, so it is skipped.
        #expect(HomeLayout(order: [.sleep, .diaper, .bottle, .breast, .solids, .meds, .growth]).widgetSlots()
                == [.action(.sleep), .action(.diaper), .action(.bottle), .action(.solids)])
    }

    /// Whatever the order and whatever runs, every action that does not
    /// conflict with the running timer is on the dock or in the ··· sheet.
    @Test func nothingIsUnreachableWhileATimerRuns() {
        func permutations(_ items: [HomeActionKind]) -> [[HomeActionKind]] {
            guard items.count > 1 else { return [items] }
            return items.indices.flatMap { i -> [[HomeActionKind]] in
                var rest = items
                let head = rest.remove(at: i)
                return permutations(rest).map { [head] + $0 }
            }
        }
        let untimed: Set<HomeActionKind> = [.diaper, .solids, .meds, .growth]
        var unreachable: [String] = []
        for order in permutations(HomeActionKind.allCases) {
            let layout = HomeLayout(order: order)
            let feed = Set(layout.tilesWhileFeedRuns).union(layout.overflow(feedRunning: true, sleepRunning: false))
            if !untimed.isSubset(of: feed) { unreachable.append("feed \(order)") }

            let tiles = layout.tilesWhileSleepRuns
            var sleep = Set(layout.overflow(feedRunning: false, sleepRunning: true))
            for tile in tiles {
                switch tile {
                case .feed: sleep.formUnion([.breast, .bottle])
                case .action(let kind): sleep.insert(kind)
                }
            }
            if !untimed.union([.breast, .bottle]).isSubset(of: sleep) { unreachable.append("sleep \(order)") }
        }
        #expect(unreachable.isEmpty, "\(unreachable.count) orders hide an action, e.g. \(unreachable.first ?? "")")
    }

    @Test func pumpIsTheLastDefaultActionAndRunsBesideATimer() {
        #expect(HomeActionKind.defaultOrder.last == .pump)
        #expect(HomeActionKind.pump.rawValue == "pump")
        // The default dock is as it was: Pump is in More.
        #expect(HomeLayout().docked == [.breast, .bottle, .sleep, .diaper])
        #expect(HomeLayout().overflow.last == .pump)
        // An order a caregiver arranged before this version reads with Pump
        // appended, the dock they chose untouched.
        let before = ["bottle", "breast", "sleep", "diaper", "solids", "meds", "growth", "vaccine"]
        let stored = HomeLayout(order: HomeActionKind.order(fromStored: before))
        #expect(stored.docked == [.bottle, .breast, .sleep, .diaper])
        #expect(stored.overflow == [.solids, .meds, .growth, .vaccine, .pump])
        // Pumping runs beside a feed or a sleep: on the dock it keeps its tile…
        let docked = HomeLayout(order: [.breast, .pump, .bottle, .sleep])
        #expect(docked.tilesWhileFeedRuns.contains(.pump))
        #expect(docked.tilesWhileSleepRuns.contains(.action(.pump)))
        // …and in More it stays offered while either runs.
        #expect(HomeLayout().overflow(feedRunning: true, sleepRunning: false).contains(.pump))
        #expect(HomeLayout().overflow(feedRunning: false, sleepRunning: true).contains(.pump))
        // A widget slot for it is an ordinary action slot: a link into the app.
        #expect(HomeLayout(order: [.pump, .bottle, .sleep, .diaper]).widgetSlots().first == .action(.pump))
    }
}
