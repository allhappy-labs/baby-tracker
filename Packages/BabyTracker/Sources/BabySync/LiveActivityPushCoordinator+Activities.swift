import BabyCore
import BabyProtocol
import BabyStore
import Foundation

/// Each card's update-token registration: planned under the ledger lock
/// from what ActivityKit reported and what the card controller published,
/// versioned per card, and adopted into the server's canonical generation.
extension LiveActivityPushCoordinator {
    /// `skippingDenied`: the device registration was just renewed in this
    /// pass, so a card the server still refuses is refused for itself. It is
    /// left pending for the next trigger and the other cards still register.
    func syncActivities(_ session: Session, skippingDenied: Bool = false) async -> Outcome {
        let context = session.context
        guard let device = try? store.read().device, device.accepted, !device.revoked,
              device.familyID == context.familyKey.familyID, device.environment == context.environment else { return .idle }
        for id in observed.keys.sorted() {
            guard isCurrent(session) else { return .idle }
            guard !retired.contains(id), let card = observed[id], card.state != .unknown, let scope = card.scope, context.contains(scope),
                  scope.babyID == card.babyID, scope.kindRaw == card.kindRaw else { continue }
            if !card.state.isLive, lostToRegisteredDuplicate(id) {
                // Recorded as retired, never reported; not recordable, it is
                // still not reported, and the next pass tries again.
                if !retire([id]) { AppLog.sync.error("live activity duplicate end not recorded") }
                continue
            }
            let outcome = await syncActivity(card, scope: scope, session: session, skippingDenied: skippingDenied)
            guard outcome == .idle else { return outcome }
        }
        return .idle
    }

    private struct ActivityPlan {
        let version: Int64
        let sent: LiveActivityRegistrationLedger.Activity.Sent
        /// As for the device: the Keychain follows the committed ledger.
        var storesToken = false
    }

    private func syncActivity(_ card: LiveActivityObservation, scope: LiveActivityScope, session: Session,
                              skippingDenied: Bool) async -> Outcome {
        let slot = LiveActivityRegistrationStore.TokenSlot.activity(session.installationID, card.activityID)
        var conflicts = 0
        while isCurrent(session) {
            // A card whose token has not been issued yet waits for it.
            guard let token = card.pushToken ?? (try? store.token(for: slot)) else { return .idle }
            let state: LiveActivityRegistrationState = card.state.isLive ? .active : (card.state == .dismissed ? .dismissed : .ended)
            let plan: ActivityPlan?
            do {
                plan = try store.update { ledger -> ActivityPlan? in
                    try planActivity(&ledger, card: card, scope: scope, state: state, token: token, slot: slot, session: session)
                }
            } catch {
                return .failClosed
            }
            guard let plan else { return .idle }
            if plan.storesToken {
                do { try store.saveToken(token, for: slot) } catch { return .failClosed }
            }
            let registration: LiveActivityTokenRegistration
            do {
                registration = try LiveActivityTokenRegistration(
                    installationID: session.installationID, activityID: card.activityID, token: token, tokenVersion: plan.version,
                    scope: scope, knownGenerationID: plan.sent.knownGenerationID, timerID: plan.sent.timerID,
                    sessionID: plan.sent.sessionID, state: plan.sent.state
                )
            } catch {
                return .idle
            }
            do {
                let receipt = try await session.client.registerActivity(registration)
                guard isCurrent(session) else { return .idle }
                let context = session.context
                guard receipt.serverInstanceID == context.endpoint.serverInstanceID, receipt.recoveryEpoch == context.recoveryEpoch,
                      receipt.acceptedTokenVersion == plan.version else { return .failClosed }
                let canonical = receipt.generations.first { $0.activityID == card.activityID && $0.scope == scope }?.generationID
                try store.update { ledger in
                    guard ledger.installationID == session.installationID, var record = ledger.activities[card.activityID],
                          record.sent?.version == plan.version else { return }
                    record.accepted = true
                    if let canonical {
                        // The server has adopted this card into its canonical
                        // generation; that is now what the card claims, and
                        // learning it is not itself a reason to re-register.
                        record.canonicalGenerationID = canonical
                        record.sent?.knownGenerationID = canonical
                    }
                    ledger.activities[card.activityID] = record
                    // A finished card's token has nothing left to do.
                    if plan.sent.state != .active { try? store.deleteToken(for: slot) }
                }
                return .idle
            } catch LiveActivityPushClientError.versionConflict {
                guard isCurrent(session) else { return .idle }
                conflicts += 1
                guard conflicts <= 1 else { return .idle }
                try? store.update { ledger in
                    if ledger.activities[card.activityID]?.sent?.version == plan.version {
                        ledger.activities[card.activityID]?.superseded = true
                    }
                }
            } catch LiveActivityPushClientError.registrationDenied {
                guard isCurrent(session) else { return .idle }
                if skippingDenied {
                    AppLog.sync.error("live activity registration refused; left for the next trigger")
                    return .idle
                }
                try? store.update { ledger in
                    if ledger.device?.familyID == session.context.familyKey.familyID { ledger.device?.superseded = true }
                }
                return .deviceStale
            } catch {
                return outcome(of: error)
            }
        }
        return .idle
    }

    private func planActivity(
        _ ledger: inout LiveActivityRegistrationLedger, card: LiveActivityObservation, scope: LiveActivityScope,
        state: LiveActivityRegistrationState, token: String, slot: LiveActivityRegistrationStore.TokenSlot, session: Session
    ) throws -> ActivityPlan? {
        guard ledger.installationID == session.installationID else { throw LiveActivityRegistrationStore.StoreError.unreadable }
        var record = ledger.activities[card.activityID]
            ?? .init(scope: scope, tokenVersion: 0, sent: nil, accepted: false, canonicalGenerationID: nil)
        guard record.scope == scope else { return nil }
        if let sent = record.sent, sent.state != .active, state == .active { return nil }
        let references = Self.references(of: card, published: pushState.references(for: card.activityID), sent: record.sent)
        let desired = LiveActivityRegistrationLedger.Activity.Sent(
            version: record.tokenVersion, state: state,
            knownGenerationID: record.canonicalGenerationID ?? card.remote?.generationID,
            timerID: references.timerID, sessionID: references.sessionID, environment: session.context.environment
        )
        let sentToken = try store.token(for: slot)
        if let sent = record.sent, !record.superseded, sent == desired, sentToken == token {
            return record.accepted ? nil : ActivityPlan(version: sent.version, sent: sent)
        }
        guard record.tokenVersion < LiveActivityContract.maximumCounter else { return nil }
        record.tokenVersion += 1
        var sent = desired
        sent.version = record.tokenVersion
        record.sent = sent
        record.accepted = false
        record.superseded = false
        ledger.activities[card.activityID] = record
        return ActivityPlan(version: sent.version, sent: sent, storesToken: true)
    }

    /// The lifecycle a card is registered against. A pushed card says it
    /// itself, in the server's revision. A card this app requested has none:
    /// its timer and session are what the card controller published for it —
    /// or, in a process where it has not yet, what was last sent — so the
    /// server can adopt it into the lifecycle the phone itself started.
    static func references(
        of card: LiveActivityObservation, published: LiveActivityPushState.References?,
        sent: LiveActivityRegistrationLedger.Activity.Sent?
    ) -> LiveActivityPushState.References {
        if let remote = card.remote { return .init(timerID: remote.timerID, sessionID: remote.sessionID) }
        if let published { return published }
        return .init(timerID: sent?.timerID, sessionID: sent?.sessionID)
    }

    /// A card that lost to a registered card of the same generation with a
    /// smaller activity ID — the one the server keeps and pushes to. The
    /// server ended this one itself as a duplicate; its ending is that
    /// retirement, never the parent dismissing the lifecycle.
    private func lostToRegisteredDuplicate(_ id: String) -> Bool {
        guard let ledger = try? store.read(), let mine = ledger.activities[id], let generation = mine.canonicalGenerationID else {
            return false
        }
        return ledger.activities.contains { other, record in
            other < id && !retired.contains(other) && record.scope == mine.scope && record.canonicalGenerationID == generation
                && record.accepted && record.sent?.state == .active
        }
    }
}
