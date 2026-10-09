import BabyCore
import BabyProtocol
import BabyStore
import Foundation

/// What the registration coordinator publishes to the card controller
/// (`LiveActivityPushState`), and what it does with a card the controller
/// retires. Kept apart from the coordinator's registration pump: these are
/// pure reads of the ledger and of what ActivityKit reported.
public enum LiveActivityPushCardState {
    /// Retired cards remembered in the ledger, newest last. Bounded: a card
    /// is only ever reported again while ActivityKit still lists it.
    static let retiredLimit = 64

    struct Publication: Equatable {
        var mode: LiveActivityPushState.Mode = .unknown
        var registrations: [String: LiveActivityPushState.Registration] = [:]
        var dismissed: Set<UUID> = []
    }

    /// For a process that has the App Group ledger but no running coordinator
    /// yet — every cold launch, and an App Intent: the realm the server last
    /// accepted this device for is push-managed until something definitive
    /// says otherwise, and retired cards are recorded in the ledger. A
    /// coordinator replaces both with its own as it learns more.
    public static func installProvisional(from store: LiveActivityRegistrationStore, into state: LiveActivityPushState) {
        state.setRetireHandler { ids in
            do {
                try retire(ids, in: store)
                return true
            } catch {
                AppLog.sync.error("live activity retirement not recorded")
                return false
            }
        }
        guard state.mode == .unknown, let ledger = try? store.read(), let identity = ledger.identity,
              ledger.installationID != nil, let device = ledger.device, device.accepted, !device.revoked,
              identity.recoveryEpoch >= 1, identity.recoveryEpoch <= UInt64(LiveActivityContract.maximumCounter) else { return }
        state.install(LiveActivityRealmFence(
            serverInstanceID: identity.serverInstanceID, recoveryEpoch: Int64(identity.recoveryEpoch), familyID: device.familyID
        ))
    }

    /// The realm is push-managed once the server has accepted this device's
    /// registration for it — before that nothing could push, so it stays
    /// unknown — and stays so through a later re-registration in flight. It is
    /// withdrawn when the device was taken down, Live Activities are off or the
    /// server stopped advertising push; unknown again while the device moves to
    /// another family or environment.
    static func publication(
        context: LiveActivityPushContext,
        installationID: UUID,
        ledger: LiveActivityRegistrationLedger,
        observed: [String: LiveActivityObservation],
        acceptedBefore: Bool,
        activitiesEnabled: Bool
    ) -> (Publication, accepted: Bool) {
        let family = context.familyKey.familyID
        guard activitiesEnabled, context.pushAdvertised else { return (Publication(mode: .withdrawn), acceptedBefore) }
        guard ledger.installationID == installationID, let device = ledger.device, device.familyID == family,
              device.environment == context.environment else { return (Publication(), false) }
        guard !device.revoked else { return (Publication(mode: .withdrawn), false) }
        let accepted = acceptedBefore || device.accepted
        guard accepted else { return (Publication(), accepted) }

        let retired = Set(ledger.retiredActivityIDs ?? [])
        var publication = Publication(mode: .managed(LiveActivityRealmFence(
            serverInstanceID: context.endpoint.serverInstanceID, recoveryEpoch: context.recoveryEpoch, familyID: family
        )))
        for (id, record) in ledger.activities where context.contains(record.scope) && !retired.contains(id) {
            // Sent active, accepted or not: the server may hold it, and may
            // treat it as the lifecycle's primary card.
            guard let sent = record.sent, sent.state == .active else { continue }
            publication.registrations[id] = .init(
                generationID: record.canonicalGenerationID ?? sent.knownGenerationID,
                timerID: sent.timerID, sessionID: sent.sessionID, accepted: record.accepted
            )
        }
        for (id, card) in observed where !retired.contains(id) && (card.state == .dismissed || card.state == .ended) {
            guard let scope = card.scope, context.contains(scope) else { continue }
            let sent = ledger.activities[id]?.sent
            publication.dismissed.formUnion([card.remote?.timerID, card.remote?.sessionID, sent?.timerID, sent?.sessionID].compactMap { $0 })
        }
        return (publication, accepted)
    }

    /// Forgets retired cards: their ledger records go, and their IDs are
    /// remembered so that no later pass — in this process or after a relaunch
    /// — registers their end. Throws when the ledger could not be written, so
    /// the card is not ended. A token the Keychain refuses to delete is
    /// harmless: nothing registers a retired card.
    static func retire(_ ids: [String], in store: LiveActivityRegistrationStore) throws {
        try store.update { ledger in
            var retired = ledger.retiredActivityIDs ?? []
            for id in ids {
                ledger.activities[id] = nil
                if let installation = ledger.installationID { try? store.deleteToken(for: .activity(installation, id)) }
                if !retired.contains(id) { retired.append(id) }
            }
            ledger.retiredActivityIDs = Array(retired.suffix(retiredLimit))
        }
    }
}
