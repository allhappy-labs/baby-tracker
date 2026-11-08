import BabyCore
import BabyProtocol
import CryptoKit
import Foundation

/// The two push-token routes, `PUT` and `DELETE /v1/devices/push-token`.
public protocol PushTokenTransport: Sendable {
    func putPushToken(_ request: PushTokenRequest) async throws
    func deletePushToken() async throws
}

/// The identity a push token is registered under: which server, which account,
/// which device, which family. A change in any of them is a new registration.
public struct PushSession: Sendable, Equatable {
    public let serverInstanceID: UUID, accountID: UUID, deviceID: UUID, familyID: UUID

    public init(serverInstanceID: UUID, accountID: UUID, deviceID: UUID, familyID: UUID) {
        self.serverInstanceID = serverInstanceID
        self.accountID = accountID
        self.deviceID = deviceID
        self.familyID = familyID
    }

    public init(_ key: FamilySessionKey) {
        self.init(serverInstanceID: key.serverInstanceID, accountID: key.accountID, deviceID: key.deviceID, familyID: key.familyID)
    }
}

/// How a family session became active.
///
/// `.signedIn`: sign-in, create, join or re-authentication; the server may have
/// dropped the token with the old session, so upload whatever the fingerprint says.
/// `.restored`: launch or an engine restart; upload only if the fingerprint changed.
public enum PushActivation: Sendable, Equatable { case signedIn, restored }

/// Keeps the server's copy of this device's APNs token current.
///
/// It uploads when four things hold: it has a device token, a family session
/// is active, the server advertises `push-token-v1`, and the build is
/// provisioned for push (`PushEnvironmentResolving` answers an environment).
///
/// What the server already has is remembered as one fingerprint —
/// `SHA256(token ‖ environment ‖ serverInstanceID ‖ accountID ‖ deviceID ‖ familyID)`
/// in App Group defaults — so a launch with nothing new costs no request. The
/// raw token is never stored or logged. A `.signedIn` activation forgets the
/// fingerprint first: the server's hourly sweep deletes the tokens of devices
/// that had no family session, so after a sign-in, a join or a
/// re-authentication what the phone remembers uploading says nothing about
/// what the server still holds. A failed upload stores nothing, so the next
/// activation or token tries again.
///
/// One pass at a time: a token that arrives while an upload is in flight
/// waits for it and then reconciles once more.
@MainActor
public final class PushTokenRegistrar {
    static let fingerprintKey = "push.uploaded-fingerprint"

    private let environment: any PushEnvironmentResolving
    private let defaults: UserDefaults
    private let capabilities: @Sendable () async throws -> [String]
    private let endingDeadline: Duration

    private var token: Data?
    private var session: PushSession?
    private var transport: (any PushTokenTransport)?
    private var running: Task<Void, Never>?
    private var needsAnotherPass = false

    /// - Parameter endingDeadline: how long sign-out waits for the `DELETE`.
    ///   The server's sweep removes a signed-out device's token anyway, so a
    ///   slow network must not hold up leaving a family.
    public init(environment: any PushEnvironmentResolving, defaults: UserDefaults,
                capabilities: @escaping @Sendable () async throws -> [String],
                endingDeadline: Duration = .seconds(5)) {
        self.environment = environment
        self.defaults = defaults
        self.capabilities = capabilities
        self.endingDeadline = endingDeadline
    }

    /// application(_:didRegisterForRemoteNotificationsWithDeviceToken:)
    public func deviceTokenReceived(_ token: Data) async {
        self.token = token
        await reconcile()
    }

    /// After a family session becomes active. Remembers `transport` for `sessionEnding`.
    public func sessionActivated(_ session: PushSession, transport: any PushTokenTransport, activation: PushActivation) async {
        self.session = session
        self.transport = transport
        if activation == .signedIn { defaults.removeObject(forKey: Self.fingerprintKey) }
        await reconcile()
    }

    /// An upload is owed: a sign-in, a join or a re-authentication has begun,
    /// and its `.signedIn` activation may not run in this process (it can
    /// fail, and the app can be killed). Forgetting now means the next
    /// activation of any kind uploads.
    public func forgetUploadedFingerprint() {
        defaults.removeObject(forKey: Self.fingerprintKey)
    }

    /// Before sign-out, leaving or removal: DELETE through the remembered transport, then forget the fingerprint.
    public func sessionEnding() async {
        let transport = self.transport
        let inFlight = running
        // From here on nothing uploads for the ending session, whatever
        // arrives while the DELETE is out.
        session = nil
        self.transport = nil
        let capabilities = self.capabilities
        _ = await MainActorDeadline.first(within: endingDeadline) {
            // An upload already on the wire lands first, so it cannot
            // re-register the token after the DELETE.
            await inFlight?.value
            guard let transport, Self.advertisesPush(try? await capabilities()) else { return }
            do { try await transport.deletePushToken() } catch {
                AppLog.sync.error("push token delete failed: \(Self.logKind(of: error), privacy: .public)")
            }
        }
        defaults.removeObject(forKey: Self.fingerprintKey)
    }

    // MARK: Internals

    private func reconcile() async {
        if let running {
            needsAnotherPass = true
            await running.value
            return
        }
        let pass = Task { @MainActor [weak self] in
            guard let self else { return }
            repeat {
                self.needsAnotherPass = false
                await self.uploadIfOwed()
            } while self.needsAnotherPass
            // In the same turn as the last check, so a caller arriving now
            // starts a fresh pass rather than waiting on a finished one.
            self.running = nil
        }
        running = pass
        await pass.value
    }

    private func uploadIfOwed() async {
        guard let token, let session, let transport, let environment = environment.environment() else { return }
        let fingerprint = Self.fingerprint(token: token, environment: environment, session: session)
        guard defaults.string(forKey: Self.fingerprintKey) != fingerprint else { return }
        guard Self.advertisesPush(try? await capabilities()), self.session == session else { return }
        do {
            try await transport.putPushToken(PushTokenRequest(token: token, environment: environment))
        } catch {
            AppLog.sync.error("push token upload failed: \(Self.logKind(of: error), privacy: .public)")
            return
        }
        // Only if it is still the session that was uploaded for: one that
        // ended meanwhile must not leave a fingerprint behind.
        if self.session == session { defaults.set(fingerprint, forKey: Self.fingerprintKey) }
    }

    private static func advertisesPush(_ capabilities: [String]?) -> Bool {
        capabilities?.contains(PushTokenRequest.capability) == true
    }

    static func fingerprint(token: Data, environment: PushEnvironment, session: PushSession) -> String {
        var input = token
        input.append(Data(environment.rawValue.utf8))
        for id in [session.serverInstanceID, session.accountID, session.deviceID, session.familyID] {
            input.append(Data(id.uuidString.lowercased().utf8))
        }
        return SHA256.hash(data: input).map { String(format: "%02x", $0) }.joined()
    }

    private static func logKind(of error: any Error) -> String { SyncLogKind.of(error) }
}
