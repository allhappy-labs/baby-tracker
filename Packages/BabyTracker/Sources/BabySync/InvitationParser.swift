import BabyStore
import Foundation

/// A scanned, pasted or linked invitation, before anything has been redeemed.
public struct InvitationIntent: Sendable, Equatable {
    public let origin: ServerOrigin
    public let token: String

    public init(origin: ServerOrigin, token: String) {
        self.origin = origin
        self.token = token
    }
}

public struct FamilyInvitation: Sendable, Equatable {
    public let id: UUID
    public let shareURL: URL
    public let role: FamilyRole
    public let expiresAt: Date
    public init(id: UUID, shareURL: URL, role: FamilyRole, expiresAt: Date) {
        self.id = id
        self.shareURL = shareURL
        self.role = role
        self.expiresAt = expiresAt
    }
}

/// Turns invitation text into an intent, and nothing else.
///
/// Parsing is side-effect free by construction: it performs no request, so
/// receiving a link — by scan, paste, or Universal Link — never redeems
/// membership. Only an authenticated confirmation request does that.
public enum InvitationParser {
    public enum ParseError: Error, Sendable, Equatable {
        case unsupportedScheme, missingToken, malformedToken, invalidOrigin(ServerOrigin.ValidationError)
    }

    /// Hosted links use the approved domain; self-hosted links use the
    /// confirmed custom scheme with an explicit origin parameter.
    public static let customScheme = "babytracker"
    public static let invitationPath = "/invite"

    /// The join field's actual entry point: the pasted text may be a bare
    /// code or a full invitation link.
    ///
    /// A bare code carries no origin of its own — it is only ever shown next
    /// to a token, never a link — so it is paired with the origin the user
    /// already confirmed for this server, the same origin `acceptInvitation`
    /// then checks a pasted link against. Text that carries its own scheme is
    /// a link and must parse as one: refusing it here rather than silently
    /// re-reading it as a very long, invalid code keeps a broken link's real
    /// error (a wrong path, an unapproved domain) visible instead of masked
    /// as `malformedToken`.
    public static func intent(fromCodeOrLink input: String, confirmedOrigin: ServerOrigin) throws -> InvitationIntent {
        let trimmed = input.trimmingCharacters(in: .whitespacesAndNewlines)
        if looksLikeALink(trimmed) {
            return try parse(trimmed)
        }
        try validate(trimmed)
        return InvitationIntent(origin: confirmedOrigin, token: trimmed)
    }

    private static func looksLikeALink(_ input: String) -> Bool {
        guard let components = URLComponents(string: input) else { return false }
        switch components.scheme?.lowercased() {
        case "https", customScheme: return true
        default: return false
        }
    }

    public static func parse(_ input: String) throws -> InvitationIntent {
        let trimmed = input.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let components = URLComponents(string: trimmed) else { throw ParseError.unsupportedScheme }
        // The server mints `<origin>/invite#token=…`: the token is in the
        // fragment, so it never reaches a request line, a log or a Referer.
        // Older links, and the handoff page's app link, carry it in the query.
        var items = components.queryItems ?? []
        if let fragment = components.percentEncodedFragment {
            var fragmentItems = URLComponents()
            fragmentItems.percentEncodedQuery = fragment
            items += fragmentItems.queryItems ?? []
        }

        guard let token = items.first(where: { $0.name == "token" })?.value, !token.isEmpty else {
            throw ParseError.missingToken
        }
        try validate(token)

        switch components.scheme?.lowercased() {
        case "https":
            guard components.path == invitationPath, let host = components.host else {
                throw ParseError.unsupportedScheme
            }
            let origin = try makeOrigin("https://\(host)\(components.port.map { ":\($0)" } ?? "")")
            return InvitationIntent(origin: origin, token: token)
        case customScheme:
            // `server` is what the server's `/invite` handoff page writes;
            // `origin` is what this app's own links have always used.
            guard let raw = items.first(where: { $0.name == "server" || $0.name == "origin" })?.value else {
                throw ParseError.unsupportedScheme
            }
            return InvitationIntent(origin: try makeOrigin(raw), token: token)
        default:
            throw ParseError.unsupportedScheme
        }
    }

    /// Tokens are opaque to the client; it only checks the shape it will put
    /// on the wire, so a pasted sentence never becomes a request.
    private static func validate(_ token: String) throws {
        let allowed = CharacterSet.alphanumerics.union(CharacterSet(charactersIn: "-_"))
        guard token.count >= 16, token.count <= 256,
              token.unicodeScalars.allSatisfy(allowed.contains) else {
            throw ParseError.malformedToken
        }
    }

    private static func makeOrigin(_ raw: String) throws -> ServerOrigin {
        do { return try ServerOrigin(validating: raw) }
        catch let error as ServerOrigin.ValidationError { throw ParseError.invalidOrigin(error) }
    }
}
