import Foundation

/// An origin whose server instance has been probed and accepted by the user.
public struct ConfirmedEndpoint: Sendable, Hashable, Codable {
    public let origin: ServerOrigin
    public let serverInstanceID: UUID

    public init(origin: ServerOrigin, serverInstanceID: UUID) {
        self.origin = origin
        self.serverInstanceID = serverInstanceID
    }
}

/// What `GET /v1/server` tells us about an endpoint before we trust it.
public struct ServerInfo: Codable, Sendable, Equatable {
    public let serverInstanceID: UUID
    public let canonicalOrigin: String
    public let recoveryEpoch: UInt64
    public let capabilities: [String]
    public let appleAuthorizationMode: String
    public let maxItemsPerPage: Int
    public let maxEncodedBytes: Int
    public let sourceVersion: String
    public let sourceURL: String

    public init(
        serverInstanceID: UUID, canonicalOrigin: String, recoveryEpoch: UInt64,
        capabilities: [String], appleAuthorizationMode: String,
        maxItemsPerPage: Int, maxEncodedBytes: Int, sourceVersion: String, sourceURL: String
    ) {
        self.serverInstanceID = serverInstanceID
        self.canonicalOrigin = canonicalOrigin
        self.recoveryEpoch = recoveryEpoch
        self.capabilities = capabilities
        self.appleAuthorizationMode = appleAuthorizationMode
        self.maxItemsPerPage = maxItemsPerPage
        self.maxEncodedBytes = maxEncodedBytes
        self.sourceVersion = sourceVersion
        self.sourceURL = sourceURL
    }

    /// `GET /v1/server` names the instance `instance_id` — not
    /// `server_instance_id` as the challenge does — and omits the source and
    /// page fields when they are unset (`protocol.ServerInfo`, pinned by
    /// `Server/api/fixtures/server-info.json`). Reading the wrong name failed
    /// every probe, so no server could be confirmed from the selection screen.
    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        serverInstanceID = try container.decode(UUID.self, forKey: .serverInstanceID)
        canonicalOrigin = try container.decode(String.self, forKey: .canonicalOrigin)
        recoveryEpoch = try container.decode(UInt64.self, forKey: .recoveryEpoch)
        capabilities = try container.decode([String].self, forKey: .capabilities)
        appleAuthorizationMode = try container.decode(String.self, forKey: .appleAuthorizationMode)
        maxItemsPerPage = try container.decodeIfPresent(Int.self, forKey: .maxItemsPerPage) ?? 0
        maxEncodedBytes = try container.decodeIfPresent(Int.self, forKey: .maxEncodedBytes) ?? 0
        sourceVersion = try container.decodeIfPresent(String.self, forKey: .sourceVersion) ?? ""
        sourceURL = try container.decodeIfPresent(String.self, forKey: .sourceURL) ?? ""
    }

    enum CodingKeys: String, CodingKey {
        case serverInstanceID = "instance_id", canonicalOrigin = "canonical_origin"
        case recoveryEpoch = "recovery_epoch", capabilities
        case appleAuthorizationMode = "apple_authorization_mode"
        case maxItemsPerPage = "max_items_per_page", maxEncodedBytes = "max_encoded_bytes"
        case sourceVersion = "source_version", sourceURL = "source_url"
    }

    public var advertisesLiveHints: Bool { capabilities.contains("live-hints-v1") }
    public var advertisesRefreshRecovery: Bool { capabilities.contains("refresh-recovery-v1") }
    public var advertisesLiveActivityPush: Bool { capabilities.contains("live-activity-push-v1") }
}

public enum EndpointIdentityError: Error, Sendable, Equatable {
    /// A known origin now reports a different instance. Could be a rebuild, a
    /// restore, or a different server answering — either way the user decides,
    /// because credentials and data are about to go there.
    case instanceChanged(known: UUID, reported: UUID)
    /// The server named an origin other than the one we dialled. An advertised
    /// origin is never accepted as a replacement target.
    case originMismatch(dialled: String, advertised: String)
    case redirected
}

/// Probes an origin and decides whether it may be used.
public struct EndpointIdentityProbe: Sendable {
    public init() {}

    /// Validates a probe result against what we already knew about the origin.
    ///
    /// Confirmation-first: an authenticated endpoint is immutable, so a changed
    /// instance is surfaced for explicit confirmation *before* any credential
    /// or record leaves the device.
    public func confirm(
        origin: ServerOrigin,
        info: ServerInfo,
        knownInstanceID: UUID?,
        wasRedirected: Bool = false
    ) throws -> ConfirmedEndpoint {
        guard !wasRedirected else { throw EndpointIdentityError.redirected }
        guard info.canonicalOrigin == origin.canonicalString else {
            throw EndpointIdentityError.originMismatch(dialled: origin.canonicalString, advertised: info.canonicalOrigin)
        }
        if let knownInstanceID, knownInstanceID != info.serverInstanceID {
            throw EndpointIdentityError.instanceChanged(known: knownInstanceID, reported: info.serverInstanceID)
        }
        return ConfirmedEndpoint(origin: origin, serverInstanceID: info.serverInstanceID)
    }
}
