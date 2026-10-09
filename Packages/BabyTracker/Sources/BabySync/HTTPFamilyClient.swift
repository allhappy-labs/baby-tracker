import BabyStore
import Foundation

/// The P1 family and lifecycle endpoints over HTTPS.
///
/// Every call is authenticated, and the token is fetched per request rather
/// than captured at construction: a rotated or erased session must take effect
/// on the very next call, not after this object is rebuilt.
public final class HTTPFamilyClient: FamilyClient, @unchecked Sendable {
    private let endpoint: ConfirmedEndpoint
    private let tokens: any SessionTokenProviding
    private let session: URLSession

    public init(endpoint: ConfirmedEndpoint, tokens: any SessionTokenProviding, session: URLSession = .shared) {
        self.endpoint = endpoint
        self.tokens = tokens
        self.session = session
    }

    private func url(_ path: String) -> URL {
        URL(string: endpoint.origin.canonicalString + "/v1/" + path)!
    }

    /// The envelope every family and account command must carry.
    ///
    /// Not one of these bodies had it. The server decodes each request into a
    /// typed struct that embeds `RequestMeta` and refuses anything whose
    /// `protocol_version` is not "v1" or whose `capabilities` are absent —
    /// with `DisallowUnknownFields` on top, so a body with the wrong shape is
    /// rejected rather than partially honoured. Every command on this client
    /// was therefore a guaranteed 400 or 409. The routes were corrected once
    /// before; the envelopes inside them were not.
    private struct Command<Extra: Encodable>: Encodable {
        let protocolVersion = "v1"
        let capabilities = ["family-lifecycle-v1"]
        let commandID: UUID
        let extra: Extra

        enum CodingKeys: String, CodingKey {
            case protocolVersion = "protocol_version"
            case capabilities
            case commandID = "command_id"
        }

        func encode(to encoder: Encoder) throws {
            var container = encoder.container(keyedBy: CodingKeys.self)
            try container.encode(protocolVersion, forKey: .protocolVersion)
            try container.encode(capabilities, forKey: .capabilities)
            // Lower-cased, because `Foundation` encodes a UUID in upper case
            // and the server stores identifiers in canonical form. Its own
            // projection compares `identifier::uuid::text` against the string
            // it was sent and rejects anything that is not already canonical.
            try container.encode(commandID.uuidString.lowercased(), forKey: .commandID)
            // Flattened into the same object rather than nested: the server's
            // request types embed the metadata, they do not contain it.
            try extra.encode(to: encoder)
        }
    }

    private struct NoExtra: Encodable {
        func encode(to encoder: Encoder) throws {}
    }

    /// The envelope for the one family request that carries no command id:
    /// `PreviewInvitationRequest` is `RequestMeta` and nothing else, because
    /// previewing mutates nothing and has no outcome to make idempotent.
    private struct MetaOnlyCommand<Extra: Encodable>: Encodable {
        let protocolVersion = "v1"
        let capabilities = ["family-lifecycle-v1"]
        let extra: Extra

        enum CodingKeys: String, CodingKey {
            case protocolVersion = "protocol_version"
            case capabilities
        }

        func encode(to encoder: Encoder) throws {
            var container = encoder.container(keyedBy: CodingKeys.self)
            try container.encode(protocolVersion, forKey: .protocolVersion)
            try container.encode(capabilities, forKey: .capabilities)
            try extra.encode(to: encoder)
        }
    }

    /// A command with nothing but the envelope, which is most of them.
    ///
    /// Named `envelope`, not `command`: `perform(_ command:)` binds that word
    /// to its parameter, and a call to `command()` inside it resolves to the
    /// parameter rather than to this.
    private func envelope(_ commandID: UUID = UUID()) -> Command<NoExtra> {
        Command(commandID: commandID, extra: NoExtra())
    }

    /// Recovery and logout are the two families of command that do NOT accept
    /// `family-lifecycle-v1`: the server validates each against its own
    /// capability list, and an unlisted one is a 409 rather than a courtesy.
    private struct ScopedCommand<Extra: Encodable>: Encodable {
        let protocolVersion = "v1"
        let capabilities: [String]
        let commandID: UUID
        let extra: Extra

        enum CodingKeys: String, CodingKey {
            case protocolVersion = "protocol_version"
            case capabilities
            case commandID = "command_id"
        }

        func encode(to encoder: Encoder) throws {
            var container = encoder.container(keyedBy: CodingKeys.self)
            try container.encode(protocolVersion, forKey: .protocolVersion)
            try container.encode(capabilities, forKey: .capabilities)
            try container.encode(commandID.uuidString.lowercased(), forKey: .commandID)
            try extra.encode(to: encoder)
        }
    }

    /// The server spells "yes, I mean it" as a field on the body for the
    /// commands that destroy something.
    private struct Confirmed: Encodable {
        let confirmed = true
    }

    public func createFamily(name: String, displayName: String) async throws -> FamilyMembership {
        // `name` is not sent. `CreateFamilyRequest` carries one display name
        // and the decoder rejects unknown fields, so a second name made every
        // call fail. The parameter stays because `FamilyClient` declares it.
        struct Extra: Encodable {
            let displayName: String
            enum CodingKeys: String, CodingKey { case displayName = "display_name" }
        }
        let body = Command(commandID: UUID(), extra: Extra(displayName: displayName))
        let family = try await send(WireFamily.self, "POST", url("family"), body)
        return try await membership(in: family.id)
    }

    public func confirmJoin(token: String, displayName: String) async throws -> FamilyMembership {
        // Redemption takes the token and an explicit confirmation. The display
        // name belongs to the account and is set through `updateDisplayName`;
        // sending it here was rejected as an unknown field.
        struct Extra: Encodable {
            let confirmed = true
            let token: String
        }
        let body = Command(commandID: UUID(), extra: Extra(token: token))
        let family = try await send(WireFamily.self, "POST", url("family/invitations/redeem"), body)
        return try await membership(in: family.id)
    }

    /// The family the server named, plus the three parts of the session only
    /// this device knows.
    ///
    /// Both commands used to decode a body of `family_id`, `account_id`,
    /// `device_id` and `recovery_epoch`. The server sends none of those: it
    /// answers with the family — `id`, `display_name`, `status`, `members` —
    /// and says nothing about the caller, which it has already authenticated.
    /// Every join and every family creation failed on the first key.
    private func membership(in familyID: UUID) async throws -> FamilyMembership {
        guard let identity = await tokens.currentSessionIdentity() else {
            throw HTTPClientError.sessionIdentityUnavailable
        }
        return FamilyMembership(
            familyID: familyID, accountID: identity.accountID,
            deviceID: identity.deviceID, recoveryEpoch: identity.recoveryEpoch
        )
    }

    /// The members and their roles, which is what Settings renders.
    ///
    /// `GET /v1/family`, not `GET /v1/family/actors`. Actors are display
    /// LABELS — an account id, a name and whether they have left — and carry
    /// no role at all, so every member decoded from there failed on `role`.
    /// The roles live on the family.
    public func members() async throws -> [FamilyMember] {
        try await send(WireFamily.self, "GET", url("family"), nil as Never?).members.map(\.value)
    }

    /// This device's own account, and the role it holds in its family.
    ///
    /// Two requests, because no single response carries both. `GET /v1/account`
    /// knows the name and not the role; the role is a property of a membership
    /// and lives on the family. An account with no family has no role, and
    /// says so rather than claiming one.
    public func profile() async throws -> CaregiverProfile {
        let account = try await send(WireAccount.self, "GET", url("account"), nil as Never?)
        guard account.familySlot != nil else {
            return CaregiverProfile(accountID: account.id, displayName: account.displayName, role: "")
        }
        let family = try await send(WireFamily.self, "GET", url("family"), nil as Never?)
        let role = family.members.first { $0.accountID == account.id }?.role ?? ""
        return CaregiverProfile(accountID: account.id, displayName: account.displayName, role: role)
    }

    public func updateDisplayName(_ name: String) async throws -> CaregiverProfile {
        struct Extra: Encodable {
            let confirmed = true
            let displayName: String
            enum CodingKeys: String, CodingKey { case confirmed, displayName = "display_name" }
        }
        let body = Command(commandID: UUID(), extra: Extra(displayName: name))
        let account = try await send(WireAccount.self, "PATCH", url("account"), body)
        // The role is unchanged by a rename, and this response does not carry
        // one, so it is read back rather than invented.
        return try await CaregiverProfile(
            accountID: account.id, displayName: account.displayName, role: profile().role
        )
    }

    public func createInvitation(role: FamilyRole) async throws -> FamilyInvitation {
        // `link`, not `share_url`. `InvitationResponse` has never had a
        // `share_url`, so every invitation this client minted was created on
        // the server and then thrown away undecodable on the way back.
        struct Wire: Decodable {
            let id: UUID
            let link: URL
            let role: String
            let expiresAt: Date

            enum CodingKeys: String, CodingKey { case id, link, role, expiresAt = "expires_at" }

            init(from decoder: Decoder) throws {
                let container = try decoder.container(keyedBy: CodingKeys.self)
                id = try container.decode(UUID.self, forKey: .id)
                link = try container.decode(URL.self, forKey: .link)
                role = try container.decode(String.self, forKey: .role)
                expiresAt = try RFC3339.date(
                    from: try container.decode(String.self, forKey: .expiresAt), key: .expiresAt, in: container
                )
            }
        }
        // The role rides on the invitation, so it is decided when the link is
        // written rather than when a stranger redeems it — and a redeemer who
        // could pick their own would not stay a viewer for long.
        struct Extra: Encodable { let role: String }
        let body = Command(commandID: UUID(), extra: Extra(role: role.rawValue))
        let wire = try await send(Wire.self, "POST", url("family/invitations"), body)
        // An unrecognised role degrades to the least privileged one, same as
        // `FamilyMember.familyRole` — guessing upwards would hand a stranger
        // write access this device never actually asked for.
        return FamilyInvitation(
            id: wire.id, shareURL: wire.link, role: FamilyRole(rawValue: wire.role) ?? .viewer, expiresAt: wire.expiresAt
        )
    }

    /// `GET /v1/family/invitations`, not the create route: this never carries
    /// a token or link, which the server only ever answers with once.
    public func pendingInvitations() async throws -> [PendingInvitation] {
        struct Wire: Decodable {
            let id: UUID
            let role: String
            let expiresAt: Date
            let createdAt: Date

            enum CodingKeys: String, CodingKey { case id, role, expiresAt = "expires_at", createdAt = "created_at" }

            init(from decoder: Decoder) throws {
                let container = try decoder.container(keyedBy: CodingKeys.self)
                id = try container.decode(UUID.self, forKey: .id)
                role = try container.decode(String.self, forKey: .role)
                expiresAt = try RFC3339.date(
                    from: try container.decode(String.self, forKey: .expiresAt), key: .expiresAt, in: container
                )
                createdAt = try RFC3339.date(
                    from: try container.decode(String.self, forKey: .createdAt), key: .createdAt, in: container
                )
            }
        }
        struct Page: Decodable { let invitations: [Wire] }
        let page = try await send(Page.self, "GET", url("family/invitations"), nil as Never?)
        return page.invitations.map {
            PendingInvitation(id: $0.id, role: FamilyRole(rawValue: $0.role) ?? .viewer, expiresAt: $0.expiresAt, createdAt: $0.createdAt)
        }
    }

    public func revokeInvitation(_ invitationID: UUID) async throws {
        try await sendVoid("POST", url("family/invitations/\(invitationID.uuidString.lowercased())/revoke"), body: envelope())
    }

    /// Previewing takes no confirmation and no command id: `PreviewInvitationRequest`
    /// is `RequestMeta` plus the token, nothing else — sending a `command_id`
    /// here would be an undeclared field the server refuses outright.
    public func previewInvitation(token: String) async throws -> InvitationPreview {
        struct Extra: Encodable { let token: String }
        struct Wire: Decodable {
            let familyDisplayName: String
            let expiresAt: Date

            enum CodingKeys: String, CodingKey { case familyDisplayName = "family_display_name", expiresAt = "expires_at" }

            init(from decoder: Decoder) throws {
                let container = try decoder.container(keyedBy: CodingKeys.self)
                familyDisplayName = try container.decode(String.self, forKey: .familyDisplayName)
                expiresAt = try RFC3339.date(
                    from: try container.decode(String.self, forKey: .expiresAt), key: .expiresAt, in: container
                )
            }
        }
        let body = MetaOnlyCommand(extra: Extra(token: token))
        let wire = try await send(Wire.self, "POST", url("family/invitations/preview"), body)
        return InvitationPreview(familyName: wire.familyDisplayName, expiresAt: wire.expiresAt)
    }

    public func leaveFamily() async throws {
        try await sendVoid("POST", url("family/leave"), body: envelope())
    }

    public func perform(_ command: FamilyLifecycleCommand) async throws {
        switch command {
        case .removeMember(let id):
            try await sendVoid("POST", url("family/members/\(id.uuidString.lowercased())/remove"), body: envelope())
        case .transferOwnership(let id):
            // The account being handed the family goes in the BODY. It was
            // dropped entirely — the path carries no id, so the server was
            // asked to transfer ownership to nobody.
            struct Extra: Encodable {
                let newOwnerAccountID: String
                enum CodingKeys: String, CodingKey { case newOwnerAccountID = "new_owner_account_id" }
            }
            try await sendVoid(
                "POST",
                url("family/ownership-transfer"),
                body: Command(
                    commandID: UUID(),
                    extra: Extra(newOwnerAccountID: id.uuidString.lowercased())
                )
            )
        case .changeRole(let id, let role):
            struct Extra: Encodable { let role: String }
            try await sendVoid(
                "POST",
                url("family/members/\(id.uuidString.lowercased())/role"),
                body: Command(commandID: UUID(), extra: Extra(role: role.rawValue))
            )
        case .deleteAccount:
            try await sendVoid("POST", url("account/delete"), body: Command(commandID: UUID(), extra: Confirmed()))
        }
    }

    public func scheduleFamilyDeletion() async throws {
        try await sendVoid("POST", url("family/delete"), body: Command(commandID: UUID(), extra: Confirmed()))
    }

    public func recoverFamily() async throws -> [FamilyMember] {
        struct Page: Decodable { let actors: [WireMember] }
        let body = ScopedCommand(capabilities: ["recovery-v1"], commandID: UUID(), extra: Confirmed())
        return try await send(Page.self, "POST", url("family/recover"), body).actors.map(\.value)
    }

    public func forfeitFamilyRecovery() async throws {
        let body = ScopedCommand(capabilities: ["recovery-v1"], commandID: UUID(), extra: Confirmed())
        try await sendVoid("POST", url("family/recovery/forfeit"), body: body)
    }

    /// Reserved for explicit disconnect and account deletion — the two paths
    /// that genuinely revoke this device's binding. Credential-state loss must
    /// never reach here.
    public func logout() async throws {
        let body = ScopedCommand(capabilities: ["identity-v1"], commandID: UUID(), extra: NoExtra())
        try await sendVoid("POST", url("auth/logout"), body: body)
    }

    // MARK: Plumbing

    private func request(_ method: String, _ url: URL, _ body: (some Encodable)?) async throws -> URLRequest {
        var request = URLRequest(url: url)
        request.httpMethod = method
        if let token = await tokens.currentAccessToken() {
            request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        }
        if let body {
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONEncoder().encode(body)
        }
        return request
    }

    private func send<T: Decodable>(_ type: T.Type, _ method: String, _ url: URL, _ body: (some Encodable)?) async throws -> T {
        let (data, response) = try await session.data(for: try await request(method, url, body))
        try Self.check(response, data)
        return try JSONDecoder().decode(type, from: data)
    }

    private func sendVoid(_ method: String, _ url: URL, body: (some Encodable)? = nil as Never?) async throws {
        let (data, response) = try await session.data(for: try await request(method, url, body))
        try Self.check(response, data)
    }

    private static func check(_ response: URLResponse, _ data: Data) throws {
        guard let http = response as? HTTPURLResponse else { throw HTTPClientError.notHTTP }
        guard (200..<300).contains(http.statusCode) else {
            throw HTTPClientError.status(http.statusCode, ServerErrorEnvelope.decode(data))
        }
    }
}

/// `FamilyResponse`. The shape three routes answer with: create, redeem and
/// read. It names the family and its members and nothing about the caller.
struct WireFamily: Decodable {
    let id: UUID
    let displayName: String
    let status: String
    let members: [WireMember]

    enum CodingKeys: String, CodingKey {
        case id, displayName = "display_name", status, members
    }
}

/// `AccountResponse`. No role: a role belongs to a membership, not an account.
struct WireAccount: Decodable {
    let id: UUID
    let displayName: String
    let status: String
    let familySlot: WireFamilySlot?

    enum CodingKeys: String, CodingKey {
        case id, displayName = "display_name", status, familySlot = "family_slot"
    }
}

struct WireFamilySlot: Decodable {
    let familyID: UUID
    let state: String

    enum CodingKeys: String, CodingKey { case familyID = "family_id", state }
}

struct WireMember: Decodable {
    let accountID: UUID
    let displayName: String
    let role: String
    let status: String

    enum CodingKeys: String, CodingKey {
        case accountID = "account_id", displayName = "display_name", role, status
    }

    var value: FamilyMember { FamilyMember(id: accountID, displayName: displayName, role: role) }
}
