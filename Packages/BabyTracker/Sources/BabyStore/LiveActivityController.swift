import Foundation
import BabyCore

/// Decides what every Live Activity card shows, and when one goes.
///
/// Every call goes through `LiveCards.board`: ActivityKit on an iPhone, a
/// no-op off iOS, and `MemoryCardBoard` in the tests that check sequences.
/// Call sites never need a `#if`.
///
/// Nothing here is load-bearing for correctness: a running timer is persisted
/// data, so it keeps counting whether or not a card exists. Every entry point
/// fails silently by design — and logs.
///
/// Three regimes, from `LiveCards.pushState` (see `Regime`):
///
/// - **Withdrawn** — local tracking, a self-hosted server, a server without
///   push, a sign-out: every entry point behaves exactly as it always has.
/// - **Unknown** — a cold launch before the push coordinator has answered:
///   the same, except that a realm-scoped card is left for later (one with
///   server content is never overwritten by a snapshot that may be stale),
///   and no plain card is stacked beside one. A change the parent makes on
///   this phone is still shown at once.
/// - **Managed** — the server updates and ends this realm's cards (see
///   `LiveActivityController+Push.swift`).
///
/// A write the phone makes to a realm-scoped card is stamped from the card's
/// last server second (`LiveActivityPushState.phoneStamp`), never the phone's
/// clock, so the server's next push always wins.
@MainActor
public enum LiveActivityController {

    /// Requests a card for a freshly started timer and persists its id on the
    /// timer. Does nothing for a timer that already has one. Silently does
    /// nothing when the user has Live Activities switched off, or when the
    /// request is refused (the system caps how many an app may run).
    ///
    /// Also the app's catch-up for running timers without a card: in a
    /// push-managed realm it adopts a card the server already started for the
    /// timer, and never brings back one the parent dismissed; while push is
    /// unknown it never stacks a plain card beside a realm-scoped one.
    public static func start(for timer: ActiveTimer, in store: BabyStore) {
        let babyID = timer.babyID
        let kind = timer.kind
        let regime = regime(for: store)
        switch regime {
        case .managed(let push):
            guard timer.liveActivityID == nil, !push.isDismissed(timer) else { return }
            if LiveCards.board.cards().contains(where: { $0.babyID == babyID && $0.kindRaw == kind.rawValue }) {
                Task { await adoptOrStart(for: timer, in: store, policy: .reconciliation, own: false) }
                return
            }
            Task { await endBreaks(besides: kind, babyID: babyID, in: store, regime: regime, own: false) }
            requestCard(for: timer, in: store, push: push)
        case .unknown, .withdrawn:
            Task { await endBreaks(besides: kind, babyID: babyID, in: store, regime: regime, own: false) }
            guard timer.liveActivityID == nil else { return }
            if case .unknown = regime, hasRealmCard(babyID: babyID, kind: kind, in: store) { return }
            requestCard(for: timer, in: store, push: nil)
        }
    }

    @discardableResult
    static func requestCard(for timer: ActiveTimer, in store: BabyStore, push: Push?) -> String? {
        guard let id = requestCard(content(running(timer, in: store), in: store), babyID: timer.babyID,
                                   kind: timer.kind, entryID: timer.id, lifecycle: .init(timerID: timer.id, sessionID: timer.sessionID),
                                   in: store, push: push) else { return nil }
        persist(id, on: timer, in: store)
        return id
    }

    /// A running part's presentation. A sleep that continues the same sleep
    /// counts that sleep's earlier time asleep too, as Home's hero does; a
    /// failed read only costs the card that earlier time. Every running card
    /// the app writes — requested, adopted, edited, reconciled — is built here.
    static func running(_ timer: ActiveTimer, in store: BabyStore) -> ActivityPresentation {
        let earlier = (try? store.earlierAsleepSeconds(of: timer)) ?? 0
        return ActivityPresentation(timer: timer, earlierAsleepSeconds: earlier)
    }

    /// Everything the app writes on a card is built here: the presentation,
    /// the acknowledged revision it was built from when there is one, and
    /// whether this phone may write in the store's realm right now. A viewer's
    /// card never offers controls — a token card no push has reached yet
    /// included, which has no server metadata to say so.
    static func content(_ presentation: ActivityPresentation, remote: LiveActivityRevision? = nil,
                        in store: BabyStore) -> TimerActivityContentState {
        TimerActivityContentState(presentation, remote: remote, readOnly: !store.canWriteInActiveRealm)
    }

    /// `lifecycle`: what the card shows. A token card's is published for the
    /// push coordinator, which registers the card with it: until a push has
    /// reached the card, that is how the server knows whose card it is.
    static func requestCard(_ state: TimerActivityContentState, babyID: UUID, kind: TimerKind, entryID: UUID,
                            lifecycle: LiveActivityReconciliation.Lifecycle, in store: BabyStore, push: Push?) -> String? {
        let babyName = (try? store.babies())?.first { $0.id == babyID }?.name ?? ""
        let scope = push?.fence.scope(babyID: babyID, kindRaw: kind.rawValue)
        let request = LiveCardRequest(babyID: babyID, babyName: babyName, kindRaw: kind.rawValue, state: state,
                                      scope: scope, isPushEnabled: scope != nil)
        // Still silent for the user — a refused card loses no data, the timer
        // is persisted and keeps counting — but not silent in the log, which
        // is what made "my Lock Screen shows nothing" unanswerable.
        do {
            let id = try LiveCards.board.request(request)
            if let id, scope != nil { publish(lifecycle, for: id) }
            return id
        } catch {
            AppLog.timers.error(
                "live activity refused for \(AppLog.describe(entryKind: "ActiveTimer", id: entryID), privacy: .public): \(error.localizedDescription, privacy: .private)"
            )
            return nil
        }
    }

    /// The lifecycle a card shows, for the push coordinator (see
    /// `LiveActivityPushState.References`). Only a realm-scoped card is ever
    /// registered, so publishing one for any other card is inert.
    static func publish(_ lifecycle: LiveActivityReconciliation.Lifecycle, for id: String) {
        LiveCards.pushState.setReferences(.init(timerID: lifecycle.timerID, sessionID: lifecycle.sessionID), for: id)
    }

    static func persist(_ id: String, on timer: ActiveTimer, in store: BabyStore) {
        publish(.init(timerID: timer.id, sessionID: timer.sessionID), for: id)
        guard timer.liveActivityID != id else { return }
        do {
            try store.setLiveActivityID(id, on: timer)
        } catch {
            AppLog.timers.error(
                "could not persist live activity id for \(AppLog.describe(entryKind: "ActiveTimer", id: timer.id), privacy: .public): \(error.localizedDescription, privacy: .private)"
            )
        }
    }

    /// Shows `timer` on the card already up for its baby and kind, or
    /// requests a new card when there is none.
    ///
    /// The card outlives the part it was started for: Stop moves it into the
    /// break, and the next part — L or R tapped on that very card, or started
    /// in the app or on another caregiver's phone — takes the same card back
    /// to running. Requesting a fresh one instead would stack a second card
    /// beside the break card, and from a Lock Screen button (an intent running
    /// in the background) the request may be refused outright; an update to a
    /// card that exists is not.
    ///
    /// Awaited, not fired into a `Task`: from an App Intent's `perform()`,
    /// returning lets iOS suspend the process before an unawaited push lands.
    ///
    /// A start the parent made on this phone: shown at once, offline too.
    public static func adoptOrStart(for timer: ActiveTimer, in store: BabyStore) async {
        await adoptOrStart(for: timer, in: store, policy: .userAction, own: true)
    }

    /// Another caregiver's timer appeared in a sync. As `adoptOrStart`, except
    /// that in a push-managed realm the card comes from the server's
    /// push-to-start: a card already up is adopted, none is requested.
    public static func adoptRemoteTimer(_ timer: ActiveTimer, in store: BabyStore) async {
        await adoptOrStart(for: timer, in: store, policy: .remoteAnnouncement, own: false)
    }

    static func adoptOrStart(for timer: ActiveTimer, in store: BabyStore, policy: RequestPolicy, own: Bool) async {
        let regime = regime(for: store)
        await endBreaks(besides: timer.kind, babyID: timer.babyID, in: store, regime: regime, own: own)
        if case .managed(let push) = regime {
            await reconcileGroup(babyID: timer.babyID, kind: timer.kind, in: store, push: push, policy: policy,
                                 deferringPushed: false, own: own)
            return
        }
        let cards = LiveCards.board.cards().filter { $0.babyID == timer.babyID && $0.kindRaw == timer.kindRaw }
        guard let card = chosenCard(among: cards, ownedID: timer.liveActivityID) else {
            requestCard(for: timer, in: store, push: nil)
            return
        }
        persist(card.id, on: timer, in: store)
        await showLocally(running(timer, in: store), lifecycle: .init(timerID: timer.id, sessionID: timer.sessionID), on: card,
                          own: own, regime: regime, in: store)
        // A second card for the same baby and kind can only ever disagree
        // with this one. A realm-scoped one is left for a definitive answer:
        // ending it here could reach the server as a dismissal.
        for duplicate in cards where duplicate.id != card.id {
            if case .unknown = regime, duplicate.scope != nil { continue }
            await endLocally(duplicate, now: store.now)
        }
    }

    /// Takes down `babyID`'s break cards of every kind but `kind`. A break
    /// card only means anything while nothing runs for the baby: the moment
    /// a sleep starts, the feed's break card goes, and the other way round.
    static func endBreaks(besides kind: TimerKind, babyID: UUID, in store: BabyStore, regime: Regime, own: Bool) async {
        for card in LiveCards.board.cards() where card.babyID == babyID
            && card.kindRaw != kind.rawValue && card.state.phase != .running {
            if case .managed(let push) = regime, let scope = card.scope, push.fence.contains(scope) {
                await show(nil, lifecycle: .none, on: card, push: push, in: store, own: own)
            } else {
                await showLocally(nil, lifecycle: .none, on: card, own: own, regime: regime, in: store)
            }
        }
    }

    /// The one card a baby and kind keeps when there are several: the one the
    /// running timer owns, otherwise the first. Every other is a duplicate.
    /// Outside a push-managed realm; inside one the server's choice wins
    /// (`LiveActivityReconciliation.winner`).
    public nonisolated static func chosenCard(among cards: [LiveCard], ownedID: String?) -> LiveCard? {
        cards.first { $0.id == ownedID } ?? cards.first
    }

    /// Pushes the timer's current state — after an edit to its start, its
    /// bottle, or its session link.
    ///
    /// `own`: the parent made the edit on this phone (Home, the running-timer
    /// editor). Shown at once; on a realm-scoped card it keeps the card's
    /// server revision and is stamped from it, so the server's push for this
    /// edit, or anything later, still replaces it.
    ///
    /// Not own — another caregiver's change pulled in a sync — is a snapshot
    /// that may already be older than what the server pushed: in a
    /// push-managed realm it goes through the ordering rule, and while push is
    /// unknown a card with server content is left alone.
    public static func update(for timer: ActiveTimer, in store: BabyStore, own: Bool) async {
        guard let id = timer.liveActivityID, let card = LiveCards.board.cards().first(where: { $0.id == id }) else { return }
        let regime = regime(for: store)
        if !own {
            if case .managed(let push) = regime {
                await reconcileGroup(babyID: timer.babyID, kind: timer.kind, in: store, push: push, policy: .remoteAnnouncement,
                                     deferringPushed: false, own: false)
            } else {
                await showLocally(running(timer, in: store), lifecycle: .init(timerID: timer.id, sessionID: timer.sessionID), on: card,
                                  own: false, regime: regime, in: store)
            }
            return
        }
        let presentation = running(timer, in: store)
        if card.scope != nil, !LiveCards.pushState.isWithdrawn {
            publish(.init(timerID: timer.id, sessionID: timer.sessionID), for: card.id)
            await LiveCards.board.update(id: card.id, state: content(presentation, remote: card.state.remote, in: store),
                                         staleDate: nil, timestamp: phoneStamp(for: card, now: store.now))
            return
        }
        await updateLocally(card, to: content(presentation, in: store), staleDate: nil, now: store.now)
    }

    /// The Lock Screen's sleep toggle found nothing to do: the card was behind
    /// the store, or the store behind the card. The parent changed nothing
    /// here, so this is a catch-up, not their own change: the nap someone
    /// else started is shown — or the card goes once it has ended — only as
    /// far as the ordering rule allows a possibly stale snapshot.
    public static func showSleepAsStored(running: ActiveTimer?, babyID: UUID, in store: BabyStore) async {
        if let running {
            await adoptOrStart(for: running, in: store, policy: .reconciliation, own: false)
        } else {
            await enterBreak(kind: .sleep, babyID: babyID, in: store)
        }
    }

    /// Whether one card is the one a tap for `target`/`kind` means to dismiss.
    ///
    /// Matched on `kind` as well as the baby, because a feed and a sleep may
    /// legally run at once for one baby: dismissing on the baby alone takes
    /// down the other, still-live card along with the dead one.
    ///
    /// `target` is the raw string the App Intent carries, because App Intent
    /// parameters cannot be `UUID`. A `target` that is absent or unparseable
    /// matches nothing: that is the Home Screen widget's button, which owns no
    /// card.
    public nonisolated static func isStale(
        activityBabyID: UUID,
        activityKindRaw: String,
        target: String?,
        kind: TimerKind
    ) -> Bool {
        guard let target, let id = UUID(uuidString: target) else { return false }
        return activityBabyID == id && activityKindRaw == kind.rawValue
    }

    /// Dismisses the card left on the Lock Screen for a baby who has since
    /// been deleted: a tap on it correctly writes nothing, and without this
    /// would leave a dead button on a card that never goes away.
    public nonisolated static func endStaleActivity(babyID: String?, kind: TimerKind) async {
        let board = LiveCards.board
        for card in board.cards() where isStale(
            activityBabyID: card.babyID, activityKindRaw: card.kindRaw, target: babyID, kind: kind
        ) {
            await board.end(id: card.id, timestamp: phoneStamp(for: card, now: Date()))
        }
    }

    /// Runs a stop that deletes `timer`, then moves its card into the break
    /// that follows — or ends the card when no break follows.
    ///
    /// `stopFeed`/`stopSleep` call `context.delete(timer)`, so everything the
    /// card needs afterwards — its id, the baby, the kind — is read before
    /// `stop` runs. Funnelling every stop through here makes that ordering
    /// impossible to get wrong at a call site.
    ///
    /// The break is read from `homeSnapshot` after the stop, so the card says
    /// exactly what Home says: a feed part that was discarded as too short
    /// leaves the previous session's break if one is still open, and no break
    /// at all otherwise, in which case the card goes.
    ///
    /// A stop the parent made on this phone: shown at once, offline too.
    @discardableResult
    public static func stopping<T>(
        _ timer: ActiveTimer,
        in store: BabyStore,
        _ stop: () throws -> T
    ) async rethrows -> T {
        let activityID = timer.liveActivityID
        let babyID = timer.babyID
        let kind = timer.kind
        let result = try stop()
        await enterBreak(kind: kind, babyID: babyID, in: store, activityID: activityID, policy: .userAction, own: true)
        return result
    }

    /// Brings `babyID`'s `kind` card in line after its part stopped: the
    /// break the store says is open, or gone when none is. Also the answer to
    /// a Stop that finds nothing to stop — the part ended elsewhere, and the
    /// card should show what followed rather than vanish.
    public static func enterBreak(kind: TimerKind, babyID: UUID, in store: BabyStore) async {
        await enterBreak(kind: kind, babyID: babyID, in: store, activityID: nil, policy: .userAction, own: false)
    }

    /// The same, for a card known only by its id — a timer another caregiver
    /// stopped, of which nothing is left here but the card it ran under.
    public static func enterBreak(activityID: String?, in store: BabyStore) async {
        guard let card = LiveCards.board.cards().first(where: { $0.id == activityID }),
              let kind = TimerKind(rawValue: card.kindRaw) else { return }
        await enterBreak(kind: kind, babyID: card.babyID, in: store, activityID: card.id, policy: .remoteAnnouncement, own: false)
    }

    /// What a card shows once the part it was showing has stopped.
    public enum AfterStop: Equatable, Sendable {
        /// The next part already runs — started on another phone in the same
        /// sync, or by the store's safety net: the card shows it.
        case adoptRunning
        case showBreak
        case end
    }

    /// The rule `enterBreak` applies. A remote stop is announced after the
    /// whole sync has landed, so the next part's timer may already be in the
    /// store by then; ending the card would take down the card that part is
    /// (or should be) running on.
    public nonisolated static func afterStop(hasRunningTimer: Bool, isBreakOpen: Bool) -> AfterStop {
        if hasRunningTimer { return .adoptRunning }
        return isBreakOpen ? .showBreak : .end
    }

    private static func enterBreak(kind: TimerKind, babyID: UUID, in store: BabyStore, activityID: String?,
                                   policy: RequestPolicy, own: Bool) async {
        let regime = regime(for: store)
        if case .managed(let push) = regime {
            await reconcileGroup(babyID: babyID, kind: kind, in: store, push: push, policy: policy, deferringPushed: false, own: own)
            return
        }
        let running = try? store.activeTimer(kind: kind, for: babyID)
        let open = readBreak(kind: kind, babyID: babyID, in: store)
        switch afterStop(hasRunningTimer: running != nil, isBreakOpen: open != nil) {
        case .adoptRunning:
            if let running { await adoptOrStart(for: running, in: store, policy: policy, own: own) }
        case .showBreak, .end:
            let cards = LiveCards.board.cards().filter {
                $0.id == activityID || ($0.babyID == babyID && $0.kindRaw == kind.rawValue)
            }
            let lifecycle = LiveActivityReconciliation.Lifecycle(timerID: nil, sessionID: open?.sessionID)
            for card in cards {
                await showLocally(open?.presentation, lifecycle: lifecycle, on: card, own: own, regime: regime, in: store)
            }
        }
    }

    static func readBreak(kind: TimerKind, babyID: UUID, in store: BabyStore) -> OpenBreak? {
        do {
            return try openBreak(kind: kind, babyID: babyID, in: store)
        } catch {
            AppLog.timers.error(
                "could not read the break for a live activity: \(error.localizedDescription, privacy: .private)"
            )
            return nil
        }
    }

    struct OpenBreak {
        let presentation: ActivityPresentation
        let sessionID: UUID
    }

    /// The break card for `babyID`'s `kind`, or nil when no break is open —
    /// a part still runs, nothing ran, or the gap has passed the limit — or
    /// when anything else runs for the baby: a feed's break card goes once a
    /// sleep starts, though Home still shows the break.
    ///
    /// A sleep has no break card: Wake up takes the sleep's card down.
    public static func breakPresentation(
        kind: TimerKind,
        babyID: UUID,
        in store: BabyStore
    ) throws -> ActivityPresentation? {
        try openBreak(kind: kind, babyID: babyID, in: store)?.presentation
    }

    /// The lifecycle `babyID`'s `kind` card should show now: the running
    /// timer, or the open break's session — the same reading every pass
    /// makes. What a card action checks its card against.
    public static func lifecycle(kind: TimerKind, babyID: UUID, in store: BabyStore) throws -> LiveActivityReconciliation.Lifecycle {
        if let timer = try store.activeTimer(kind: kind, for: babyID) {
            return LiveActivityReconciliation.Lifecycle(timerID: timer.id, sessionID: timer.sessionID)
        }
        return LiveActivityReconciliation.Lifecycle(timerID: nil, sessionID: try openBreak(kind: kind, babyID: babyID, in: store)?.sessionID)
    }

    private static func openBreak(kind: TimerKind, babyID: UUID, in store: BabyStore) throws -> OpenBreak? {
        let snapshot = try store.homeSnapshot(for: babyID)
        guard snapshot.feedTimer == nil, snapshot.sleepTimer == nil else { return nil }
        switch kind {
        case .feed:
            guard let session = snapshot.feedBreak else { return nil }
            let presentation = ActivityPresentation.feedBreak(
                session: session,
                members: snapshot.sessionMembers(session),
                limit: snapshot.breakLimits.feed,
                suggested: try store.suggestedSide(for: babyID)
            )
            return OpenBreak(presentation: presentation, sessionID: session.id)
        case .sleep:
            return nil
        }
    }
}
