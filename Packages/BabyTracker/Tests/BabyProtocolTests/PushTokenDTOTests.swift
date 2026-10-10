import Foundation
import Testing
@testable import BabyProtocol

@Suite("Push token DTO")
struct PushTokenDTOTests {
    /// The body of `PUT /v1/devices/push-token`: lowercase hex, the wire's
    /// snake-case keys, and exactly the one capability the route negotiates.
    @Test func encodesTheWireShape() throws {
        let request = PushTokenRequest(token: Data([0x0a, 0xff] + Array(repeating: 0, count: 14)), environment: .production)
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        let json = try #require(String(data: try encoder.encode(request), encoding: .utf8))
        #expect(json == #"{"capabilities":["push-token-v1"],"environment":"production","protocol_version":"v1","token":"0aff0000000000000000000000000000"}"#)
    }
}
