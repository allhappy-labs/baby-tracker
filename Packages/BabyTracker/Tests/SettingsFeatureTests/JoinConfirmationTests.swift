import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// "Join {family}?" — the step both the scanner and a deep link go through.
/// It names the family from the server's preview before anything is
/// redeemed, and a preview that fails never joins.
@MainActor
@Suite("Join confirmation")
struct JoinConfirmationTests {
    private static let token = "Abcdefghijklmnopqrstuvwxyz0123456789-_ABCDE"

    /// Signed in, with a name, and no family yet.
    private func signedInWithoutFamily() async throws -> FamilySettingsFixture {
        let fixture = try FamilySettingsFixture.make(familyInBundle: false)
        try await fixture.model.signInWithApple()
        #expect(!fixture.model.hasFamily)
        return fixture
    }

    @Test func joinConfirmationShowsFamilyNameThenJoins() async throws {
        let fixture = try await signedInWithoutFamily()
        fixture.client.previewResult = InvitationPreview(familyName: "The Kovals", expiresAt: .distantFuture)
        let confirmation = JoinConfirmation(
            model: fixture.model, intent: InvitationIntent(origin: FamilySettingsFixture.origin, token: Self.token)
        )

        await confirmation.load()
        #expect(confirmation.state == .ready(familyName: "The Kovals"))
        #expect(JoinConfirmationCopy.title(familyName: "The Kovals") == "Join The Kovals?")
        #expect(fixture.client.previewedTokens == [Self.token])
        #expect(!fixture.model.hasFamily, "previewing joined the family")

        await confirmation.join()
        #expect(confirmation.state == .joined)
        #expect(fixture.model.hasFamily)
    }

    @Test func previewExpiredShowsJoinFailureCopyAndDoesNotJoin() async throws {
        let fixture = try await signedInWithoutFamily()
        fixture.client.previewErrors = [HTTPClientError.status(410, "invitation_expired")]
        let confirmation = JoinConfirmation(
            model: fixture.model, intent: InvitationIntent(origin: FamilySettingsFixture.origin, token: Self.token)
        )

        await confirmation.load()
        #expect(confirmation.state == .failed(FamilyCopy.invitationExpired))

        // Join does nothing from a failed preview.
        await confirmation.join()
        #expect(confirmation.state == .failed(FamilyCopy.invitationExpired))
        #expect(!fixture.model.hasFamily)
    }

    /// A link for another server is refused before the token goes anywhere:
    /// previewing it would send it to this phone's server, not the one that
    /// minted it.
    @Test func deepLinkForAnotherServerShowsTheOrigin() async throws {
        let fixture = try await signedInWithoutFamily()
        let other = try ServerOrigin(validating: "https://other.example.test")
        let confirmation = JoinConfirmation(model: fixture.model, intent: InvitationIntent(origin: other, token: Self.token))

        await confirmation.load()
        guard case .failed(let message) = confirmation.state else {
            Issue.record("expected a failure, got \(confirmation.state)")
            return
        }
        #expect(message.contains(FamilyCopy.joinWrongServer))
        #expect(message.contains("https://other.example.test"))
        #expect(fixture.client.previewedTokens.isEmpty, "the token was sent to this phone's server")
        await confirmation.join()
        #expect(!fixture.model.hasFamily)
    }
}
