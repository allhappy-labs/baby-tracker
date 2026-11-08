import BabyProtocol
import Foundation

/// One request the server's APNs client made, as the identitytest recorder
/// took it (`GET /__identitytest/apns`, main's recorder JSON). In the
/// disposable push profile the recorder is the client's whole transport: it
/// never dials, and Apple's hosts are replaced by names that cannot resolve.
struct APNSRecord: Decodable, Sendable {
    let sequence: Int
    let host: String
    let path: String
    let token: String
    let headers: [String: String]
    /// The ES256 provider token verified against the mounted key's public half.
    let providerTokenValid: Bool
    let acknowledged: Bool
    let payload: JSONValue

    enum CodingKeys: String, CodingKey {
        case sequence, host, path, token, headers, acknowledged, payload
        case providerTokenValid = "provider_token_valid"
    }
}

/// Reads the server's APNs recorder through a session pinned to the
/// harness's certificate, as every other transport in this suite is.
struct APNSRecorder: Sendable {
    private let base: String
    private let session: URLSession

    init(environment: E2EEnvironment) {
        base = environment.baseURL
        session = PinnedCertificateDelegate.session(pinning: environment.certificate)
    }

    /// Every push the server has made so far, in order.
    func records() async throws -> [APNSRecord] {
        guard let url = URL(string: base + "/__identitytest/apns") else { throw E2EFailure("bad recorder URL") }
        let (data, response) = try await session.data(from: url)
        let status = (response as? HTTPURLResponse)?.statusCode ?? 0
        guard status == 200 else {
            throw E2EFailure("the APNs recorder answered \(status): is the push profile on? \(String(decoding: data.prefix(200), as: UTF8.self))")
        }
        struct Envelope: Decodable { let requests: [APNSRecord] }
        return try JSONDecoder().decode(Envelope.self, from: data).requests
    }

    /// Every push to `token` so far.
    func records(token: String) async throws -> [APNSRecord] {
        try await records().filter { $0.token == token }
    }

    /// The wake worker's successful ticks so far (`GET /__identitytest/wake`),
    /// each counted when it returns. A failed tick restores its work and
    /// sends nothing, so it is not counted.
    func wakeTicks() async throws -> UInt64 {
        guard let url = URL(string: base + "/__identitytest/wake") else { throw E2EFailure("bad wake URL") }
        let (data, response) = try await session.data(from: url)
        let status = (response as? HTTPURLResponse)?.statusCode ?? 0
        guard status == 200 else {
            throw E2EFailure("the wake tick route answered \(status): \(String(decoding: data.prefix(200), as: UTF8.self))")
        }
        struct Envelope: Decodable { let ticks: UInt64 }
        return try JSONDecoder().decode(Envelope.self, from: data).ticks
    }

    /// Waits until the wake worker has run a whole successful tick that began
    /// after this call: two more successful ticks returned (the first may
    /// have begun before it). That tick scanned everything committed before
    /// the call and sent what it found, so a push it would make has been
    /// made. A sleep past the 2-second interval proves nothing on a loaded
    /// machine, where the worker may not have ticked, or its tick may have
    /// failed.
    func waitForWholeWakeTick(timeout: Duration) async throws {
        let before = try await wakeTicks()
        let start = ContinuousClock.now
        while try await wakeTicks() < before + 2 {
            guard ContinuousClock.now - start < timeout else {
                throw E2EFailure("the wake worker ran no whole successful tick within \(timeout) (ticks before: \(before))")
            }
            try await Task.sleep(for: .milliseconds(100))
        }
    }

    /// The first push to `token`, polling until it arrives or `timeout` runs out.
    func waitForPush(token: String, timeout: Duration) async throws -> APNSRecord {
        let start = ContinuousClock.now
        while true {
            if let push = try await records(token: token).first { return push }
            guard ContinuousClock.now - start < timeout else {
                throw E2EFailure("no push to the registered token within \(timeout)")
            }
            try await Task.sleep(for: .milliseconds(200))
        }
    }
}
