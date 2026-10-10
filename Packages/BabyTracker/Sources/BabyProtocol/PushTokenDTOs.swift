import Foundation

/// Which APNs gateway a device token belongs to. A token is only valid on the
/// gateway that issued it, so the server must know which one to send to.
public enum PushEnvironment: String, Codable, Sendable {
    case sandbox, production
}

/// The body of `PUT /v1/devices/push-token` (`push-token-v1`).
///
/// No device ID: the server binds the token to the authenticated session's own
/// device, so a phone can only ever register itself.
public struct PushTokenRequest: Codable, Sendable, Equatable {
    public static let capability = "push-token-v1"

    public let protocolVersion: String
    public let capabilities: [String]
    /// Lowercase hex, as the server validates it.
    public let token: String
    public let environment: PushEnvironment

    enum CodingKeys: String, CodingKey {
        case protocolVersion = "protocol_version", capabilities, token, environment
    }

    public init(token: Data, environment: PushEnvironment) {
        protocolVersion = "v1"
        capabilities = [Self.capability]
        self.token = token.map { String(format: "%02x", $0) }.joined()
        self.environment = environment
    }
}
