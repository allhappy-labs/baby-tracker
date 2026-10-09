import BabyCore
import BabyProtocol
import BabyStore
import Foundation

/// The device registration: one version per distinct request, allocated under the
/// ledger lock; the push-to-start token reaches the Keychain only once the
/// ledger recording its version has been committed.
extension LiveActivityPushCoordinator {
    private struct DevicePlan {
        let version: Int64
        let inventory: LiveActivityForegroundInventory?
        /// A newly allocated version: its token goes to the Keychain only
        /// once the ledger recording that version has been committed.
        var storesToken = false
    }

    private func currentStartToken(_ session: Session) -> String? {
        pushToStartToken ?? (try? store.token(for: .pushToStart(session.installationID)))
    }

    func syncDevice(_ session: Session) async -> Outcome {
        var conflicts = 0
        while isCurrent(session) {
            guard let token = currentStartToken(session) else { return .idle }
            let context = session.context
            let plan: DevicePlan?
            do {
                plan = try store.update { ledger -> DevicePlan? in
                    try planDevice(&ledger, session: session, token: token)
                }
            } catch {
                return .failClosed
            }
            guard let plan else {
                replayDevice = false
                return .idle
            }
            if plan.storesToken {
                // After the commit: a failed ledger write must not leave the
                // Keychain claiming a token the ledger never versioned, or the
                // rotation would look already sent and never be replayed.
                do { try store.saveToken(token, for: .pushToStart(session.installationID)) } catch { return .failClosed }
            }
            let registration: LiveActivityDeviceRegistration
            do {
                registration = try LiveActivityDeviceRegistration(
                    installationID: session.installationID, registrationVersion: plan.version, apnsEnvironment: context.environment,
                    pushToStartToken: token, feedBreakMinutes: context.feedBreakMinutes, foregroundInventory: plan.inventory
                )
            } catch {
                return .failClosed
            }
            do {
                let receipt = try await session.client.registerDevice(registration)
                guard isCurrent(session) else { return .idle }
                guard receipt.serverInstanceID == context.endpoint.serverInstanceID, receipt.recoveryEpoch == context.recoveryEpoch,
                      receipt.acceptedRegistrationVersion == plan.version else { return .failClosed }
                try store.update { ledger in
                    accept(receipt, plan: plan, family: context.familyKey.familyID, installation: session.installationID, ledger: &ledger)
                }
                replayDevice = false
                return .idle
            } catch LiveActivityPushClientError.versionConflict {
                guard isCurrent(session) else { return .idle }
                dropRefusedInventory(plan)
                conflicts += 1
                guard conflicts <= 1 else { return .failClosed }
                // Another writer, or a server already past this version:
                // this exact version is spent, so the next send takes a newer one.
                try? store.update { ledger in
                    if ledger.device?.version == plan.version { ledger.device?.superseded = true }
                }
            } catch {
                // Only a refusal of the request itself; a missing credential
                // says nothing about the proof.
                if case LiveActivityPushClientError.rejected = error, isCurrent(session) { dropRefusedInventory(plan) }
                return outcome(of: error)
            }
        }
        return .idle
    }

    /// A proof the server refused outright is not retried: carried on every
    /// later registration it would block this family's device for good. Its
    /// inventory version stays spent, so any later proof is strictly newer.
    private func dropRefusedInventory(_ plan: DevicePlan) {
        guard let refused = plan.inventory else { return }
        try? store.update { ledger in
            guard ledger.pendingInventory == refused else { return }
            ledger.pendingInventory = nil
            ledger.pendingInventoryFamilyID = nil
        }
    }

    /// Decides the version under the ledger lock: the same version only for
    /// the identical request that was not yet accepted (or is being replayed),
    /// a newly allocated one for anything else.
    private func planDevice(_ ledger: inout LiveActivityRegistrationLedger, session: Session, token: String) throws -> DevicePlan? {
        guard ledger.installationID == session.installationID else { throw LiveActivityRegistrationStore.StoreError.unreadable }
        let context = session.context
        let family = context.familyKey.familyID
        let sentToken = try store.token(for: .pushToStart(session.installationID))
        // A proof is only ever sent to the family whose receipt it answers.
        let inventory = ledger.pendingInventoryFamilyID == family ? ledger.pendingInventory : nil
        if let device = ledger.device, !device.revoked, !device.superseded, device.familyID == family,
           device.environment == context.environment, device.feedBreakMinutes == context.feedBreakMinutes, sentToken == token {
            guard !device.accepted || replayDevice || inventory != nil else { return nil }
            return DevicePlan(version: device.version, inventory: inventory)
        }
        ledger.registrationVersion += 1
        ledger.device = .init(familyID: family, version: ledger.registrationVersion, environment: context.environment,
                              feedBreakMinutes: context.feedBreakMinutes, accepted: false)
        return DevicePlan(version: ledger.registrationVersion, inventory: inventory, storesToken: true)
    }

    private func accept(_ receipt: LiveActivityRegistrationReceipt, plan: DevicePlan, family: UUID, installation: UUID,
                        ledger: inout LiveActivityRegistrationLedger) {
        // Another process may have moved on while this was in flight.
        guard ledger.installationID == installation, var device = ledger.device,
              device.version == plan.version, device.familyID == family else { return }
        device.accepted = true
        ledger.device = device
        ledger.unconfirmedStarts = receipt.unconfirmedStarts.filter { $0.scope.familyID == family }
        if let sent = plan.inventory, ledger.pendingInventory == sent { ledger.pendingInventory = nil }
        if ledger.pendingInventoryFamilyID != family {
            ledger.pendingInventory = nil
            ledger.pendingInventoryFamilyID = nil
        }
        // The server's installation fence has retired every other family's
        // rows: their removals and cards are no longer this device's to send.
        ledger.pendingRemovals.removeAll { $0.installationID == installation && $0.familyID != family }
        for (id, activity) in ledger.activities where activity.scope.familyID != family {
            ledger.activities[id] = nil
            try? store.deleteToken(for: .activity(installation, id))
        }
    }
}
