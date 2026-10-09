import BabySync
import Foundation
import Testing
@testable import SettingsFeature

@MainActor
@Suite("Server selection copy and validation")
struct ServerSelectionTests {
    @Test func productionEndpointIsBoundToPrivateTailnetServerIdentity() throws {
        let endpoint = try #require(FamilySyncComposition.hostedEndpoint)
        #expect(endpoint.origin.canonicalString == "https://baby-sync.example-tailnet.ts.net")
        #expect(endpoint.serverInstanceID == UUID(uuidString: "11111111-2222-4333-8444-555555555555"))
    }

    /// Every rejection gets a sentence that says what to change, because the
    /// alternative is a user pasting the same bad address repeatedly.
    @Test(arguments: [
        ServerOrigin.ValidationError.schemeNotHTTPS,
        .credentialsPresent, .pathPresent, .queryPresent,
        .fragmentPresent, .missingHost, .notAURL, .invalidPort,
    ])
    func everyValidationErrorHasGuidance(error: ServerOrigin.ValidationError) {
        let described = ServerSelectionView.describe(error)
        #expect(!described.isEmpty)
        #expect(described.hasSuffix("."))
    }

    @Test func httpsGuidanceNamesTheScheme() {
        #expect(ServerSelectionView.describe(.schemeNotHTTPS).contains("https://"))
    }

    @Test func credentialGuidanceNamesWhatToRemove() {
        #expect(ServerSelectionView.describe(.credentialsPresent).contains("password"))
    }

    /// The placeholder exists so a refused, changed instance still shows the
    /// user which ID it now reports — refusing silently would leave them
    /// unable to judge whether the change was legitimate.
    @Test func placeholderCarriesTheReportedInstance() throws {
        let origin = try ServerOrigin(validating: "https://sync.example.test")
        let reported = UUID()
        let info = ServerInfoPlaceholder.make(origin: origin, instanceID: reported)
        #expect(info.serverInstanceID == reported)
        #expect(info.canonicalOrigin == origin.canonicalString)
    }
}
