import BabyProtocol
import BabyStore
import Foundation

/// The three registration calls, as the coordinator needs them.
public protocol LiveActivityPushRegistering: Sendable {
    func registerDevice(_ registration: LiveActivityDeviceRegistration) async throws -> LiveActivityRegistrationReceipt
    func registerActivity(_ registration: LiveActivityTokenRegistration) async throws -> LiveActivityRegistrationReceipt
    func unregisterDevice(installationID: UUID, version: Int64) async throws
}

/// What went wrong, in the terms the coordinator acts on.
public enum LiveActivityPushClientError: Error, Sendable, Equatable {
    /// 409 `live_activity_version_conflict`: not newer than the server's.
    case versionConflict
    /// 403 `live_activity_registration_denied`: the server will not hold this
    /// registration for this session.
    case registrationDenied
    /// No usable family credential: none stored, a 401 that survives the
    /// fenced renewal, or a renewal refused outright — fresh proof required,
    /// the account gate cancelled, the session revoked. A renewal that
    /// merely could not run now (the lease held by the sync engine's own
    /// renewal, offline, a 5xx) is `.unavailable`, so the registration is
    /// retried once the other renewal has finished.
    case unauthorized
    /// 5xx, a stopped push worker, no network, or a renewal that could not
    /// run yet.
    case unavailable
    case rateLimited
    /// Any other refusal, with the server's code. Not retried within the pass;
    /// never treated as the server having applied or refused the version.
    case rejected(String?)
    /// A receipt that does not belong to this server.
    case invalidReceipt

    public var isRetryable: Bool { self == .unavailable || self == .rateLimited }
}

/// `PUT /v1/live-activities/device`, `PUT /v1/live-activities/activity` and
/// `DELETE /v1/live-activities/device/{installation_id}` against one confirmed
/// endpoint.
///
/// Bound to one family's credentials for its whole life: the token provider
/// reads that family's Keychain item, so a client built for an old realm can
/// never send the new realm's token. A 401 first tries a credential another
/// renewal already verified, then the existing fenced renewer once.
public final class LiveActivityPushClient: LiveActivityPushRegistering, @unchecked Sendable {
    static let requestTimeout: TimeInterval = 15
    static let capability = LiveActivityContract.capability

    private let endpoint: ConfirmedEndpoint
    private let tokens: any SessionTokenProviding
    private let session: URLSession
    private let retryCredentials: (@Sendable (UUID) async throws -> SessionCredentials?)?
    private let renew: (@Sendable () async throws -> Void)?

    public init(
        endpoint: ConfirmedEndpoint,
        tokens: any SessionTokenProviding,
        session: URLSession = .shared,
        retryCredentials: (@Sendable (UUID) async throws -> SessionCredentials?)? = nil,
        renew: (@Sendable () async throws -> Void)? = nil
    ) {
        self.endpoint = endpoint
        self.tokens = tokens
        self.session = session
        self.retryCredentials = retryCredentials
        self.renew = renew
    }

    public func registerDevice(_ registration: LiveActivityDeviceRegistration) async throws -> LiveActivityRegistrationReceipt {
        try receipt(from: await send("PUT", path: "live-activities/device", body: try JSONEncoder().encode(registration)))
    }

    public func registerActivity(_ registration: LiveActivityTokenRegistration) async throws -> LiveActivityRegistrationReceipt {
        try receipt(from: await send("PUT", path: "live-activities/activity", body: try JSONEncoder().encode(registration)))
    }

    public func unregisterDevice(installationID: UUID, version: Int64) async throws {
        _ = try await send("DELETE", path: "live-activities/device/\(installationID.uuidString.lowercased())?version=\(version)", body: nil)
    }

    private func receipt(from data: Data) throws -> LiveActivityRegistrationReceipt {
        guard let receipt = try? JSONDecoder().decode(LiveActivityRegistrationReceipt.self, from: data),
              receipt.serverInstanceID == endpoint.serverInstanceID else {
            throw LiveActivityPushClientError.invalidReceipt
        }
        return receipt
    }

    private func send(_ method: String, path: String, body: Data?) async throws -> Data {
        var credentials = await tokens.currentSessionCredentials()
        var accessToken: String? = if let credentials { credentials.accessToken } else { await tokens.currentAccessToken() }
        var renewed = false
        var retriedVerified = false
        while true {
            guard let token = accessToken else { throw LiveActivityPushClientError.unauthorized }
            var request = URLRequest(url: URL(string: endpoint.origin.canonicalString + "/v1/" + path)!)
            request.httpMethod = method
            request.timeoutInterval = Self.requestTimeout
            request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
            request.setValue("v1", forHTTPHeaderField: "X-Baby-Tracker-Protocol")
            request.setValue(Self.capability, forHTTPHeaderField: "X-Baby-Tracker-Capabilities")
            request.setValue("no-store", forHTTPHeaderField: "Cache-Control")
            if let body {
                request.setValue("application/json", forHTTPHeaderField: "Content-Type")
                request.httpBody = body
            }

            let data: Data
            let response: URLResponse
            do {
                (data, response) = try await session.data(for: request)
            } catch let error as URLError where error.code == .cancelled {
                throw CancellationError()
            } catch is CancellationError {
                throw CancellationError()
            } catch {
                throw LiveActivityPushClientError.unavailable
            }
            guard let http = response as? HTTPURLResponse else { throw LiveActivityPushClientError.unavailable }
            if (200..<300).contains(http.statusCode) { return data }
            guard http.statusCode == 401 else { throw Self.error(status: http.statusCode, data: data) }

            // Another process may already hold a newer verified credential.
            if !retriedVerified, let used = credentials, let retryCredentials {
                retriedVerified = true
                if let verified = try? await retryCredentials(used.credentialVersion) {
                    credentials = verified
                    accessToken = verified.accessToken
                    continue
                }
            }
            guard !renewed, let renew else { throw LiveActivityPushClientError.unauthorized }
            renewed = true
            do { try await renew() } catch is CancellationError { throw CancellationError() } catch {
                throw Self.renewalFailure(error)
            }
            credentials = await tokens.currentSessionCredentials()
            accessToken = if let credentials { credentials.accessToken } else { await tokens.currentAccessToken() }
        }
    }

    /// What a failed renewal means for the registration. Only a refusal that
    /// needs fresh proof — the renewer's own verdict, the account gate
    /// cancelled, the server refusing the refresh token — is terminal. The
    /// lease held by the sync engine's concurrent renewal
    /// (`RefreshError.lockUnavailable`), a dropped connection or a 5xx say
    /// nothing about the session: the next pass finds the renewed credential.
    static func renewalFailure(_ error: any Error) -> LiveActivityPushClientError {
        if let bound = error as? CredentialBoundAuthFailure {
            return bound.requiresFreshProof || SessionRefresher.requiresFreshProof(bound.underlying) ? .unauthorized : .unavailable
        }
        // An irreconcilable checkpoint (`RefreshRecoveryError`) is terminal
        // for the sync engine and `SessionRefresher.terminalReason` alike.
        return SessionRefresher.requiresFreshProof(error) || error is RefreshRecoveryError ? .unauthorized : .unavailable
    }

    static let versionConflictCode = "live_activity_version_conflict"
    static let registrationDeniedCode = "live_activity_registration_denied"
    /// 409 while the push worker is stopped: a refusal that says nothing
    /// about the request itself, and lifts when the worker is back.
    static let unsupportedCapabilityCode = "unsupported_capability"

    /// Only the server's dedicated Live Activity codes are verdicts about a
    /// registration (`httpapi/live_activity.go` writeLiveActivityError). The
    /// same statuses carry other codes too — 409 `unsupported_capability`
    /// while the push worker is stopped, 403 `membership_revoked` — and
    /// those say nothing about versions or ownership.
    static func error(status: Int, data: Data) -> LiveActivityPushClientError {
        let code = ServerErrorEnvelope.decode(data)
        switch (status, code) {
        case (409, versionConflictCode): return .versionConflict
        case (403, registrationDeniedCode): return .registrationDenied
        case (429, _): return .rateLimited
        case (500...599, _): return .unavailable
        default: return .rejected(code)
        }
    }
}
