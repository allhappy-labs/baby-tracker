import Foundation
import Testing
@testable import BabySync

@Suite("Endpoint identity confirmation")
struct EndpointIdentityTests {
    private let probe = EndpointIdentityProbe()

    private func info(
        origin: String = SessionFixture.origin.canonicalString,
        instance: UUID = SessionFixture.instanceID
    ) -> ServerInfo {
        ServerInfo(
            serverInstanceID: instance, canonicalOrigin: origin, recoveryEpoch: 0,
            capabilities: ["records-v1", "live-hints-v1"], appleAuthorizationMode: "hosted",
            maxItemsPerPage: 200, maxEncodedBytes: 1_000_000,
            sourceVersion: "1.0.0", sourceURL: "https://example.test/src"
        )
    }

    @Test func firstConfirmationBindsTheInstance() throws {
        let confirmed = try probe.confirm(origin: SessionFixture.origin, info: info(), knownInstanceID: nil)
        #expect(confirmed.serverInstanceID == SessionFixture.instanceID)
        #expect(confirmed.origin == SessionFixture.origin)
    }

    @Test func sameInstanceOnAKnownOriginIsAccepted() throws {
        let confirmed = try probe.confirm(
            origin: SessionFixture.origin, info: info(), knownInstanceID: SessionFixture.instanceID
        )
        #expect(confirmed.serverInstanceID == SessionFixture.instanceID)
    }

    /// An authenticated endpoint is immutable. A changed instance at a known
    /// origin is blocked here so no credential or record is sent before the
    /// user has been asked about it.
    @Test func changedInstanceIsBlockedUntilConfirmed() throws {
        #expect(throws: EndpointIdentityError.instanceChanged(
            known: SessionFixture.otherInstanceID, reported: SessionFixture.instanceID
        )) {
            try probe.confirm(origin: SessionFixture.origin, info: info(), knownInstanceID: SessionFixture.otherInstanceID)
        }
    }

    /// The server naming a different origin is refused outright: an advertised
    /// origin is never accepted as a replacement target.
    @Test func advertisedOriginIsNeverAcceptedAsAReplacement() throws {
        #expect(throws: EndpointIdentityError.originMismatch(
            dialled: SessionFixture.origin.canonicalString,
            advertised: "https://elsewhere.example"
        )) {
            try probe.confirm(
                origin: SessionFixture.origin,
                info: info(origin: "https://elsewhere.example"),
                knownInstanceID: nil
            )
        }
    }

    @Test func redirectsAreRefused() throws {
        #expect(throws: EndpointIdentityError.redirected) {
            try probe.confirm(origin: SessionFixture.origin, info: info(), knownInstanceID: nil, wasRedirected: true)
        }
    }

    @Test func liveHintsAreOnlyUsedWhenAdvertised() {
        #expect(info().advertisesLiveHints)
        let without = ServerInfo(
            serverInstanceID: SessionFixture.instanceID,
            canonicalOrigin: SessionFixture.origin.canonicalString,
            recoveryEpoch: 0, capabilities: ["records-v1"], appleAuthorizationMode: "hosted",
            maxItemsPerPage: 200, maxEncodedBytes: 1_000_000,
            sourceVersion: "1.0.0", sourceURL: ""
        )
        #expect(!without.advertisesLiveHints)
    }
}
