import Foundation

/// Dials `/v1/live`. The seam between the socket runner and the network, so
/// every reconnect rule can be tested against a scripted fake.
public protocol LiveSocketConnecting: Sendable {
    /// Returns once the upgrade has succeeded — proved by a first pong — or
    /// throws a `LiveSocketError`.
    func open(token: String) async throws -> any LiveSocket
}

/// One open `/v1/live` socket.
public protocol LiveSocket: Sendable {
    /// The next frame's bytes. Throws `LiveSocketError.closed` when the
    /// server closed the socket, `.network` when the connection just died.
    func receive() async throws -> Data
    /// Resolves on the pong.
    func ping() async throws
    func close(_ code: URLSessionWebSocketTask.CloseCode)
}

/// Why a dial or an open socket failed, in the terms the runner decides on.
public enum LiveSocketError: Error, Sendable, Equatable {
    /// 401: the access token expired or the session ended.
    case unauthorized
    /// 403: role, membership or entitlement.
    case forbidden
    /// 429: rate or capacity, with the server's `Retry-After` (5 s if absent).
    case rateLimited(TimeInterval)
    /// 400/404/426: this server or proxy does not do `/v1/live`.
    case unsupported
    /// 503 or another 5xx.
    case unavailable
    case closed(code: Int, reason: String)
    case network
}

/// The real dialler, over the same `URLSession` the sync transport uses so
/// the e2e suite's pinned-certificate delegate covers the socket too.
public struct URLSessionLiveSocketConnector: LiveSocketConnecting {
    /// Hints are ~250 bytes. Anything near this is not a hint.
    static let maximumMessageSize = 4096
    /// What the server sends with a capacity refusal; used when it is absent.
    static let defaultRetryAfter: TimeInterval = 5

    private let endpoint: ConfirmedEndpoint
    private let session: URLSession

    public init(endpoint: ConfirmedEndpoint, session: URLSession = .shared) {
        self.endpoint = endpoint
        self.session = session
    }

    public func open(token: String) async throws -> any LiveSocket {
        let task = session.webSocketTask(with: Self.request(origin: endpoint.origin, token: token))
        task.maximumMessageSize = Self.maximumMessageSize
        task.resume()
        // No delegate is needed to learn the upgrade succeeded: a pong can
        // only come back over an open socket. A refused upgrade fails the
        // ping instead, and the refusal's status is on `task.response`.
        do {
            try await withTaskCancellationHandler {
                try await Self.awaitPong { task.sendPing(pongReceiveHandler: $0) }
            } onCancel: {
                task.cancel(with: .goingAway, reason: nil)
            }
        } catch {
            task.cancel()
            throw Self.upgradeError(response: task.response)
        }
        return URLSessionLiveSocket(task: task)
    }

    /// Waits for the first call of a pong handler. Only the first counts:
    /// URLSession may call it more than once on a failed connection, and
    /// resuming a continuation twice crashes the app.
    static func awaitPong(_ send: (@escaping @Sendable ((any Error)?) -> Void) -> Void) async throws {
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, any Error>) in
            let once = ResumeOnce(continuation)
            send { error in once.resume(with: error.map { .failure($0) } ?? .success(())) }
        }
    }

    /// Exactly what `/v1/live` checks. The token rides in the header only —
    /// the server refuses a query or a cookie — and there is no `Origin`: a
    /// native client may leave it out, and one that is not byte-for-byte the
    /// canonical origin is a 400.
    static func request(origin: ServerOrigin, token: String) -> URLRequest {
        let url = URL(string: "wss" + origin.canonicalString.dropFirst("https".count) + "/v1/live")!
        var request = URLRequest(url: url)
        request.httpShouldHandleCookies = false
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        request.setValue("v1", forHTTPHeaderField: "X-Baby-Tracker-Protocol")
        request.setValue(
            FamilySyncWebSocket.capabilities.joined(separator: ","),
            forHTTPHeaderField: "X-Baby-Tracker-Capabilities"
        )
        request.setValue(FamilySyncWebSocket.subprotocol, forHTTPHeaderField: "Sec-WebSocket-Protocol")
        return request
    }

    /// A failed upgrade with no HTTP answer at all never reached the server.
    static func upgradeError(response: URLResponse?) -> LiveSocketError {
        guard let http = response as? HTTPURLResponse else { return .network }
        return error(status: http.statusCode, retryAfter: http.value(forHTTPHeaderField: "Retry-After"))
    }

    /// Anything unrecognised backs off rather than stopping: a status this
    /// build has never heard of is likelier a passing proxy hiccup than a
    /// server that will never have the route.
    static func error(status: Int, retryAfter: String?) -> LiveSocketError {
        switch status {
        case 401: .unauthorized
        case 403: .forbidden
        case 429: .rateLimited(retryAfter.flatMap(TimeInterval.init).map { max(0, $0) } ?? defaultRetryAfter)
        case 400, 404, 426: .unsupported
        default: .unavailable
        }
    }
}

/// An open `URLSessionWebSocketTask`, translating its failures.
final class URLSessionLiveSocket: LiveSocket, @unchecked Sendable {
    private let task: URLSessionWebSocketTask

    init(task: URLSessionWebSocketTask) { self.task = task }

    func receive() async throws -> Data {
        let message: URLSessionWebSocketTask.Message
        do { message = try await task.receive() } catch { throw failure() }
        switch message {
        case .string(let text): return Data(text.utf8)
        case .data(let data): return data
        @unknown default: throw LiveSocketError.network
        }
    }

    func ping() async throws {
        do {
            try await URLSessionLiveSocketConnector.awaitPong { task.sendPing(pongReceiveHandler: $0) }
        } catch {
            throw failure()
        }
    }

    func close(_ code: URLSessionWebSocketTask.CloseCode) {
        task.cancel(with: code, reason: nil)
    }

    /// The server's close code when it sent one — 1000 at the socket's
    /// lifetime, 1008 on an authorization change — because the runner reacts
    /// to each differently. Otherwise the connection simply died.
    private func failure() -> LiveSocketError {
        guard task.closeCode != .invalid else { return .network }
        let reason = task.closeReason.map { String(decoding: $0, as: UTF8.self) } ?? ""
        return .closed(code: task.closeCode.rawValue, reason: reason)
    }
}

/// A continuation that can be offered its result any number of times and
/// takes only the first.
final class ResumeOnce<Value: Sendable>: @unchecked Sendable {
    private let lock = NSLock()
    private var continuation: CheckedContinuation<Value, any Error>?

    init(_ continuation: CheckedContinuation<Value, any Error>) { self.continuation = continuation }

    func resume(with result: Result<Value, any Error>) {
        lock.lock()
        let pending = continuation
        continuation = nil
        lock.unlock()
        pending?.resume(with: result)
    }
}
