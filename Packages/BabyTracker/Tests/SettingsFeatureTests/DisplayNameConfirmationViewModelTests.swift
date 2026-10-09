import BabySync
import Foundation
import Testing
@testable import SettingsFeature

@MainActor
@Suite("Display name confirmation")
struct DisplayNameConfirmationViewModelTests {
    /// The name is asked before the first Apple sign-in: the server refuses
    /// an account's first sign-in without one, and Apple offers a name only
    /// on the very first authorization — never on a second device.
    @Test func signingInNeedsAConfirmedNameFirst() async throws {
        let fixture = try FamilySettingsFixture.unnamedModel()
        #expect(!fixture.model.displayNameConfirmed)
        await #expect(throws: FamilySyncError.displayNameConfirmationRequired) {
            try await fixture.model.signInWithApple()
        }
        #expect(fixture.identity.exchangeCount == 0, "Apple was asked before there was a name to send")

        try await fixture.model.confirmDisplayName("  Alex  ")
        #expect(fixture.model.displayNameConfirmed)
        #expect(fixture.model.profile.displayName == "Alex")
        try await fixture.model.signInWithApple()
        #expect(fixture.identity.lastRequest?.displayName == "Alex")
    }

    /// Apple's own name never overrides the one the user confirmed.
    @Test func appleSuppliedNameDoesNotReplaceTheConfirmedOne() async throws {
        let fixture = try FamilySettingsFixture.make(displayName: "", appleFullName: "Alexandra", confirmedName: "Alex", familyInBundle: false)
        try await fixture.model.signInWithApple()
        #expect(fixture.model.profile.displayName == "Alex")
        #expect(fixture.identity.lastRequest?.displayName == "Alex")
    }

    /// Confirmed once, it is there at the next launch — a 24 h
    /// re-authentication must not ask again.
    @Test func theConfirmedNameSurvivesARelaunch() async throws {
        let fixture = try FamilySettingsFixture.unnamedModel()
        try await fixture.model.confirmDisplayName("Alex")
        let relaunched = FamilySettingsViewModel(
            store: fixture.store, coordinator: AccountCoordinator(
                store: fixture.store, keychain: fixture.keychain, identity: fixture.identity,
                apple: fixture.appleClient, credentialState: fixture.credentialStateClient, defaults: fixture.defaults
            ),
            client: fixture.client, defaults: fixture.defaults
        )
        #expect(relaunched.displayNameConfirmed)
        #expect(relaunched.profile.displayName == "Alex")
    }

    /// A server that still says the name is missing puts the name row back.
    @Test func displayNameRequiredFromTheServerAsksForTheNameAgain() async throws {
        let fixture = try FamilySettingsFixture.make(displayName: "", familyInBundle: false)
        fixture.identity.exchangeError = HTTPClientError.status(400, "display_name_required")
        await #expect(throws: FamilySyncError.displayNameConfirmationRequired) {
            try await fixture.model.signInWithApple()
        }
        #expect(!fixture.model.displayNameConfirmed)
    }

    @Test(arguments: ["", "   ", "\n\t"])
    func emptyConfirmationIsRefused(input: String) async throws {
        let fixture = try FamilySettingsFixture.unnamedModel()
        await #expect(throws: FamilySyncError.displayNameConfirmationRequired) {
            try await fixture.model.confirmDisplayName(input)
        }
        #expect(!fixture.model.displayNameConfirmed)
    }
}
