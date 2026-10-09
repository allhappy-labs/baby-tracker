import Foundation
import Testing
@testable import BabyStore

@Suite("Account lease identity")
struct AccountLeaseKeyTests {
    private let origin = "https://sync.example"
    private let instance = UUID(uuidString: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")!
    private let account = UUID(uuidString: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")!
    private let device = UUID(uuidString: "cccccccc-cccc-4ccc-8ccc-cccccccccccc")!

    @Test func roundTripsWithoutSessionDependency() throws {
        let key = AccountLeaseKey(canonicalOrigin: origin, serverInstanceID: instance, accountID: account, deviceID: device)
        let encoded = try JSONEncoder().encode(LeaseScope.account(key))

        #expect(try JSONDecoder().decode(LeaseScope.self, from: encoded) == .account(key))
    }

    @Test func everyIdentityFieldProducesDistinctScope() {
        let base = AccountLeaseKey(canonicalOrigin: origin, serverInstanceID: instance, accountID: account, deviceID: device)
        let variants = [
            AccountLeaseKey(canonicalOrigin: "https://other.example", serverInstanceID: instance, accountID: account, deviceID: device),
            AccountLeaseKey(canonicalOrigin: origin, serverInstanceID: UUID(), accountID: account, deviceID: device),
            AccountLeaseKey(canonicalOrigin: origin, serverInstanceID: instance, accountID: UUID(), deviceID: device),
            AccountLeaseKey(canonicalOrigin: origin, serverInstanceID: instance, accountID: account, deviceID: UUID()),
        ]

        #expect(Set(variants.map { LeaseScope.account($0).encodedKey }).count == variants.count)
        #expect(!variants.contains(base))
    }
}
