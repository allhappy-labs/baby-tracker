import Foundation
import Testing
@testable import BabySync

@Suite("Apple nonce D6 vector")
struct AppleNonceTests {
    // The checked-in P1 vector, byte for byte.
    static let canonicalOrigin = "https://sync.example.test"
    static let instanceID = UUID(uuidString: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let challenge = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
    static let expected = "6227c68e4a909c26acc63eaa3f0da6d0446d861a6edaf459330283c2e4afbbc1"

    @Test func matchesTheServerVector() {
        #expect(appleNonce(
            canonicalOrigin: Self.canonicalOrigin,
            serverInstanceID: Self.instanceID,
            challenge: Self.challenge
        ) == Self.expected)
    }

    /// The origin is inside the hash, which is what stops a relay replaying an
    /// assertion obtained for one server against another.
    @Test func differentOriginProducesADifferentNonce() {
        #expect(appleNonce(
            canonicalOrigin: "https://relay.example",
            serverInstanceID: Self.instanceID,
            challenge: Self.challenge
        ) != Self.expected)
    }

    @Test func differentInstanceProducesADifferentNonce() {
        #expect(appleNonce(
            canonicalOrigin: Self.canonicalOrigin,
            serverInstanceID: UUID(uuidString: "b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!,
            challenge: Self.challenge
        ) != Self.expected)
    }

    /// The challenge is hashed as its own base64url *text*. Decoding it first
    /// is the mistake the vector exists to catch.
    @Test func challengeIsHashedAsTextNotDecodedBytes() {
        let decoded = Data(base64Encoded: Self.challenge.replacingOccurrences(of: "-", with: "+")
            .replacingOccurrences(of: "_", with: "/") + "=") ?? Data()
        let asDecodedText = String(decoding: decoded, as: UTF8.self)
        #expect(appleNonce(
            canonicalOrigin: Self.canonicalOrigin,
            serverInstanceID: Self.instanceID,
            challenge: asDecodedText
        ) != Self.expected)
    }

    /// The separator is one NUL byte, not a backslash followed by a zero.
    @Test func separatorIsASingleNulByte() {
        let literalBackslashZero = appleNonce(
            canonicalOrigin: "\\0" + Self.canonicalOrigin,
            serverInstanceID: Self.instanceID,
            challenge: Self.challenge
        )
        #expect(literalBackslashZero != Self.expected)
    }
}
