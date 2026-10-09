import BabyCore
import BabyProtocol
import BabyStore
import Foundation

/// The foreground inventory: what a read of `Activity.activities` may attest —
/// an unconfirmed remote start absent, a registered card dismissed while
/// nothing observed it — and only from the foreground.
extension LiveActivityPushCoordinator {
    /// Attests absence only from a foreground read of `Activity.activities`,
    /// only for starts the latest receipt says are unconfirmed, and never from
    /// a token that simply has not arrived yet.
    func prepareForegroundInventory(_ session: Session, present: [LiveActivityObservation]) -> Bool {
        do {
            return try store.update { ledger -> Bool in
                let family = session.context.familyKey.familyID
                guard ledger.installationID == session.installationID else { return false }
                if ledger.pendingInventory != nil { return ledger.pendingInventoryFamilyID == family }
                var absent: [LiveActivityAbsentGeneration] = []
                for start in ledger.unconfirmedStarts where session.context.contains(start.scope) {
                    let isPresent = present.contains { card in
                        guard card.babyID == start.scope.babyID, card.kindRaw == start.scope.kindRaw,
                              card.scope.map(session.context.contains) == true else { return false }
                        return ledger.activities[card.activityID]?.canonicalGenerationID == start.generationID
                            || card.remote?.generationID == start.generationID
                    }
                    guard !isPresent, !absent.contains(where: { $0.generationID == start.generationID }),
                          let proof = try? LiveActivityAbsentGeneration(generationID: start.generationID, observedAttemptID: start.attemptID)
                    else { continue }
                    absent.append(proof)
                    if absent.count == Self.maximumAbsenceProofs { break }
                }
                guard !absent.isEmpty, ledger.inventoryVersion < LiveActivityContract.maximumCounter else { return false }
                ledger.inventoryVersion += 1
                ledger.pendingInventory = try LiveActivityForegroundInventory(inventoryVersion: ledger.inventoryVersion, absentGenerations: absent)
                ledger.pendingInventoryFamilyID = family
                return true
            }
        } catch {
            return false
        }
    }

    /// A registered card missing from a foreground `Activity.activities`
    /// read was dismissed while nothing observed it: register that, with its
    /// stored token, then forget it once the server has the terminal state.
    /// A card that is merely waiting for a token is in the list, so it is
    /// never mistaken for a dismissal.
    ///
    /// An absent card with no token at all — never stored, or gone from the
    /// Keychain — can never be registered dismissed: ActivityKit will not
    /// report a card it no longer lists, so no token will come. It is
    /// forgotten at once, not re-marked dismissed and carried in the ledger
    /// on every foreground.
    func reconcileDismissals(_ session: Session, present: [LiveActivityObservation]) {
        let presentIDs = Set(present.map(\.activityID))
        guard let ledger = try? store.read(), ledger.installationID == session.installationID else { return }
        var finished: [String] = []
        for (id, record) in ledger.activities where !presentIDs.contains(id) && !retired.contains(id) && session.context.contains(record.scope) {
            if let sent = record.sent, sent.state != .active, record.accepted {
                finished.append(id)
                continue
            }
            // A Keychain that cannot answer right now is not an answer of
            // "no token": the card is left for the next foreground.
            let token: String?
            do { token = try observed[id]?.pushToken ?? store.token(for: .activity(session.installationID, id)) } catch { continue }
            if token == nil {
                finished.append(id)
            } else if observed[id]?.state.isLive != false {
                observed[id] = LiveActivityObservation(
                    activityID: id, babyID: record.scope.babyID, kindRaw: record.scope.kindRaw, scope: record.scope,
                    remote: observed[id]?.remote, state: .dismissed, pushToken: observed[id]?.pushToken
                )
            }
        }
        guard !finished.isEmpty else { return }
        for id in finished { observed[id] = nil }
        try? store.update { ledger in
            for id in finished {
                ledger.activities[id] = nil
                try? store.deleteToken(for: .activity(session.installationID, id))
            }
        }
    }
}
