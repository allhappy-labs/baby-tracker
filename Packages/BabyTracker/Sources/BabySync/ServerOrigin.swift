import Foundation

/// An HTTPS origin the client is willing to talk to.
///
/// Canonicalised down to scheme, host and non-default port and nothing else.
/// Credentials, a path, a query or a fragment are all rejected rather than
/// stripped: an input carrying them is not a mistyped origin, it is somebody
/// trying to smuggle a target past the user, and silently "fixing" it would
/// hide that.
public struct ServerOrigin: Sendable, Hashable, Codable {
    public let canonicalString: String

    public enum ValidationError: Error, Sendable, Equatable {
        case notAURL, schemeNotHTTPS, missingHost, credentialsPresent
        case pathPresent, queryPresent, fragmentPresent, invalidPort
    }

    public init(validating input: String) throws {
        let trimmed = input.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let components = URLComponents(string: trimmed) else { throw ValidationError.notAURL }
        guard components.scheme?.lowercased() == "https" else { throw ValidationError.schemeNotHTTPS }
        guard let host = components.host?.lowercased(), !host.isEmpty else { throw ValidationError.missingHost }
        guard components.user == nil, components.password == nil else { throw ValidationError.credentialsPresent }
        guard components.path.isEmpty || components.path == "/" else { throw ValidationError.pathPresent }
        guard components.query == nil else { throw ValidationError.queryPresent }
        guard components.fragment == nil else { throw ValidationError.fragmentPresent }

        if let port = components.port {
            guard (1...65535).contains(port) else { throw ValidationError.invalidPort }
            // 443 is the default for https and is never part of the canonical form.
            canonicalString = port == 443 ? "https://\(host)" : "https://\(host):\(port)"
        } else {
            canonicalString = "https://\(host)"
        }
    }

    /// The `GET /v1/server` identity probe for this origin.
    public var serverInfoURL: URL { URL(string: canonicalString + "/v1/server")! }
}
