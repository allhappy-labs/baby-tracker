import Foundation
import Testing
@testable import BabySync

@Suite("Invitation parsing")
struct InvitationParserTests {
    static let token = "abcdefghijklmnop0123456789"

    @Test func parsesHostedUniversalLink() throws {
        let intent = try InvitationParser.parse("https://sync.example.test/invite?token=\(Self.token)")
        #expect(intent.origin.canonicalString == "https://sync.example.test")
        #expect(intent.token == Self.token)
    }

    @Test func parsesSelfHostedCustomScheme() throws {
        let intent = try InvitationParser.parse(
            "babytracker://invite?token=\(Self.token)&origin=https://self.example.test"
        )
        #expect(intent.origin.canonicalString == "https://self.example.test")
        #expect(intent.token == Self.token)
    }

    /// The link the server actually mints (`family/service.go`):
    /// `<origin>/invite#token=…`. The token rides in the FRAGMENT so it never
    /// reaches a server log or a Referer, and the parser read only the query
    /// — so every link and QR code the owner shared failed to parse on the
    /// joiner's phone. Found by the two-caregiver live-server gate.
    @Test func parsesTheLinkTheServerMints() throws {
        let token = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCd"
        let intent = try InvitationParser.parse("https://sync.example.test/invite#token=\(token)")
        #expect(intent.origin.canonicalString == "https://sync.example.test")
        #expect(intent.token == token)
    }

    /// The server's `/invite` handoff page opens the app with
    /// `babytracker://invite?server=<origin>&token=…` — `server`, not
    /// `origin` (`httpapi/assets/invitation.html`).
    @Test func parsesTheServersHandoffPageLink() throws {
        let intent = try InvitationParser.parse(
            "babytracker://invite?server=https%3A%2F%2Fself.example.test&token=\(Self.token)"
        )
        #expect(intent.origin.canonicalString == "https://self.example.test")
        #expect(intent.token == Self.token)
    }

    @Test(arguments: [
        "http://sync.example.test/invite?token=abcdefghijklmnop0123456789",
        "https://sync.example.test/join?token=abcdefghijklmnop0123456789",
        "ftp://sync.example.test/invite?token=abcdefghijklmnop0123456789",
        "just some pasted words",
    ])
    func rejectsUnsupportedInput(input: String) {
        #expect(throws: (any Error).self) { try InvitationParser.parse(input) }
    }

    @Test func rejectsMissingToken() {
        #expect(throws: InvitationParser.ParseError.missingToken) {
            try InvitationParser.parse("https://sync.example.test/invite")
        }
    }

    @Test(arguments: ["short", "has spaces in it and is long enough", "has/slashes/and/is/long/enough"])
    func rejectsMalformedToken(token: String) {
        #expect(throws: InvitationParser.ParseError.malformedToken) {
            try InvitationParser.parse("https://sync.example.test/invite?token=\(token)")
        }
    }

    /// A self-hosted link whose origin is unusable is refused rather than
    /// quietly pointed somewhere else.
    @Test func rejectsSelfHostedLinkWithUnsafeOrigin() {
        #expect(throws: InvitationParser.ParseError.invalidOrigin(.schemeNotHTTPS)) {
            try InvitationParser.parse("babytracker://invite?token=\(Self.token)&origin=http://self.example.test")
        }
    }
}

/// `intent(fromCodeOrLink:confirmedOrigin:)`: what the paste-a-code join
/// field actually calls. A bare code has no scheme at all, so it is paired
/// with the endpoint the user already confirmed rather than an origin lifted
/// out of the text itself.
@Suite("Invitation code or link")
struct InvitationCodeOrLinkTests {
    static let token = InvitationParserTests.token
    static let confirmedOrigin = try! ServerOrigin(validating: "https://sync.example.test")

    @Test func bareCodeIsPairedWithTheConfirmedOrigin() throws {
        let intent = try InvitationParser.intent(fromCodeOrLink: Self.token, confirmedOrigin: Self.confirmedOrigin)
        #expect(intent.origin == Self.confirmedOrigin)
        #expect(intent.token == Self.token)
    }

    /// A pasted link keeps its own origin — the confirmed origin is only a
    /// fallback for text with no origin of its own, never an override.
    @Test func aPastedLinkUsesItsOwnOriginNotTheConfirmedOne() throws {
        let elsewhere = try ServerOrigin(validating: "https://elsewhere.example.test")
        let intent = try InvitationParser.intent(
            fromCodeOrLink: "https://sync.example.test/invite?token=\(Self.token)",
            confirmedOrigin: elsewhere
        )
        #expect(intent.origin.canonicalString == "https://sync.example.test")
        #expect(intent.token == Self.token)
    }

    @Test func whitespaceAroundABareCodeIsTrimmed() throws {
        let intent = try InvitationParser.intent(
            fromCodeOrLink: "  \(Self.token)  \n", confirmedOrigin: Self.confirmedOrigin
        )
        #expect(intent.token == Self.token)
    }

    @Test func aMalformedBareCodeIsRejectedBeforeAnyRequest() {
        #expect(throws: InvitationParser.ParseError.malformedToken) {
            try InvitationParser.intent(fromCodeOrLink: "too-short", confirmedOrigin: Self.confirmedOrigin)
        }
    }

    /// Text that carries its own (unsupported) scheme is a broken link, not a
    /// very long code — it must fail the way a pasted link fails, not be
    /// silently swallowed as token garbage.
    @Test func aBrokenLinkIsRefusedRatherThanTreatedAsACode() {
        #expect(throws: InvitationParser.ParseError.self) {
            try InvitationParser.intent(
                fromCodeOrLink: "ftp://sync.example.test/invite?token=\(Self.token)",
                confirmedOrigin: Self.confirmedOrigin
            )
        }
    }
}
