import Foundation

public enum ExportState: String, Codable, Sendable {
    case queued, running, ready, failed, expired
}

public struct CreateExportRequest: Encodable, Sendable {
    public let protocolVersion: String
    public let capabilities: [String]
    public let requestID: UUID

    public init(requestID: UUID) {
        self.protocolVersion = "v1"
        self.capabilities = ["export-v1"]
        self.requestID = requestID
    }

    enum CodingKeys: String, CodingKey {
        case protocolVersion = "protocol_version", capabilities, requestID = "request_id"
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(protocolVersion, forKey: .protocolVersion)
        try container.encode(capabilities, forKey: .capabilities)
        try container.encode(requestID.uuidString.lowercased(), forKey: .requestID)
    }
}

public struct ExportStatusDTO: Decodable, Sendable, Equatable {
    public let protocolVersion: String
    public let exportID: UUID
    public let state: ExportState
    public let createdAt: String
    public let expiresAt: String
    public let plaintextBytes: UInt64?
    public let sha256: String?
    public let code: String?

    enum CodingKeys: String, CodingKey {
        case protocolVersion = "protocol_version", exportID = "export_id", state
        case createdAt = "created_at", expiresAt = "expires_at"
        case plaintextBytes = "plaintext_bytes", sha256, code
    }
}
