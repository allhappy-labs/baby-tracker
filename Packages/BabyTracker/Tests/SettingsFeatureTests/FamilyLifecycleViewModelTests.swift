import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// Scheduling deletion, then either recovering or forfeiting.
///
/// The two branches get separate fixtures on purpose: a recovery claim is
/// consumed by whichever path runs first, so forfeiting a claim that recovery
/// already used would prove nothing about either.
@MainActor
@Suite("Family lifecycle recovery and forfeiture")
struct FamilyLifecycleViewModelTests {
    @Test func scheduledFamilyRecoveryRestoresOnlyOwnerAndConsumesClaim() async throws {
        let fixture = try await FamilySettingsFixture.ownerModelWithTwoMembers()
        let model = fixture.model

        try await model.scheduleFamilyDeletion()
        #expect(fixture.client.scheduledDeletion)
        #expect(model.members.isEmpty)
        #expect(model.confirmationCopy.contains("recovery window"))

        // P1 requires fresh Apple proof for the recovery-only claim.
        fixture.identity.bundle = Self.recoveryBundle
        try await model.signInWithApple()
        guard case .recovery = model.mode else {
            Issue.record("Expected recovery-only claim")
            return
        }

        try await model.recoverFamily()
        guard case .shared = model.mode else {
            Issue.record("Expected recovered shared family")
            return
        }
        #expect(fixture.client.recoveryClaimConsumed)
        #expect(model.members.count == 1)
        #expect(model.members.first?.role == "owner")

        // An active owner again, so account deletion needs a transfer again.
        await #expect(throws: FamilySyncError.ownerTransferRequired) { try await model.deleteAccount() }
    }

    @Test func scheduledFamilyForfeitReleasesSlotWithoutRecovering() async throws {
        let fixture = try await FamilySettingsFixture.ownerModelWithTwoMembers()
        let model = fixture.model

        try await model.scheduleFamilyDeletion()
        fixture.identity.bundle = Self.recoveryBundle
        try await model.signInWithApple()
        guard case .recovery = model.mode else {
            Issue.record("Expected recovery-only claim")
            return
        }

        try await model.forfeitFamilyRecovery()
        guard case .authenticated = model.mode else {
            Issue.record("Expected account without family slot")
            return
        }
        #expect(fixture.client.recoveryClaimConsumed)
        #expect(try fixture.store.babies(in: FamilySettingsFixture.sharedRealm).isEmpty)

        // No active owner membership or recovery claim remains.
        try await model.deleteAccount()
        #expect(model.lastLifecycleCommands.contains(.deleteAccount))
    }

    static var recoveryBundle: SessionBundle {
        SessionBundle(
            accountID: FamilySettingsFixture.accountID,
            deviceID: FamilySettingsFixture.deviceID,
            recoveryEpoch: 0,
            accessToken: "recovery-access",
            refreshToken: "recovery-refresh",
            expiresAt: Date(timeIntervalSince1970: 20_000),
            deviceCredential: nil,
            familyID: FamilySettingsFixture.familyID,
            isRecoveryOnly: true
        )
    }
}
