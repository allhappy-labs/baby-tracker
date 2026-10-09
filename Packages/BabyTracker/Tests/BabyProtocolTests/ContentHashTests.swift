import CryptoKit
import Foundation
import Testing
@testable import BabyProtocol

/// The import manifest's `content_sha256` must equal the server's
/// `CanonicalContentHash`, or `AdmitRecord` refuses every imported record as
/// `import_manifest_mismatch`. The literals below are pinned on the Go side
/// too (`Server/internal/protocol/content_hash_test.go`), so either
/// implementation drifting fails its own test.
@Suite("Import content hash")
struct ContentHashTests {
    struct Vector: Sendable, CustomTestStringConvertible {
        let name: String
        let mutationJSON: String
        let canonicalJSON: String
        let sha256: String
        var testDescription: String { name }
    }

    /// A decomposed accent and a raw line separator, both of which the
    /// server's encoder treats specially.
    static let unicodeNote = "Cafe\u{0301} \u{2028}top-up"

    /// Go's `contentHashVectors`, one for one: the same mutation, canonical
    /// JSON and hash.
    static let vectors: [Vector] = [
        Vector(
            name: "supplement fixture",
            mutationJSON: fixture("supplement.json"),
            canonicalJSON: #"{"entity_kind":"supplement","payload":{"logged_at":"2026-09-20T15:00:00.000000Z","name":"Vitamin D","note":null,"started_at":"2026-09-20T14:10:00.000000Z","time_zone_id":"Europe/Zurich"},"payload_version":1}"#,
            sha256: "ada019a1605551c30b8a3926f283b062b0939f229fbc3be9f1133df9ad5307c9"
        ),
        Vector(
            name: "sleep fixture",
            mutationJSON: fixture("sleep.json"),
            canonicalJSON: #"{"entity_kind":"sleep","payload":{"ended_at":"2026-09-20T14:00:00.000000Z","kind_override":"nap","note":null,"sleep_session_id":null,"started_at":"2026-09-20T13:00:00.000000Z","time_zone_id":"Europe/Zurich"},"payload_version":1}"#,
            sha256: "d3fd9350402e119e2bf99e814b8e963702f039660e1a2c8eb87198c3e61c5e5e"
        ),
        Vector(
            name: "diaper fixture",
            mutationJSON: fixture("diaper.json"),
            canonicalJSON: #"{"entity_kind":"diaper","payload":{"color":"mustard","consistency":"soft","contents":"mixed","ended_at":null,"note":null,"started_at":"2026-09-20T14:10:00.000000Z","time_zone_id":"Europe/Zurich"},"payload_version":1}"#,
            sha256: "32d53d1dd1b2ac16c08953f678d5f163d4804d243d5be00f1514e114552aa029"
        ),
        Vector(
            // The shape the client freezes: millisecond times, a decomposed
            // accent and a raw U+2028 in the note, a fractional bottle volume
            // and a solids part.
            name: "feed v4 composite",
            mutationJSON: #"{"mutation_id":"01000000-0000-4000-8000-0000000000a1","entity_id":"20000000-0000-4000-8000-0000000000a1","entity_kind":"feed","operation":"create","payload_version":4,"observed_entity_version":0,"client_mutation_time":"2026-09-24T10:31:00.000Z","import_id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","payload":{"baby_id":"10000000-0000-4000-8000-000000000001","kind":"breast","started_at":"2026-09-24T10:00:00.250Z","ended_at":"2026-09-24T10:30:00.000Z","time_zone_id":"Europe/Zurich","note":"NOTE","feeding_session_id":null,"components":[{"id":"30000000-0000-4000-8000-000000000001","kind":"breast","side":"left","seconds":480},{"id":"30000000-0000-4000-8000-000000000002","kind":"bottle","volume_ml":50.125,"contents":"breast_milk"},{"id":"30000000-0000-4000-8000-000000000003","kind":"solids","foods":["banana","yogurt"],"amount":"some"}]}}"#
                .replacingOccurrences(of: "NOTE", with: unicodeNote),
            canonicalJSON: #"{"entity_kind":"feed","payload":{"components":[{"ended_at":null,"id":"30000000-0000-4000-8000-000000000001","kind":"breast","seconds":480,"side":"left","started_at":null},{"contents":"breast_milk","ended_at":null,"id":"30000000-0000-4000-8000-000000000002","kind":"bottle","seconds":null,"started_at":null,"volume_ml":50.125},{"amount":"some","foods":["banana","yogurt"],"id":"30000000-0000-4000-8000-000000000003","kind":"solids"}],"ended_at":"2026-09-24T10:30:00.000000Z","feeding_session_id":null,"kind":"breast","note":"NOTE","started_at":"2026-09-24T10:00:00.250000Z","time_zone_id":"Europe/Zurich"},"payload_version":1}"#
                .replacingOccurrences(of: "NOTE", with: "Caf\u{00e9} \u{2028}top-up"),
            sha256: "27afa828b79af603d09a39f2ad57cef405a70f9f795741998fd83583857f355a"
        ),
        Vector(
            // The client omits an empty note and end rather than sending null.
            name: "feed v4 sparse",
            mutationJSON: #"{"mutation_id":"01000000-0000-4000-8000-0000000000a2","entity_id":"20000000-0000-4000-8000-0000000000a2","entity_kind":"feed","operation":"create","payload_version":4,"observed_entity_version":0,"client_mutation_time":"2026-09-24T11:00:00.000Z","import_id":null,"payload":{"baby_id":"10000000-0000-4000-8000-000000000001","kind":"bottle","started_at":"2026-09-24T11:00:00.000Z","time_zone_id":"Europe/Zurich","feeding_session_id":null,"components":[{"id":"30000000-0000-4000-8000-000000000004","kind":"bottle","volume_ml":60,"contents":"formula"}]}}"#,
            canonicalJSON: #"{"entity_kind":"feed","payload":{"components":[{"contents":"formula","ended_at":null,"id":"30000000-0000-4000-8000-000000000004","kind":"bottle","seconds":null,"started_at":null,"volume_ml":60}],"ended_at":null,"feeding_session_id":null,"kind":"bottle","note":null,"started_at":"2026-09-24T11:00:00.000000Z","time_zone_id":"Europe/Zurich"},"payload_version":1}"#,
            sha256: "67659c4328a6ffe8b5c8d1d017f95ba35866aec597f58d4df4c63107a30c99a9"
        ),
        Vector(
            name: "timer v5 running feed",
            mutationJSON: #"{"mutation_id":"01000000-0000-4000-8000-0000000000a3","entity_id":"50000000-0000-4000-8000-0000000000a3","entity_kind":"timer","operation":"timer_start","payload_version":5,"observed_entity_version":0,"client_mutation_time":"2026-09-24T12:00:00.000Z","import_id":null,"payload":{"baby_id":"10000000-0000-4000-8000-000000000001","timer_kind":"feed","started_at":"2026-09-24T11:50:00.000Z","time_zone_id":"Europe/Zurich","completed_components":[{"id":"30000000-0000-4000-8000-000000000005","kind":"breast","side":"left","seconds":300}],"current_side":"right","current_side_started_at":"2026-09-24T11:55:00.000Z","current_bottle_started_at":null,"session_id":null}}"#,
            canonicalJSON: #"{"entity_kind":"timer","payload":{"completed_components":[{"ended_at":null,"id":"30000000-0000-4000-8000-000000000005","kind":"breast","seconds":300,"side":"left","started_at":null}],"current_bottle_started_at":null,"current_side":"right","current_side_started_at":"2026-09-24T11:55:00.000000Z","session_id":null,"started_at":"2026-09-24T11:50:00.000000Z","time_zone_id":"Europe/Zurich","timer_kind":"feed"},"payload_version":1}"#,
            sha256: "e604e2e693e8e56bf6a90b736efe1bc77f73ac435a3813b5338cef0b675d8b05"
        ),
    ]

    @Test(arguments: vectors)
    func contentHashMatchesTheServer(_ vector: Vector) throws {
        let mutation = try JSONDecoder().decode(WireMutation.self, from: Data(vector.mutationJSON.utf8))

        let bytes = try ProtocolCanonicalizer.canonicalContentBytes(
            entityKind: mutation.entityKind.rawValue, payload: mutation.payload
        )

        #expect(String(decoding: bytes, as: UTF8.self) == vector.canonicalJSON)
        #expect(try ProtocolCanonicalizer.contentSHA256(
            entityKind: mutation.entityKind.rawValue, payload: mutation.payload
        ) == vector.sha256)
    }

    @Test func growthContentHashesMatchServerVectors() throws {
        for (name, hash, mutationHash) in [
            ("growth-profile.json", "3250e46c4d908a49a89e34d14ab78c3bb73d8cd225b497e64a763172e24fca6a", "c5c274fb7b44c8f91f79a32a78def410d021bd7c074fb885c34ec02ab752c220"),
            ("measurement.json", "76464011b33e02e20fd0fc1a25f1190f100c533d8b6b0ad25b94d02090f9625b", "e99207fb7e59b99f2629a2af12052fa690fc9266a9d057b11992b5fa7a86b510"),
        ] {
            let mutation = try Self.loader.decode(WireMutation.self, named: name)
            #expect(try ProtocolCanonicalizer.contentSHA256(entityKind: mutation.entityKind.rawValue,
                payload: mutation.payload) == hash)
            #expect(try ProtocolCanonicalizer.canonicalMutationSHA256(mutation) == mutationHash)
        }
    }

    /// A timed bottle's seconds and a running bottle's start are content.
    @Test func timedBottleFieldsAreContent() throws {
        let bottle: JSONValue = .object([
            "kind": .string("bottle"), "id": .string("30000000-0000-4000-8000-000000000001"),
            "volume_ml": .number(60), "contents": .string("formula"), "seconds": .number(135),
        ])
        let feed: JSONValue = .object([
            "kind": .string("bottle"), "started_at": .string("2026-09-29T10:00:00Z"),
            "time_zone_id": .string("Europe/Zurich"), "components": .array([bottle]),
            "feeding_session_id": .null,
        ])
        let timer: JSONValue = .object([
            "timer_kind": .string("feed"), "started_at": .string("2026-09-29T10:00:00Z"),
            "time_zone_id": .string("Europe/Zurich"), "completed_components": .array([bottle]),
            "current_bottle_started_at": .string("2026-09-29T10:01:00Z"), "session_id": .null,
        ])
        let feedJSON = String(decoding: try ProtocolCanonicalizer.canonicalContentBytes(entityKind: "feed", payload: feed), as: UTF8.self)
        let timerJSON = String(decoding: try ProtocolCanonicalizer.canonicalContentBytes(entityKind: "timer", payload: timer), as: UTF8.self)
        #expect(feedJSON.contains("\"seconds\":135"))
        #expect(timerJSON.contains("\"current_bottle_started_at\":\"2026-09-29T10:01:00.000000Z\""))
        #expect(timerJSON.contains("\"seconds\":135"))
    }

    /// The fixture corpus's own encoder vector: U+2028/U+2029 go out as raw
    /// UTF-8, while the same six characters typed as text stay escaped text.
    @Test func lineSeparatorVectorMatchesTheServerEncoder() throws {
        let vectors = try Self.loader.decode(CanonicalVectors.self, named: "canonical-vectors.json")
        let unicode = try #require(vectors.unicode.json)

        let bytes = Data(try ProtocolCanonicalizer.canonicalJSON(.object([
            "actual": .string(unicode.actual),
            "literal": .string(unicode.literal),
        ])).utf8)

        #expect(String(decoding: bytes, as: UTF8.self) == unicode.canonicalJSON)
        #expect(SHA256.hash(data: bytes).map { String(format: "%02x", $0) }.joined() == unicode.sha256)
    }

    /// Moved from the deleted `LegacyFeedUpgradeTests`. ICU's `$` also
    /// matches before a trailing newline; Go's does not, so the server hashes
    /// such a value as text.
    @Test func timestampWithTrailingNewlineIsHashedAsText() {
        let raw = "2026-09-20T12:00:00Z\n"
        #expect(ProtocolCanonicalizer.hashTimestamp(raw) == raw)
        #expect(throws: ProtocolCanonicalizer.Error.self) { try ProtocolCanonicalizer.canonicalTimestamp(raw) }
    }

    static var loader: ProtocolFixtureLoader {
        ProtocolFixtureLoader(directory: URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appending(path: "Server/api/fixtures/v1"))
    }

    static func fixture(_ name: String) -> String {
        (try? String(contentsOf: loader.directory.appending(path: name), encoding: .utf8)) ?? ""
    }
}
