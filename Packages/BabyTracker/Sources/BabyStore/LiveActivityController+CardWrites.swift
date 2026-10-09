import Foundation
import BabyCore

/// The writes outside a push-managed realm, and the primitives every regime's
/// writes end in: a realm card's write and stamp, a local update, a retired end.
extension LiveActivityController {

    /// Whether a non-wrong realm-scoped card is up for this baby and kind.
    static func hasRealmCard(babyID: UUID, kind: TimerKind, in store: BabyStore) -> Bool {
        LiveCards.board.cards().contains {
            $0.babyID == babyID && $0.kindRaw == kind.rawValue && $0.scope != nil
                && !isWrongScope($0, in: store, regime: .unknown)
        }
    }

    /// One card brought in line with `presentation` (nil: nothing to show)
    /// while the realm is not push-managed.
    ///
    /// Withdrawn, or a card with no realm: at once, as always. Unknown and a
    /// realm-scoped card: a change the parent made on this phone is shown at
    /// once, stamped from the card's last server second and keeping its
    /// server revision, so the server's own push still replaces it; ending it
    /// is retired first, never reported as a dismissal. Anything else — a
    /// snapshot that may be stale — leaves a card with server content alone.
    static func showLocally(_ presentation: ActivityPresentation?, lifecycle: LiveActivityReconciliation.Lifecycle,
                            on card: LiveCard, own: Bool, regime: Regime, in store: BabyStore) async {
        if isWrongScope(card, in: store, regime: regime) {
            await endLocally(card, now: store.now)
            return
        }
        guard case .unknown = regime, card.scope != nil else {
            if let presentation {
                await updateLocally(card, to: content(presentation, in: store), staleDate: presentation.breakEndsAt, now: store.now)
            } else {
                await endLocally(card, now: store.now)
            }
            return
        }
        if card.state.remote != nil, !own { return }
        // A token card with no push yet may still be registered: ended
        // without a recorded retirement, the server would read the end as a
        // dismissal. Only the parent's own change ends it before push is known.
        if presentation == nil, !own { return }
        await writeOnRealmCard(presentation, lifecycle: lifecycle, on: card, written: card.state.remote.flatMap {
            writtenRevision(for: card, remote: $0, lifecycle: lifecycle, registered: nil, in: store)
        }, in: store)
    }

    /// The write itself, on a realm-scoped card the server may push to. An
    /// end is the app's, never the parent's swipe, so it is retired before
    /// the card goes — the server must not read it as a dismissal — and the
    /// card is left up when that cannot be recorded. Anything else publishes
    /// the lifecycle the card now shows.
    static func writeOnRealmCard(_ presentation: ActivityPresentation?, lifecycle: LiveActivityReconciliation.Lifecycle,
                                 on card: LiveCard, written: LiveActivityRevision?, in store: BabyStore) async {
        let board = LiveCards.board
        guard let presentation else {
            if !LiveCards.pushState.retire([card.id]) {
                AppLog.timers.error("live activity end kept: its retirement could not be recorded")
                return
            }
            await board.end(id: card.id, timestamp: phoneStamp(for: card, now: Date()))
            return
        }
        publish(lifecycle, for: card.id)
        let state = content(presentation, remote: written, in: store)
        guard state != card.state || presentation.breakEndsAt != card.staleDate else { return }
        await board.update(id: card.id, state: state, staleDate: presentation.breakEndsAt,
                           timestamp: phoneStamp(for: card, now: Date()))
    }

    /// An update the app makes on its own authority. On a realm-scoped card
    /// the server may push to, stamped from its last server second; otherwise
    /// as for a withdrawn realm.
    static func updateLocally(_ card: LiveCard, to state: TimerActivityContentState, staleDate: Date?, now: Date) async {
        await LiveCards.board.update(id: card.id, state: state, staleDate: staleDate, timestamp: phoneStamp(for: card, now: now))
    }

    /// An end the app makes on its own authority, on a card no server pushes
    /// to any more — another realm's, an earlier build's, or after push was
    /// withdrawn.
    ///
    /// Still retired first when it has a realm and something in this process
    /// records retirements: the card may be registered, and once push comes
    /// back the coordinator would otherwise report ActivityKit's `ended` for
    /// it, which the server reads as the parent dismissing that lifecycle.
    /// Not recorded, the card stays up for a later pass.
    static func endLocally(_ card: LiveCard, now: Date) async {
        let state = LiveCards.pushState
        if card.scope != nil, state.hasRetireHandler, !state.retire([card.id]) {
            AppLog.timers.error("live activity end kept: its retirement could not be recorded")
            return
        }
        await LiveCards.board.end(id: card.id, timestamp: withdrawnStamp(for: card, now: now))
    }

    /// Where no server will push again: just after the card's last server
    /// second, so ActivityKit takes it whatever this phone's clock says, or
    /// none at all for a card no server ever pushed to.
    private nonisolated static func withdrawnStamp(for card: LiveCard, now: Date) -> Date? {
        card.state.remote.map { max(now, $0.presentationDate.addingTimeInterval(1)) }
    }

    /// The stamp for any phone write: from the card's last server second for
    /// a realm-scoped card while the server may push to it, never the phone's
    /// clock; as for a withdrawn realm otherwise.
    nonisolated static func phoneStamp(for card: LiveCard, now: Date) -> Date? {
        let state = LiveCards.pushState
        guard card.scope != nil, !state.isWithdrawn else { return withdrawnStamp(for: card, now: now) }
        return state.phoneStamp(for: card.id, lastServerSecond: card.state.remote?.presentationTimestamp)
    }
}
