import BabyProtocol
import CryptoKit
import Foundation

public enum ExportClientError: Error, Sendable, Equatable {
    case notReady, expired, ownerRequired, requesterMismatch
    case rateLimited(retryAfter: TimeInterval)
    case integrity, tooLarge, authorizationChanged
    case server(String)
}

public protocol ExportServing: Sendable {
    func request(requestID: UUID) async throws -> ExportStatusDTO
    func status(id: UUID) async throws -> ExportStatusDTO
    func download(id: UUID) async throws -> URL
    func removeTemporaryFile(_ url: URL) throws
    func removeTemporaryFiles() throws
}

/// Authenticated owner export transport. A new token is fetched for every call.
public final class ExportClient: ExportServing, @unchecked Sendable {
    public static let maximumPlaintextBytes = 52_428_800

    private let endpoint: ConfirmedEndpoint
    private let tokens: any SessionTokenProviding
    private let session: URLSession
    private let files: ExportTemporaryFiles

    public init(
        endpoint: ConfirmedEndpoint,
        tokens: any SessionTokenProviding,
        session: URLSession = .shared,
        files: ExportTemporaryFiles? = nil
    ) throws {
        self.endpoint = endpoint
        self.tokens = tokens
        self.session = session
        self.files = try files ?? ExportTemporaryFiles()
        // A prior share may have been interrupted by process death.
        try self.files.removeAll()
    }

    public func request(requestID: UUID = UUID()) async throws -> ExportStatusDTO {
        let request = try await makeRequest("POST", path: "export", body: CreateExportRequest(requestID: requestID))
        return try await sendStatus(request)
    }

    public func status(id: UUID) async throws -> ExportStatusDTO {
        let request = try await makeRequest("GET", path: "export/\(id.uuidString.lowercased())")
        return try await sendStatus(request)
    }

    public func download(id: UUID) async throws -> URL {
        let ready = try await status(id: id)
        guard ready.exportID == id, ready.protocolVersion == "v1" else { throw ExportClientError.integrity }
        guard ready.state == .ready else {
            throw ready.state == .expired ? ExportClientError.expired : ExportClientError.notReady
        }
        guard let expectedLength = ready.plaintextBytes,
              expectedLength <= Self.maximumPlaintextBytes,
              let expectedHash = ready.sha256, expectedHash.count == 64 else {
            throw ExportClientError.integrity
        }

        let request = try await makeRequest("GET", path: "export/\(id.uuidString.lowercased())/download")
        let (bytes, response) = try await session.bytes(for: request)
        guard let http = response as? HTTPURLResponse else { throw ExportClientError.integrity }
        if !(200..<300).contains(http.statusCode) {
            var errorBody = Data()
            for try await byte in bytes {
                guard errorBody.count < 4_096 else { break }
                errorBody.append(byte)
            }
            _ = try Self.validatedHTTP(http, data: errorBody)
            throw ExportClientError.integrity
        }
        guard http.value(forHTTPHeaderField: "Content-Type")?.lowercased().hasPrefix("application/json") == true,
              let advertisedLength = http.value(forHTTPHeaderField: "Content-Length"),
              UInt64(advertisedLength) == expectedLength else {
            throw ExportClientError.integrity
        }
        let file = try files.createFile()
        do {
            let handle = try FileHandle(forWritingTo: file)
            defer { try? handle.close() }
            var digest = SHA256()
            var buffer = Data()
            buffer.reserveCapacity(65_536)
            var received: UInt64 = 0
            for try await byte in bytes {
                try Task.checkCancellation()
                received += 1
                guard received <= expectedLength, received <= Self.maximumPlaintextBytes else {
                    throw ExportClientError.tooLarge
                }
                buffer.append(byte)
                if buffer.count == 65_536 {
                    digest.update(data: buffer)
                    try handle.write(contentsOf: buffer)
                    buffer.removeAll(keepingCapacity: true)
                }
            }
            if !buffer.isEmpty {
                digest.update(data: buffer)
                try handle.write(contentsOf: buffer)
            }
            guard received == expectedLength,
                  digest.finalize().map({ String(format: "%02x", $0) }).joined() == expectedHash else {
                throw ExportClientError.integrity
            }
            try Task.checkCancellation()
            return file
        } catch {
            try? files.remove(file)
            throw error
        }
    }

    public func removeTemporaryFile(_ url: URL) throws { try files.remove(url) }
    public func removeTemporaryFiles() throws { try files.removeAll() }

    private func makeRequest<Body: Encodable>(_ method: String, path: String, body: Body? = nil) async throws -> URLRequest {
        let url = URL(string: endpoint.origin.canonicalString + "/v1/" + path)!
        var request = URLRequest(url: url)
        request.httpMethod = method
        request.setValue("no-store", forHTTPHeaderField: "Cache-Control")
        request.setValue("v1", forHTTPHeaderField: "X-Baby-Tracker-Protocol")
        request.setValue("export-v1", forHTTPHeaderField: "X-Baby-Tracker-Capabilities")
        guard let token = await tokens.currentAccessToken() else { throw ExportClientError.authorizationChanged }
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        if let body {
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONEncoder().encode(body)
        }
        return request
    }

    private func makeRequest(_ method: String, path: String) async throws -> URLRequest {
        try await makeRequest(method, path: path, body: nil as Never?)
    }

    private func sendStatus(_ request: URLRequest) async throws -> ExportStatusDTO {
        let (data, response) = try await session.data(for: request)
        _ = try Self.validatedHTTP(response, data: data)
        let status = try JSONDecoder().decode(ExportStatusDTO.self, from: data)
        guard status.protocolVersion == "v1" else { throw ExportClientError.integrity }
        return status
    }

    @discardableResult
    private static func validatedHTTP(_ response: URLResponse, data: Data) throws -> HTTPURLResponse {
        guard let http = response as? HTTPURLResponse else { throw ExportClientError.integrity }
        guard (200..<300).contains(http.statusCode) else {
            let code = ServerErrorEnvelope.decode(data) ?? "export_unavailable"
            switch code {
            case "export_not_ready": throw ExportClientError.notReady
            case "export_expired": throw ExportClientError.expired
            case "owner_required": throw ExportClientError.ownerRequired
            case "export_requester_mismatch": throw ExportClientError.requesterMismatch
            case "export_too_large": throw ExportClientError.tooLarge
            case "export_integrity": throw ExportClientError.integrity
            case "export_authorization_changed": throw ExportClientError.authorizationChanged
            case "export_not_found": throw ExportClientError.requesterMismatch
            case "export_rate_limited", "rate_limited":
                let retry = TimeInterval(http.value(forHTTPHeaderField: "Retry-After") ?? "") ?? 0
                throw ExportClientError.rateLimited(retryAfter: retry)
            default: throw ExportClientError.server(code)
            }
        }
        return http
    }
}
