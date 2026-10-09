import Foundation
import Testing
import BabyCore
@testable import BabyProtocol

@Suite("Feed component wire shape")
struct CanonicalProtocolTests {
    private let feedID = UUID(uuidString: "20000000-0000-4000-8000-000000000001")!

    /// Parts keep their order, IDs and decimals, and each gets the fields its
    /// kind has on the server, null when this client left them out.
    @Test func canonicalFeedPreservesOrderNamesIDsAndDecimals() throws {
        let components: [WireFeedComponent] = [
            .breast(
                id: UUID(uuidString: "30000000-0000-4000-8000-000000000001")!,
                side: .left,
                seconds: 480
            ),
            .bottle(
                id: UUID(uuidString: "30000000-0000-4000-8000-000000000002")!,
                volumeML: Decimal(string: "50.125000")!,
                contents: .breastMilk
            ),
        ]
        let encodedComponents = try JSONDecoder().decode(JSONValue.self, from: JSONEncoder().encode(components))
        let mutation = WireMutation(
            mutationID: UUID(),
            entityID: feedID,
            entityKind: .feed,
            operation: .create,
            payloadVersion: 4,
            observedEntityVersion: 0,
            clientMutationTime: "2026-09-24T10:01:02Z",
            importID: nil,
            payload: .object([
                "baby_id": .string("10000000-0000-4000-8000-000000000001"),
                "kind": .string("breast"),
                "started_at": .string("2026-09-24T10:00:00Z"),
                "time_zone_id": .string("Europe/Zurich"),
                "feeding_session_id": .null,
                "components": encodedComponents,
            ])
        )

        #expect(String(decoding: try ProtocolCanonicalizer.canonicalMutationBytes(mutation), as: UTF8.self) ==
            #"{"client_mutation_time":"2026-09-24T10:01:02.000000Z","entity_id":"20000000-0000-4000-8000-000000000001","entity_kind":"feed","import_id":null,"observed_entity_version":0,"operation":"create","payload":{"baby_id":"10000000-0000-4000-8000-000000000001","components":[{"ended_at":null,"id":"30000000-0000-4000-8000-000000000001","kind":"breast","seconds":480,"side":"left","started_at":null},{"contents":"breast_milk","ended_at":null,"id":"30000000-0000-4000-8000-000000000002","kind":"bottle","seconds":null,"started_at":null,"volume_ml":50.125}],"ended_at":null,"feeding_session_id":null,"kind":"breast","note":null,"started_at":"2026-09-24T10:00:00.000000Z","time_zone_id":"Europe/Zurich"},"payload_version":4}"#
        )
    }

    @Test(arguments: [
        #"{"id":"30000000-0000-4000-8000-000000000001","kind":"unknown"}"#,
        #"{"id":"30000000-0000-4000-8000-000000000001","kind":"breast","side":"left","seconds":10,"volume_ml":20}"#,
        #"{"id":"30000000-0000-4000-8000-000000000001","kind":"bottle","volume_ml":20}"#,
        #"{"id":"30000000-0000-4000-8000-000000000001","kind":"bottle","volume_ml":20,"contents":"formula","seconds":0}"#,
        #"{"id":"30000000-0000-4000-8000-000000000001","kind":"bottle","volume_ml":20,"contents":"formula","seconds":-1}"#,
        #"{"id":"30000000-0000-4000-8000-000000000001","kind":"solids","foods":[],"amount":"some","side":"right"}"#,
    ])
    func invalidOrMixedComponentFieldsAreRejected(json: String) {
        #expect(throws: (any Error).self) {
            try JSONDecoder().decode(WireFeedComponent.self, from: Data(json.utf8))
        }
    }

    @Test func timedBottleRoundTripsWithoutANullSeconds() throws {
        let id = UUID(uuidString: "30000000-0000-4000-8000-000000000001")!
        let timed = WireFeedComponent.bottle(id: id, volumeML: 60, contents: .formula, seconds: 135)
        let untimed = WireFeedComponent.bottle(id: id, volumeML: 60, contents: .formula)
        let timedJSON = String(decoding: try JSONEncoder().encode(timed), as: UTF8.self)
        let untimedJSON = String(decoding: try JSONEncoder().encode(untimed), as: UTF8.self)
        #expect(timedJSON.contains("\"seconds\":135"))
        #expect(!untimedJSON.contains("seconds"))
        #expect(try JSONDecoder().decode(WireFeedComponent.self, from: Data(timedJSON.utf8)) == timed)
        let payload = try JSONDecoder().decode(JSONValue.self, from: Data("{\"components\":[\(timedJSON)]}".utf8))
        #expect(try WireFeedComponent.decodeComponents("components", of: payload) == [timed])
    }

    @Test func feedCarriesClearedLinkAndExactStepBounds() throws {
        let raw = #"{"baby_id":"10000000-0000-4000-8000-000000000001","kind":"bottle","started_at":"2026-09-24T10:00:00Z","ended_at":"2026-09-24T10:02:00Z","time_zone_id":"UTC","note":null,"feeding_session_id":null,"components":[{"id":"30000000-0000-4000-8000-000000000001","kind":"bottle","seconds":120,"volume_ml":60,"contents":"formula","started_at":"2026-09-24T10:00:00Z","ended_at":"2026-09-24T10:02:00Z"}]}"#
        let payload = try JSONDecoder().decode(JSONValue.self, from: Data(raw.utf8))
        let parts = try WireFeedComponent.decodeComponents("components", of: payload)
        #expect(parts.count == 1)
        #expect(parts[0].startedAt == "2026-09-24T10:00:00Z")
        #expect(parts[0].endedAt == "2026-09-24T10:02:00Z")
        let content = String(decoding: try ProtocolCanonicalizer.canonicalContentBytes(entityKind: "feed", payload: payload), as: UTF8.self)
        #expect(content.contains(#""feeding_session_id":null"#))
        #expect(content.contains(#""started_at":"2026-09-24T10:00:00.000000Z""#))
        #expect(content.contains(#""ended_at":"2026-09-24T10:02:00.000000Z""#))
    }

    /// The scalar shape before parts — `sides`, `volume_ml`, `contents`,
    /// `foods` and `amount` on the record — is not read: the server sends
    /// only `components`, and a payload without them has no parts to give.
    @Test func theScalarShapeHasNoParts() {
        let payload: JSONValue = .object([
            "sides": .array([.object(["side": .string("left"), "seconds": .number(480)])]),
            "volume_ml": .number(50),
            "contents": .string("formula"),
            "foods": .array([]),
            "amount": .null,
        ])
        #expect(throws: FeedComponentWireError.invalidShape("components")) {
            try WireFeedComponent.decodeComponents("components", of: payload)
        }
    }
}
