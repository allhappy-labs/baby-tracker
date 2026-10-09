import Foundation
import BabyCore

/// The pass over every card when the app becomes active: expired breaks end,
/// every other card is brought in line with the store, and realm cards wait
/// for the regime that may push to them.
extension LiveActivityController {
    /// What to do with a baby and kind's one card when the app becomes active.
    public enum Reconciliation: Equatable, Sendable {
        /// The card shows the right phase: push its current state anyway, so
        /// a break limit, parts list, suggested side or label that changed
        /// since the last push is brought up to date.
        case keep
        /// Dismiss it: its break is over, or nothing it could show is true.
        case end
        /// Its part stopped elsewhere and a break is open: show the break.
        case enterBreak
        /// A part runs that the card does not show: show it running.
        case adopt
    }

    /// The rule `endExpiredBreaks` applies to each baby and kind's chosen
    /// card. Pure, so the package suite can check it.
    public nonisolated static func reconcile(
        phase: ActivityPhase,
        breakEndsAt: Date?,
        hasRunningTimer: Bool,
        timerOwnsCard: Bool,
        isBreakOpen: Bool,
        now: Date
    ) -> Reconciliation {
        if hasRunningTimer {
            return phase == .running && timerOwnsCard ? .keep : .adopt
        }
        if phase != .running, !ActivityPhase.isActionable(breakEndsAt: breakEndsAt, now: now) { return .end }
        guard isBreakOpen else { return .end }
        return phase == .running ? .enterBreak : .keep
    }

    /// Ends every card whose break is over, and brings every other card in
    /// line with the store: a part that stopped or started elsewhere — on
    /// another caregiver's phone, through Siri, while this app was not
    /// running.
    ///
    /// Each baby and kind keeps exactly one card — the one its running timer
    /// owns, otherwise the first — and every other card for it is ended
    /// *before* that one is reconciled, so two cards can never both be judged
    /// surplus while a part runs.
    ///
    /// A break card cannot end itself when its limit passes: nothing runs
    /// then. `staleDate` hides its buttons at that instant; this, run each
    /// time the app becomes active, takes the card down.
    ///
    /// Run before the foreground sync. A realm-scoped card with server content
    /// is left for `reconcileAfterSync` (push-managed) or for a definitive
    /// answer (unknown): the store may not yet have what that push showed.
    public static func endExpiredBreaks(in store: BabyStore) async {
        await reconcileBeforeSync(in: store)
    }

    /// The foreground pass before sync: `endExpiredBreaks`.
    public static func reconcileBeforeSync(in store: BabyStore) async {
        await reconcileAll(in: store, afterSync: false)
    }

    /// The pass after a successful sync, in a push-managed realm only: every
    /// card, pushed ones included, brought in line with acknowledged state.
    /// Elsewhere a no-op — the foreground pass already did everything.
    public static func reconcileAfterSync(in store: BabyStore) async {
        guard case .managed = regime(for: store) else { return }
        await reconcileAll(in: store, afterSync: true)
    }

    private static func reconcileAll(in store: BabyStore, afterSync: Bool) async {
        let board = LiveCards.board
        let regime = regime(for: store)
        // A card from another family, server or recovery epoch says nothing
        // true here — a family left, an epoch bumped.
        for card in board.cards() where isWrongScope(card, in: store, regime: regime) {
            await endLocally(card, now: store.now)
        }
        var groups: [String: [LiveCard]] = [:]
        var order: [String] = []
        for card in board.cards() {
            let key = "\(card.babyID.uuidString)-\(card.kindRaw)"
            if groups[key] == nil { order.append(key) }
            groups[key, default: []].append(card)
        }
        for key in order {
            let cards = groups[key] ?? []
            guard let first = cards.first, let kind = TimerKind(rawValue: first.kindRaw) else {
                for card in cards { await endLocally(card, now: store.now) }
                continue
            }
            switch regime {
            case .managed(let push):
                await reconcileGroup(babyID: first.babyID, kind: kind, in: store, push: push, policy: .never,
                                     deferringPushed: !afterSync, own: false)
                continue
            case .unknown:
                // Not known yet whether the server pushes here: a realm card
                // waits for the answer rather than take a possibly stale
                // snapshot, or be ended as a dismissal.
                if cards.contains(where: { $0.scope != nil }) { continue }
            case .withdrawn:
                break
            }
            let timer = try? store.activeTimer(kind: kind, for: first.babyID)
            guard let card = chosenCard(among: cards, ownedID: timer?.liveActivityID) else { continue }
            for surplus in cards where surplus.id != card.id {
                await endLocally(surplus, now: store.now)
            }

            let open = readBreak(kind: kind, babyID: card.babyID, in: store)?.presentation
            switch reconcile(
                phase: card.state.phase,
                breakEndsAt: card.state.breakEndsAt,
                hasRunningTimer: timer != nil,
                timerOwnsCard: timer?.liveActivityID == card.id,
                isBreakOpen: open != nil,
                now: store.now
            ) {
            case .end:
                await endLocally(card, now: store.now)
            case .adopt:
                if let timer { await adoptOrStart(for: timer, in: store, policy: .userAction, own: false) }
            case .keep, .enterBreak:
                if let timer {
                    await updateLocally(card, to: content(running(timer, in: store), in: store), staleDate: nil, now: store.now)
                } else if let open {
                    await updateLocally(card, to: content(open, in: store), staleDate: open.breakEndsAt, now: store.now)
                }
            }
        }
    }
}
