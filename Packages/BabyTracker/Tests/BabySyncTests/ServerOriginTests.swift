import Foundation
import Testing
@testable import BabySync

@Suite("Server origin validation")
struct ServerOriginTests {
    @Test(arguments: [
        ("https://sync.example.com", "https://sync.example.com"),
        ("https://Sync.Example.COM", "https://sync.example.com"),
        ("https://sync.example.com/", "https://sync.example.com"),
        ("  https://sync.example.com  ", "https://sync.example.com"),
        ("https://sync.example.com:8443", "https://sync.example.com:8443"),
        // 443 is https's default and is never part of the canonical form.
        ("https://sync.example.com:443", "https://sync.example.com"),
    ])
    func canonicalisesAcceptableOrigins(input: String, expected: String) throws {
        #expect(try ServerOrigin(validating: input).canonicalString == expected)
    }

    @Test(arguments: [
        ("http://sync.example.com", ServerOrigin.ValidationError.schemeNotHTTPS),
        ("https://user:pass@sync.example.com", .credentialsPresent),
        ("https://sync.example.com/v1/server", .pathPresent),
        ("https://sync.example.com?token=abc", .queryPresent),
        ("https://sync.example.com#fragment", .fragmentPresent),
        ("https://", .missingHost),
        // A bare string parses as a scheme-less relative reference, so it is
        // caught by the scheme check rather than by URL parsing. Still refused.
        ("not a url at all", .schemeNotHTTPS),
    ])
    func rejectsUnsafeOrigins(input: String, expected: ServerOrigin.ValidationError) throws {
        #expect(throws: expected) { try ServerOrigin(validating: input) }
    }

    /// Rejected, never silently stripped: an address carrying credentials or a
    /// path is not a typo to be helpfully corrected.
    @Test func rejectionIsNotSilentCorrection() throws {
        #expect(throws: ServerOrigin.ValidationError.pathPresent) {
            try ServerOrigin(validating: "https://sync.example.com/evil")
        }
    }

    @Test func probeURLTargetsTheIdentityEndpoint() throws {
        let origin = try ServerOrigin(validating: "https://sync.example.com")
        #expect(origin.serverInfoURL.absoluteString == "https://sync.example.com/v1/server")
    }
}
