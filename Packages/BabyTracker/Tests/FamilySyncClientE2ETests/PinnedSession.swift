import Foundation

/// The live server's address and one of its sessions, handed over by
/// `protocol-e2e.sh`.
///
/// Absent when the suite is run without that script, which is every ordinary
/// `swift test`. The tests then return without asserting anything rather than
/// failing: the gate that owns the container is the one that can decide a
/// missing server is a problem.
struct E2EEnvironment: Sendable {
    let baseURL: String
    let serverInstanceID: UUID
    let certificate: Data
    let outsiderAccessToken: String
    let outsiderRefreshToken: String
    /// The three parts of the session the server never repeats back.
    let outsiderIdentity: SessionIdentityFields
    /// The bootstrap's `SessionResponse` for each account, exactly as the
    /// server sent it, keyed by the name the state file uses. The
    /// two-caregiver test reads `Outsider2`'s account and device from here to
    /// prove its own Apple sign-in found that account again from a new device.
    let sessions: [String: Data]

    static func current() -> E2EEnvironment? {
        let variables = ProcessInfo.processInfo.environment
        guard let base = variables["BABY_TRACKER_E2E_BASE"],
              let statePath = variables["BABY_TRACKER_E2E_STATE"],
              let certificatePath = variables["BABY_TRACKER_E2E_CERT"],
              let instance = variables["BABY_TRACKER_E2E_INSTANCE"].flatMap(UUID.init(uuidString:)),
              let stateData = try? Data(contentsOf: URL(fileURLWithPath: statePath)),
              let certificate = try? Data(contentsOf: URL(fileURLWithPath: certificatePath)),
              let state = try? JSONSerialization.jsonObject(with: stateData) as? [String: Any],
              let outsider = state["Outsider"] as? [String: Any],
              let token = outsider["access_token"] as? String,
              let refresh = outsider["refresh_token"] as? String,
              let account = outsider["account"] as? [String: Any],
              let accountID = (account["id"] as? String).flatMap(UUID.init(uuidString:)),
              let deviceID = (outsider["device_id"] as? String).flatMap(UUID.init(uuidString:))
        else { return nil }
        let epoch = (outsider["recovery_epoch"] as? NSNumber)?.uint64Value ?? 0
        var sessions: [String: Data] = [:]
        for name in ["Outsider", "Outsider2"] {
            if let object = state[name], let data = try? JSONSerialization.data(withJSONObject: object) {
                sessions[name] = data
            }
        }
        return E2EEnvironment(
            baseURL: base, serverInstanceID: instance,
            certificate: certificate, outsiderAccessToken: token, outsiderRefreshToken: refresh,
            outsiderIdentity: SessionIdentityFields(accountID: accountID, deviceID: deviceID, recoveryEpoch: epoch),
            sessions: sessions
        )
    }
}

struct SessionIdentityFields: Sendable {
    let accountID: UUID
    let deviceID: UUID
    let recoveryEpoch: UInt64
}

/// A URLSession that trusts exactly one certificate and nothing else.
///
/// Pinned rather than permissive. `ServerOrigin` refuses any scheme but https,
/// so this suite has to speak real TLS to a container with a self-signed leaf;
/// the lazy way to get there is to accept whatever is presented, and that would
/// make a test that cannot tell the right server from any other. This compares
/// the leaf's DER bytes to the certificate the script generated and rejects a
/// mismatch, which is stricter than the production client is on a public CA.
final class PinnedCertificateDelegate: NSObject, URLSessionDelegate, @unchecked Sendable {
    private let expected: Data

    init(pinning certificate: Data) {
        // The file on disk is PEM; the trust object hands back DER.
        let text = String(decoding: certificate, as: UTF8.self)
        let body = text
            .replacingOccurrences(of: "-----BEGIN CERTIFICATE-----", with: "")
            .replacingOccurrences(of: "-----END CERTIFICATE-----", with: "")
            .replacingOccurrences(of: "\n", with: "")
        expected = Data(base64Encoded: body) ?? Data()
    }

    func urlSession(
        _ session: URLSession,
        didReceive challenge: URLAuthenticationChallenge,
        completionHandler: @escaping @Sendable (URLSession.AuthChallengeDisposition, URLCredential?) -> Void
    ) {
        let (disposition, credential) = evaluate(challenge)
        completionHandler(disposition, credential)
    }

    /// The same decision as a value, for `HTTPIdentityClient`, which owns its
    /// session and takes the trust decision as a closure instead.
    func evaluate(_ challenge: URLAuthenticationChallenge) -> (URLSession.AuthChallengeDisposition, URLCredential?) {
        guard challenge.protectionSpace.authenticationMethod == NSURLAuthenticationMethodServerTrust,
              let trust = challenge.protectionSpace.serverTrust,
              let chain = SecTrustCopyCertificateChain(trust) as? [SecCertificate],
              let leaf = chain.first,
              SecCertificateCopyData(leaf) as Data == expected
        else {
            return (.cancelAuthenticationChallenge, nil)
        }
        return (.useCredential, URLCredential(trust: trust))
    }

    static func session(pinning certificate: Data) -> URLSession {
        URLSession(
            configuration: .ephemeral,
            delegate: PinnedCertificateDelegate(pinning: certificate),
            delegateQueue: nil
        )
    }
}

/// What one phone's `URLSession` asked the server, for the live-hint step:
/// how many `POST /v1/sync` it sent — the only way to tell a hint-driven sync
/// from a poll is to count them — and how every `/v1/live` upgrade ended.
final class PhoneTraffic: @unchecked Sendable {
    /// One `/v1/live` dial: the HTTP status the upgrade got (101 when it
    /// opened), and the `Origin` the request carried as URLSession reports it.
    struct LiveDial: Sendable, CustomStringConvertible {
        let status: Int?
        let origin: String?
        var description: String { "\(status.map(String.init) ?? "no-response")\(origin.map { " origin=\($0)" } ?? "")" }
    }

    private let lock = NSLock()
    private var syncs = 0
    private var cursors: [UInt64?] = []
    private var dials: [LiveDial] = []

    var syncRequests: Int { lock.withLock { syncs } }
    var liveDials: [LiveDial] { lock.withLock { dials } }
    /// The cursor sequence every `POST /v1/sync` carried, in order: nil for
    /// `"cursor": null`, which asks the server for its journal from 0.
    var syncCursors: [UInt64?] { lock.withLock { cursors } }

    fileprivate func created(_ task: URLSessionTask) {
        guard task.originalRequest?.url?.path == "/v1/sync" else { return }
        let body = task.originalRequest?.httpBody.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] }
        let sequence = ((body?["cursor"] as? [String: Any])?["sequence"] as? NSNumber)?.uint64Value
        lock.withLock {
            syncs += 1
            cursors.append(sequence)
        }
    }

    fileprivate func dialled(_ task: URLSessionTask, status: Int?) {
        guard task.originalRequest?.url?.path == "/v1/live" else { return }
        let origin = task.currentRequest?.value(forHTTPHeaderField: "Origin")
        lock.withLock { dials.append(LiveDial(status: status, origin: origin)) }
    }
}

/// `PinnedCertificateDelegate`'s trust decision, plus a record of the traffic.
final class RecordingPinnedDelegate: NSObject, URLSessionWebSocketDelegate, @unchecked Sendable {
    private let pin: PinnedCertificateDelegate
    let traffic = PhoneTraffic()

    init(pinning certificate: Data) {
        pin = PinnedCertificateDelegate(pinning: certificate)
    }

    func urlSession(
        _ session: URLSession,
        didReceive challenge: URLAuthenticationChallenge,
        completionHandler: @escaping @Sendable (URLSession.AuthChallengeDisposition, URLCredential?) -> Void
    ) {
        let (disposition, credential) = pin.evaluate(challenge)
        completionHandler(disposition, credential)
    }

    func urlSession(_ session: URLSession, didCreateTask task: URLSessionTask) {
        traffic.created(task)
    }

    func urlSession(_ session: URLSession, webSocketTask: URLSessionWebSocketTask, didOpenWithProtocol protocol: String?) {
        traffic.dialled(webSocketTask, status: (webSocketTask.response as? HTTPURLResponse)?.statusCode ?? 101)
    }

    /// A refused upgrade ends here, with the refusal's status on the task.
    /// An opened socket ends here too, later, and was recorded on open.
    func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: (any Error)?) {
        guard let socket = task as? URLSessionWebSocketTask, socket.closeCode == .invalid,
              (task.response as? HTTPURLResponse)?.statusCode != 101 else { return }
        traffic.dialled(task, status: (task.response as? HTTPURLResponse)?.statusCode)
    }
}
