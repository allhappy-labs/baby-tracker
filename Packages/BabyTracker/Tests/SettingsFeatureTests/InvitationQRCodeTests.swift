import BabyStore
import BabySync
import CoreImage
import Foundation
import Testing
@testable import SettingsFeature

/// The QR code an owner shows is the whole hand-off to the other phone, so it
/// is proven the way a camera would read it: rendered, detected, and parsed
/// back to the same invitation.
@Suite("Invitation QR code")
struct InvitationQRCodeTests {
    private static let token = "Abcdefghijklmnopqrstuvwxyz0123456789-_ABCDE"

    private func invitation(_ link: String) -> FamilyInvitation {
        FamilyInvitation(id: UUID(), shareURL: URL(string: link)!, role: .caregiver, expiresAt: .distantFuture)
    }

    @Test func payloadIsTheAppLinkWithTheServerAndToken() throws {
        let payload = try InvitationQRCode.payload(
            for: invitation("https://sync.example.test:8443/invite#token=\(Self.token)")
        )

        #expect(payload == "babytracker://invite?server=https%3A%2F%2Fsync.example.test%3A8443&token=\(Self.token)")
    }

    @Test func renderedCodeReadsBackToTheSameIntent() throws {
        let link = "https://sync.example.test/invite#token=\(Self.token)"
        let payload = try InvitationQRCode.payload(for: invitation(link))

        let image = try #require(InvitationQRCode.cgImage(for: payload))
        let detector = try #require(
            CIDetector(ofType: CIDetectorTypeQRCode, context: nil, options: [CIDetectorAccuracy: CIDetectorAccuracyHigh])
        )
        let features = detector.features(in: CIImage(cgImage: image)).compactMap { $0 as? CIQRCodeFeature }
        let read = try #require(features.first?.messageString)

        #expect(read == payload)
        #expect(try InvitationParser.parse(read) == InvitationParser.parse(link))
    }

    @Test func aLinkThatIsNotAnInvitationHasNoPayload() {
        #expect(throws: InvitationParser.ParseError.self) {
            try InvitationQRCode.payload(for: invitation("https://sync.example.test/elsewhere"))
        }
    }
}
