import Foundation
import Observation
import BabyCore
import BabyStore
import BabyUI

/// What a Stop did. A running bottle cannot be stopped without an amount, so
/// the view asks for one instead.
public enum StopResult: Equatable {
    case stopped
    case needsBottleAmount(timerID: UUID)
}

@MainActor
@Observable
public final class HomeViewModel {
    private let store: BabyStore
    private let babyID: UUID
    /// A preference, not a fact about the entries: it can change while this
    /// screen is alive. Only the running-timer editor formats a volume here.
    public var volumeUnit: VolumeUnit
    /// A preference, like `volumeUnit`: the order of this baby's Home
    /// buttons can change while this screen is alive.
    public var homeOrder: [HomeActionKind]

    /// The last snapshot read. Not observed: the view draws `screen`, which
    /// only changes when what is on screen does.
    @ObservationIgnored public private(set) var snapshot: HomeSnapshot?

    /// What Home draws, re-derived by `refresh()`.
    public private(set) var screen: HomeScreen = .empty

    @ObservationIgnored private var suggestedSide: BreastSide = .left
    @ObservationIgnored private var canLog = true

    public init(store: BabyStore, babyID: UUID, volumeUnit: VolumeUnit = .millilitres,
                homeOrder: [HomeActionKind] = HomeActionKind.defaultOrder) {
        self.store = store
        self.babyID = babyID
        self.volumeUnit = volumeUnit
        self.homeOrder = homeOrder
    }

    /// Reads the store again. Runs only when the data may have changed — a
    /// write, a return to the foreground — and once a minute while nothing
    /// runs (a break closing, the day rolling over). Never per second: every
    /// number that ticks is drawn by a timeline over the dates on `screen`.
    public func refresh() throws {
        snapshot = try store.homeSnapshot(for: babyID)
        suggestedSide = try store.suggestedSide(for: babyID)
        canLog = store.canWriteInActiveRealm
        let next = screen(at: store.now)
        // `@Observable` does not dedupe by equality, so assigning an identical
        // value would still redraw every view that read it.
        if next != screen { screen = next }
    }

    public func screen(at now: Date) -> HomeScreen {
        guard let snapshot else { return canLog ? .empty : .emptyReadOnly }
        return HomeScreen.make(snapshot: snapshot, suggested: suggestedSide, canLog: canLog,
                               layout: HomeLayout(order: homeOrder), now: now)
    }

    /// Runs a timer verb once the store holds the family's current state.
    ///
    /// `catchUp` waits for the foreground sync. If it changed what the tap
    /// was aimed at — the other phone stopped this timer, or started another
    /// — the tap is dropped and Home shows what the family has: acting on
    /// the old screen stopped the other parent's real sleep on 2026-11-01.
    /// Returns whether `work` ran.
    @discardableResult
    public func onCurrentState(catchUp: (() async -> Void)?, _ work: () async throws -> Void) async throws -> Bool {
        if let catchUp {
            let aimed = Aim(screen)
            await catchUp()
            try refresh()
            guard Aim(screen) == aimed else { return false }
        }
        try await work()
        return true
    }

    /// What a timer tap acts on: the running or paused timer and the pump.
    private struct Aim: Equatable {
        let hero: Hero
        let dock: Dock
        let pump: RunningPump?
        init(_ screen: HomeScreen) { (hero, dock, pump) = (screen.hero, screen.dock, screen.pump) }
    }

    // MARK: - Verbs
    //
    // The view calls one of these and then `refresh()`. The store write is
    // synchronous and has happened by the time the await (for the Live
    // Activity) resumes.

    /// Starts a breast part. A part already running is stopped at this
    /// instant first — the store's safety net, not a Switch control. The
    /// feed's card, running or in its break, carries on as the new part's.
    public func startBreast(_ side: BreastSide) async throws {
        let runningID = try store.activeTimer(kind: .feed, for: babyID)?.id
        let timer = try store.startPart(.breast(side), for: babyID)
        guard timer.id != runningID else { return }
        await LiveActivityController.adoptOrStart(for: timer, in: store)
    }

    public func startSleep() async throws {
        guard try store.activeTimer(kind: .sleep, for: babyID) == nil else { return }
        let timer = try store.startSleep(for: babyID)
        await LiveActivityController.adoptOrStart(for: timer, in: store)
    }

    /// Stops the running feed part. A running bottle needs its amount first,
    /// so it is left running and the caller asks for one.
    public func stopFeed() async throws -> StopResult {
        guard let timer = try store.activeTimer(kind: .feed, for: babyID) else { return .stopped }
        if timer.currentBottleStartedAt != nil { return .needsBottleAmount(timerID: timer.id) }
        try await LiveActivityController.stopping(timer, in: store) { _ = try store.stopFeed(timer) }
        return .stopped
    }

    public func stopSleep() async throws {
        guard let timer = try store.activeTimer(kind: .sleep, for: babyID) else { return }
        try await LiveActivityController.stopping(timer, in: store) { _ = try store.stopSleep(timer) }
    }

    /// The strip's Stop: ends the family's running pump now and names it, so
    /// the app can ask How much?. Nil when none runs (the other phone stopped
    /// it a moment ago).
    public func stopPump() async throws -> UUID? {
        guard let pump = try store.runningPump() else { return nil }
        try store.stopPump(pump)
        await PumpActivityController.reconcile(in: store)
        return pump.id
    }

    /// The join chip's ✕: the running part starts a session of its own.
    public func detachRunning(_ kind: SessionKind) async throws {
        guard let timer = try store.activeTimer(kind: kind == .feed ? .feed : .sleep, for: babyID) else { return }
        try store.detachFromSession(timer)
        await LiveActivityController.update(for: timer, in: store, own: true)
    }

    /// The editor for a running timer — the hero's when `timerID` is nil — or
    /// nil when there is none or this caregiver may not write: the store would
    /// refuse the save, and an editor that opens only to fail is worse than a
    /// hero that does not open.
    public func makeTimerEditor(timerID: UUID? = nil) -> RunningTimerEditModel? {
        guard store.canWriteInActiveRealm,
              let timers = try? store.activeTimers(for: babyID) else { return nil }
        let timer: ActiveTimer? = if let timerID {
            timers.first { $0.id == timerID }
        } else {
            // The hero's timer: a running feed outranks a running sleep.
            timers.first { $0.kind == .feed } ?? timers.first
        }
        return timer.map { RunningTimerEditModel(timer: $0, store: store, unit: volumeUnit) }
    }
}

extension HomeScreen {
    /// Before the first read: an empty Home, never a stale one.
    static let empty = HomeScreen(hero: .awake(since: nil), status: [], strip: nil,
                                  dock: .start(suggested: .left, big: .breast, tiles: HomeLayout().tiles), joinChip: nil, sleepChip: nil)
    static let emptyReadOnly = HomeScreen(hero: .awake(since: nil), status: [], strip: nil,
                                          dock: .readOnly, joinChip: nil, sleepChip: nil)
}
