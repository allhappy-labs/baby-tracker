import BabyCore
import BabyProtocol
import BabyStore
import Foundation

/// Taking a registration down: the revocation queued when Live Activities
/// are switched off, and the pending removals a pass sends first — only ever
/// under this session's family and installation.
extension LiveActivityPushCoordinator {
    /// Takes the registration down once the user has switched Live Activities
    /// off: a removal scoped to this family, with a newer version.
    func enqueueRevocation(_ session: Session) -> Bool {
        let family = session.context.familyKey.familyID
        return (try? store.update { ledger -> Bool in
            guard ledger.installationID == session.installationID, var device = ledger.device,
                  device.familyID == family, !device.revoked else { return false }
            ledger.registrationVersion += 1
            ledger.pendingRemovals.append(.init(familyID: family, installationID: session.installationID, version: ledger.registrationVersion))
            device.revoked = true
            device.accepted = false
            ledger.device = device
            return true
        }) ?? false
    }

    /// Only removals scoped to this session's family and installation: a
    /// removal never runs under another family's or account's credentials.
    func sendRemovals(_ session: Session) async -> Outcome {
        let family = session.context.familyKey.familyID
        guard let pending = try? store.read().pendingRemovals.filter({
            $0.familyID == family && $0.installationID == session.installationID
        }) else { return .failClosed }
        for removal in pending {
            guard isCurrent(session) else { return .idle }
            do {
                try await session.client.unregisterDevice(installationID: removal.installationID, version: removal.version)
            } catch LiveActivityPushClientError.versionConflict, LiveActivityPushClientError.registrationDenied {
                // Already superseded on the server, or nothing of ours to
                // remove: either way this removal is finished.
            } catch LiveActivityPushClientError.rejected(nil) {
                // A refusal with no envelope of the server's — a proxy's error
                // page, a truncated body: not this server's verdict on the
                // removal, which stays pending for the next trigger.
                return .failClosed
            } catch LiveActivityPushClientError.rejected(let code) where code == LiveActivityPushClient.unsupportedCapabilityCode {
                // A server that still ties its DELETE to a running push
                // worker: not a verdict on the removal, which stays pending
                // for the next trigger. The built-in server honours the
                // removal without push, so this is not expected from it.
                return .failClosed
            } catch LiveActivityPushClientError.rejected {
                // Refused as sent, with the server's own code, and it would be
                // refused the same way on every later pass — which would wedge
                // this family and installation for good: no device
                // registration, no card. Finished here; the server's own sweep
                // retires the row, and any later registration supersedes it
                // with a newer version.
                AppLog.sync.error("live activity removal refused; left to the server's sweep")
            } catch {
                return outcome(of: error)
            }
            guard isCurrent(session) else { return .idle }
            try? store.update { ledger in ledger.pendingRemovals.removeAll { $0 == removal } }
        }
        return .idle
    }
}
