import Foundation
import BabyCore

/// The push-managed realm: the server updates and ends the store's cards too,
/// so the app
///
/// - requests its own cards with an update token and the realm's scope;
/// - keeps one card per realm, baby and kind, preferring the one the server
///   sends to (`LiveActivityReconciliation.winner`), and retires the rest so
///   their end is never reported to the server as a dismissal;
/// - shows a change the parent makes on this phone at once, offline too;
/// - retires every card it ends — whoever's pass ends it — before it goes:
///   only the parent's own swipe is ever reported as a dismissal;
/// - otherwise overwrites a card the server pushed to only with acknowledged
///   state (`LiveActivityReconciliation.decide`), and leaves cards with server
///   content alone until a sync has succeeded (`reconcileAfterSync`), so a
///   stale foreground snapshot cannot replace them;
/// - stamps every write from the card's last server second, never later than
///   the server's next push, so that push always wins;
/// - never recreates a lifecycle the parent dismissed, and leaves another
///   caregiver's new timer to the server's push-to-start.
extension LiveActivityController {

    /// When a pass may request a new card in a push-managed realm.
    enum RequestPolicy {
        /// The parent started something: a card, as always.
        case userAction
        /// Catch-up for a running timer with no card here: only one never
        /// shown on this phone and never dismissed.
        case reconciliation
        /// Another caregiver's timer: the server's push-to-start brings it.
        case remoteAnnouncement
        /// A reconciliation pass over the cards that exist.
        case never
    }

    /// The store's realm, when the server pushes to its cards.
    struct Push {
        let fence: LiveActivityRealmFence
        let state: LiveActivityPushState
        let reader: LiveActivityRevisionReader

        func isDismissed(_ timer: ActiveTimer) -> Bool {
            state.isDismissed(lifecycle: timer.id) || timer.sessionID.map { state.isDismissed(lifecycle: $0) } == true
        }
    }

    /// Whether the server pushes to the store's cards (see the type's notes).
    enum Regime {
        case managed(Push)
        case unknown
        case withdrawn
    }

    static func regime(for store: BabyStore) -> Regime {
        let state = LiveCards.pushState
        switch state.mode {
        case .withdrawn:
            return .withdrawn
        case .unknown:
            return .unknown
        case .managed(let fence):
            // Another family's realm, or the store's cursor is another epoch's:
            // whatever push serves, it is not yet known for this store.
            guard fence.matches(store.activeRealm) else { return .unknown }
            if let cursor = try? store.syncCursor(), cursor.recoveryEpoch != fence.recoveryEpoch { return .unknown }
            return .managed(Push(fence: fence, state: state, reader: LiveActivityRevisionReader(store: store)))
        }
    }

    static func isWrongScope(_ card: LiveCard, in store: BabyStore, regime: Regime) -> Bool {
        guard let scope = card.scope else { return false }
        guard scope.matchesTarget(babyID: card.babyID, kindRaw: card.kindRaw) else { return true }
        if case .managed(let push) = regime { return !push.fence.contains(scope) }
        guard case .shared(_, let instance, let family) = store.activeRealm,
              scope.serverInstanceID == instance, scope.familyID == family else { return true }
        if let cursor = try? store.syncCursor(), cursor.recoveryEpoch != scope.recoveryEpoch { return true }
        return false
    }

    /// One realm, baby and kind in a push-managed realm: wrong-realm cards
    /// end, an earlier build's card is replaced with a token card, duplicates
    /// resolve to the server's choice, and the survivor shows what the store
    /// can vouch for — or, for `own`, what the parent just did here.
    ///
    /// `deferringPushed`: the pass before sync leaves a group with server
    /// content alone.
    static func reconcileGroup(babyID: UUID, kind: TimerKind, in store: BabyStore, push: Push, policy: RequestPolicy,
                               deferringPushed: Bool, own: Bool) async {
        let board = LiveCards.board
        let regime = Regime.managed(push)
        var cards = board.cards().filter { $0.babyID == babyID && $0.kindRaw == kind.rawValue }
        for card in cards where isWrongScope(card, in: store, regime: regime) {
            await endLocally(card, now: store.now)
        }
        cards.removeAll { isWrongScope($0, in: store, regime: regime) }
        if deferringPushed, cards.contains(where: { $0.state.remote != nil }) { return }

        let timer = try? store.activeTimer(kind: kind, for: babyID)
        let open = timer == nil ? readBreak(kind: kind, babyID: babyID, in: store) : nil
        let presentation = timer.map { running($0, in: store) } ?? open?.presentation
        let lifecycle = LiveActivityReconciliation.Lifecycle(timerID: timer?.id, sessionID: timer?.sessionID ?? open?.sessionID)
        let scoped = cards.filter { $0.scope != nil }
        let legacy = cards.filter { $0.scope == nil }

        guard !scoped.isEmpty else {
            if legacy.isEmpty {
                guard let timer, mayRequest(for: timer, policy: policy, push: push) else { return }
                requestCard(for: timer, in: store, push: push)
                return
            }
            await replaceLegacy(legacy, with: presentation, timer: timer, babyID: babyID, kind: kind, lifecycle: lifecycle,
                                in: store, push: push)
            return
        }
        // A usable pushed or token card exists: an earlier build's card beside
        // it only disagrees.
        for card in legacy { await endLocally(card, now: store.now) }

        let candidates = scoped.map { card -> LiveActivityReconciliation.Candidate in
            let registration = push.state.registration(for: card.id)
            return LiveActivityReconciliation.Candidate(
                id: card.id, isServerKnown: registration != nil,
                generationID: registration?.generationID ?? card.state.remote?.generationID,
                timerID: registration?.timerID ?? card.state.remote?.timerID,
                sessionID: registration?.sessionID ?? card.state.remote?.sessionID,
                remote: card.state.remote
            )
        }
        guard let winnerID = LiveActivityReconciliation.winner(among: candidates, ownedID: timer?.liveActivityID, lifecycle: lifecycle),
              let winner = scoped.first(where: { $0.id == winnerID }) else { return }
        let losers = scoped.filter { $0.id != winnerID }
        if !losers.isEmpty {
            // Retired before they end: their end must never reach the server,
            // where an ended card of the shared lifecycle reads as the parent
            // dismissing it. Not recorded, they stay up for the next pass.
            if push.state.retire(losers.map(\.id)) {
                for card in losers { await board.end(id: card.id, timestamp: phoneStamp(for: card, now: store.now)) }
            } else {
                AppLog.timers.error("duplicate live activities kept: their retirement could not be recorded")
            }
        }
        if let timer { persist(winner.id, on: timer, in: store) }
        await show(presentation, lifecycle: lifecycle, on: winner, push: push, in: store, own: own)
    }

    private static func mayRequest(for timer: ActiveTimer, policy: RequestPolicy, push: Push) -> Bool {
        switch policy {
        case .userAction: true
        case .reconciliation: timer.liveActivityID == nil && !push.isDismissed(timer)
        case .remoteAnnouncement, .never: false
        }
    }

    /// A card from the build before push has no realm and no update token: the
    /// server can neither update nor end it, and would start a second card
    /// (with an alert) beside it. It is replaced quietly by a token card with
    /// the same content. Refused — the app is in the background — the old
    /// card is kept going locally until the next chance.
    private static func replaceLegacy(_ legacy: [LiveCard], with presentation: ActivityPresentation?, timer: ActiveTimer?,
                                      babyID: UUID, kind: TimerKind, lifecycle: LiveActivityReconciliation.Lifecycle,
                                      in store: BabyStore, push: Push) async {
        guard let presentation else {
            for card in legacy { await endLocally(card, now: store.now) }
            return
        }
        if let id = requestCard(content(presentation, in: store), babyID: babyID, kind: kind,
                                entryID: timer?.id ?? lifecycle.sessionID ?? babyID, lifecycle: lifecycle, in: store, push: push) {
            if let timer { persist(id, on: timer, in: store) }
            for card in legacy { await endLocally(card, now: store.now) }
            return
        }
        guard let keep = chosenCard(among: legacy, ownedID: timer?.liveActivityID) else { return }
        for card in legacy where card.id != keep.id { await endLocally(card, now: store.now) }
        if let timer { persist(keep.id, on: timer, in: store) }
        await updateLocally(keep, to: content(presentation, in: store), staleDate: presentation.breakEndsAt, now: store.now)
    }

    /// Brings one card in line with `presentation` (nil: nothing to show).
    ///
    /// `own`: the parent changed this on this phone. Shown at once, offline
    /// too.
    ///
    /// Otherwise a card the server has pushed to is overwritten only as
    /// `LiveActivityReconciliation.decide` allows, with the acknowledged
    /// revision it was built from.
    ///
    /// Any end here is the app's, own pass or not, and is retired first: the
    /// card may be registered against a lifecycle that goes on, and the
    /// server reads an unretired end as the parent dismissing it — that
    /// lifecycle would never reach this phone again. Not recorded, the card
    /// stays up for a later pass.
    ///
    /// Either way the write is stamped from the card's last server second
    /// (`phoneStamp`): never later than the server's next push, so that push
    /// wins whatever this phone's clock says.
    static func show(_ presentation: ActivityPresentation?, lifecycle: LiveActivityReconciliation.Lifecycle,
                     on card: LiveCard, push: Push, in store: BabyStore, own: Bool) async {
        guard let scope = card.scope, push.fence.contains(scope) else {
            // An earlier build's card: no server ever pushes to it.
            if let presentation {
                await updateLocally(card, to: content(presentation, in: store), staleDate: presentation.breakEndsAt,
                                    now: store.now)
            } else {
                await endLocally(card, now: store.now)
            }
            return
        }
        let registered = push.state.registration(for: card.id).flatMap { $0.accepted ? $0.generationID : nil }
        guard let remote = card.state.remote else {
            // A token card no push has reached yet: nothing server-ordered to
            // protect, but still stamped so that first push wins.
            await writeOnRealmCard(presentation, lifecycle: lifecycle, on: card, written: nil, in: store)
            return
        }
        let written = writtenRevision(for: card, remote: remote, lifecycle: lifecycle, registered: registered, in: store)
        if own {
            await writeOnRealmCard(presentation, lifecycle: lifecycle, on: card, written: written, in: store)
            return
        }
        let local = push.reader.localRevision(scope: scope, remote: remote, lifecycle: lifecycle, registeredGeneration: registered)
        switch LiveActivityReconciliation.decide(local: local, remote: remote,
                                                 hasPendingLocalMutation: push.reader.hasPendingMutation(babyID: card.babyID),
                                                 sameScope: true) {
        case .preserveUntilSync:
            return
        case .end:
            await writeOnRealmCard(nil, lifecycle: lifecycle, on: card, written: written, in: store)
        case .apply, .adopt:
            await writeOnRealmCard(presentation, lifecycle: lifecycle, on: card, written: written, in: store)
        }
    }

    /// The revision a phone write carries on a card the server pushed to: the
    /// card's server generation (or the registered canonical one) stays its
    /// identity; timer, session and applied cursor are the store's; version
    /// and timestamp stay the card's last server ones, which every later stamp
    /// is derived from. Without an applied cursor for the realm, the card's
    /// own revision is kept as it is.
    static func writtenRevision(for card: LiveCard, remote: LiveActivityRevision, lifecycle: LiveActivityReconciliation.Lifecycle,
                                registered: UUID?, in store: BabyStore) -> LiveActivityRevision? {
        guard let scope = card.scope,
              let local = LiveActivityRevisionReader(store: store).localRevision(
                  scope: scope, remote: remote, lifecycle: lifecycle, registeredGeneration: registered
              ) else { return remote }
        return (try? LiveActivityRevision(
            scope: scope, generationID: registered ?? remote.generationID, timerID: local.timerID, sessionID: local.sessionID,
            familySequence: local.familySequence, presentationVersion: remote.presentationVersion,
            presentationTimestamp: remote.presentationTimestamp, canWrite: local.canWrite
        )) ?? remote
    }
}
