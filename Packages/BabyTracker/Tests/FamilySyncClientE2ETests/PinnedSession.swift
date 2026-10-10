import Foundation
import Testing
import Network
import Security

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
    private let strictHostname: String?

    init(pinning certificate: Data, strictHostname: String? = nil) {
        self.strictHostname = strictHostname
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
        if let strictHostname {
            guard challenge.protectionSpace.host == strictHostname,
                  SecTrustSetPolicies(trust, SecPolicyCreateSSL(true, strictHostname as CFString)) == errSecSuccess,
                  SecTrustSetAnchorCertificates(trust, [leaf] as CFArray) == errSecSuccess,
                  SecTrustSetAnchorCertificatesOnly(trust, true) == errSecSuccess,
                  SecTrustEvaluateWithError(trust, nil) else { return (.cancelAuthenticationChallenge, nil) }
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

    /// One finished `/v1/devices/push-token` request: its method and the
    /// HTTP status it got (nil when no response came back).
    struct PushTokenCall: Sendable, Equatable, CustomStringConvertible {
        let method: String
        let status: Int?
        var description: String { "\(method) \(status.map(String.init) ?? "no-response")" }
    }

    private let lock = NSLock()
    private var syncs = 0
    private var cursors: [UInt64?] = []
    private var dials: [LiveDial] = []
    private var pushTokenTasks: [URLSessionTask] = []

    var syncRequests: Int { lock.withLock { syncs } }
    var liveDials: [LiveDial] { lock.withLock { dials } }
    /// Every push-token request the phone has finished, in the order it made
    /// them. Read from the tasks themselves: the async API that sends them
    /// does not report their completion to the session delegate.
    var pushTokenCalls: [PushTokenCall] {
        lock.withLock { pushTokenTasks }.filter { $0.state == .completed }.map {
            PushTokenCall(method: $0.originalRequest?.httpMethod ?? "GET", status: ($0.response as? HTTPURLResponse)?.statusCode)
        }
    }
    /// The cursor sequence every `POST /v1/sync` carried, in order: nil for
    /// `"cursor": null`, which asks the server for its journal from 0.
    var syncCursors: [UInt64?] { lock.withLock { cursors } }

    fileprivate func created(_ task: URLSessionTask) {
        if task.originalRequest?.url?.path == "/v1/devices/push-token" {
            lock.withLock { pushTokenTasks.append(task) }
            return
        }
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

    init(pinning certificate: Data, strictHostname: String? = nil) {
        pin = PinnedCertificateDelegate(pinning: certificate, strictHostname: strictHostname)
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


@Testing.Test func capacityProxyRejectsUnownedRoutes() throws {
    for invalid in ["remote.example:443", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:12/path", "https://127.0.0.1:12"] {
        #expect(throws: (any Error).self) { try CapacityTestRoute.configuration(endpoint: invalid) }
    }
    let configuration = try CapacityTestRoute.configuration(endpoint: "127.0.0.1:12345")
    #expect(configuration.proxyConfigurations.count == 1)
}


enum CapacityTestRoute {
    static var enabled: Bool { ProcessInfo.processInfo.environment["BABY_TRACKER_CAPACITY_CONNECT"] != nil }
    static func currentConfiguration() throws -> URLSessionConfiguration {
        guard let endpoint = ProcessInfo.processInfo.environment["BABY_TRACKER_CAPACITY_CONNECT"] else { return .ephemeral }
        return try configuration(endpoint: endpoint)
    }
    static func configuration(endpoint: String) throws -> URLSessionConfiguration {
        let parts = endpoint.split(separator: ":", omittingEmptySubsequences: false)
        guard parts.count == 2, parts[0] == "127.0.0.1", let number = UInt16(parts[1]), number > 0,
              String(number) == parts[1], let port = NWEndpoint.Port(rawValue: number) else { throw E2EFailure("invalid owned CONNECT route") }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.proxyConfigurations = [ProxyConfiguration(httpCONNECTProxy: .hostPort(host: "127.0.0.1", port: port))]
        configuration.timeoutIntervalForRequest = 10
        configuration.timeoutIntervalForResource = 20
        return configuration
    }
}


@Test func capacityProofSinkRetainsBeforeProcessExit() throws {
    let file = FileManager.default.temporaryDirectory.appending(path: "capacity-proof-\(UUID().uuidString).log")
    FileManager.default.createFile(atPath: file.path, contents: Data(), attributes: [.posixPermissions: 0o600])
    defer { try? FileManager.default.removeItem(at: file) }
    try CapacityProofSink.write("foreground-latency proof: retained", to: file.path)
    #expect(try String(contentsOf: file, encoding: .utf8) == "foreground-latency proof: retained\n")
    #expect(throws: (any Error).self) { try CapacityProofSink.write("credential arbitrary", to: file.path) }
}


enum CapacityProofSink {
    static func write(_ line: String, to path: String) throws {
        guard line.hasPrefix("foreground-latency "), line.utf8.count <= 16384 else { throw E2EFailure("invalid proof row") }
        let handle = try FileHandle(forWritingTo: URL(fileURLWithPath: path))
        defer { try? handle.close() }
        try handle.seekToEnd()
        try handle.write(contentsOf: Data((line + "\n").utf8))
        try handle.synchronize()
    }
}

func emitForegroundLine(_ line: String) {
    if let path = ProcessInfo.processInfo.environment["BABY_TRACKER_CAPACITY_PROOF"] {
        // This sink is outside measured pair intervals and survives a killed
        // SwiftPM wrapper. A failed sink cannot manufacture proof in the reader.
        try? CapacityProofSink.write(line, to: path)
    }
    print(line)
}
