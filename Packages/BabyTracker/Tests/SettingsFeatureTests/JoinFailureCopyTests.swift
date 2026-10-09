import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// `JoinFailureCopy.message(for:)` is the only place that decides what a
/// caregiver reads next to the invitation-code field. Each case here is a
/// failure the final review found silently collapsed into the same generic
/// sentence: a network that never reached the server, a link for a server
/// this device never confirmed, and a renewal the server refused outright.
@Suite("Join failure copy")
struct JoinFailureCopyTests {
    @Test func aNetworkFailureSaysToCheckTheConnection() {
        let message = JoinFailureCopy.message(for: URLError(.notConnectedToInternet))
        #expect(message == FamilyCopy.joinConnectionProblem)
    }

    @Test func aTimedOutRequestAlsoReadsAsAConnectionProblem() {
        let message = JoinFailureCopy.message(for: URLError(.timedOut))
        #expect(message == FamilyCopy.joinConnectionProblem)
    }

    @Test func aLinkForAnUnconfirmedServerNamesTheMismatch() {
        let message = JoinFailureCopy.message(for: FamilySyncError.endpointConfirmationRequired)
        #expect(message == FamilyCopy.joinWrongServer)
    }

    @Test func anExpiredInvitationNamesItself() {
        let message = JoinFailureCopy.message(for: HTTPClientError.status(409, "invitation_expired"))
        #expect(message == FamilyCopy.invitationExpired)
    }

    @Test func anAlreadyConsumedInvitationNamesItself() {
        let message = JoinFailureCopy.message(for: HTTPClientError.status(409, "invitation_consumed"))
        #expect(message == FamilyCopy.invitationConsumed)
    }

    @Test func aRevokedInvitationNamesItself() {
        let message = JoinFailureCopy.message(for: HTTPClientError.status(409, "invitation_revoked"))
        #expect(message == FamilyCopy.invitationRevokedCopy)
    }

    /// A renewal the server refuses outright — or any other server code this
    /// screen has no specific words for — still reads as a plain refusal
    /// rather than a crash or a blank field.
    @Test func anUnrecognizedServerCodeFallsBackToTheGenericRefusal() {
        let message = JoinFailureCopy.message(for: HTTPClientError.status(401, "access_expired"))
        #expect(message == FamilyCopy.joinFailed)
    }

    @Test func aStatusWithNoServerCodeFallsBackToTheGenericRefusal() {
        let message = JoinFailureCopy.message(for: HTTPClientError.status(500, nil))
        #expect(message == FamilyCopy.joinFailed)
    }

    @Test func anEntirelyUnrelatedErrorFallsBackToTheGenericRefusal() {
        struct Unrelated: Error {}
        let message = JoinFailureCopy.message(for: Unrelated())
        #expect(message == FamilyCopy.joinFailed)
    }

    @Test func aMalformedCodeReadsAsInvalidRatherThanAServerRefusal() {
        let message = JoinFailureCopy.message(for: InvitationParser.ParseError.malformedToken)
        #expect(message == FamilyCopy.invalidInvitationCode)
    }

    @Test func anUnsupportedSchemeReadsAsInvalidRatherThanAServerRefusal() {
        let message = JoinFailureCopy.message(for: InvitationParser.ParseError.unsupportedScheme)
        #expect(message == FamilyCopy.invalidInvitationCode)
    }
}
