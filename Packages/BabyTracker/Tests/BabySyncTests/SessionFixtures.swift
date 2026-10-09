import Foundation
@testable import BabySync

/// Shared identities for the session and continuity suites.
enum SessionFixture {
    static let origin = try! ServerOrigin(validating: "https://sync.example.test")
    static let otherOrigin = try! ServerOrigin(validating: "https://other.example.test")
    static let instanceID = UUID(uuidString: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let otherInstanceID = UUID(uuidString: "c1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let accountID = UUID(uuidString: "d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let otherAccountID = UUID(uuidString: "e1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let deviceID = UUID(uuidString: "f1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let otherDeviceID = UUID(uuidString: "01b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    static let familyID = UUID(uuidString: "b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!

    static var endpoint: ConfirmedEndpoint {
        ConfirmedEndpoint(origin: origin, serverInstanceID: instanceID)
    }

    static func accountKey(
        origin: ServerOrigin = origin,
        instanceID: UUID = instanceID,
        accountID: UUID = accountID,
        deviceID: UUID = deviceID,
        epoch: UInt64 = 0
    ) -> AccountSessionKey {
        AccountSessionKey(
            canonicalOrigin: origin.canonicalString,
            serverInstanceID: instanceID,
            accountID: accountID,
            deviceID: deviceID,
            recoveryEpoch: epoch
        )
    }

    static func familyKey(
        familyID: UUID = familyID,
        accountID: UUID = accountID,
        epoch: UInt64 = 0
    ) -> FamilySessionKey {
        FamilySessionKey(
            canonicalOrigin: origin.canonicalString,
            serverInstanceID: instanceID,
            familyID: familyID,
            accountID: accountID,
            deviceID: deviceID,
            recoveryEpoch: epoch
        )
    }

    static let continuity = DeviceContinuity(deviceID: deviceID, credential: "device-credential-abc")

    static func credentials(
        access: String = "access-token",
        refresh: String = "refresh-token",
        continuity: DeviceContinuity = continuity,
        epoch: UInt64 = 0
    ) -> SessionCredentials {
        SessionCredentials(
            accessToken: access,
            refreshToken: refresh,
            expiresAt: Date(timeIntervalSince1970: 10_000),
            recoveryEpoch: epoch,
            deviceContinuity: continuity,
            credentialVersion: UUID(uuidString: "11111111-1111-4111-8111-111111111111")!
        )
    }

    static func store() -> (KeychainSessionStore, InMemoryKeychainBackend) {
        let backend = InMemoryKeychainBackend()
        return (KeychainSessionStore(backend: backend), backend)
    }
}
