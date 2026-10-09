import Foundation
import Testing
@testable import BabyProtocol

@Suite("Protocol v1 fixture conformance")
struct ProtocolFixtureConformanceTests {
    private var fixtures: ProtocolFixtureLoader {
        ProtocolFixtureLoader(directory: Self.repositoryRoot.appending(path: "Server/api/fixtures/v1"))
    }

    @Test func committedServerCorpusMatchesDigestManifest() throws {
        let result = try fixtures.verifyManifest()

        #expect(result.version == "v1")
        #expect(result.verifiedFileCount == 21)
    }

    @Test func decodesMutationSyncSnapshotAndImportFixtures() throws {
        let mutations = try fixtures.decode([String: WireMutation].self, named: "mutations.json")
        let sync = try fixtures.decode(SyncResponse.self, named: "sync-page.json")
        let snapshot = try fixtures.decode(SnapshotResponse.self, named: "snapshot-page.json")
        let importEnvelope = try fixtures.decode(ImportResolveFixture.self, named: "import-resolve.json")

        #expect(mutations["timer_bottle"]?.operation == .timerFinishBottle)
        #expect(sync.changes.count == 1)
        #expect(snapshot.entities.count == 1)
        #expect(importEnvelope.response.mappingComplete)
    }

    @Test func canonicalMutationMatchesServerLiteralBytesAndHash() throws {
        let vectors = try fixtures.decode(CanonicalVectors.self, named: "canonical-vectors.json")
        let mutation = try fixtures.decode(WireMutation.self, named: vectors.mutation.fixture)

        #expect(try ProtocolCanonicalizer.canonicalMutationBytes(mutation) == Data(vectors.mutation.canonicalJSON.utf8))
        #expect(try ProtocolCanonicalizer.canonicalMutationSHA256(mutation) == vectors.mutation.sha256)
    }

    /// The canonical form is derived from the payload's type, as Go's typed
    /// structs derive it: the fixture spells out its bottle's absent
    /// `seconds` and step bounds as null, and the same bottle without those
    /// keys — as this client sends it — has the same canonical bytes.
    @Test func canonicalMutationDerivesAPartsAbsentFields() throws {
        let vectors = try fixtures.decode(CanonicalVectors.self, named: "canonical-vectors.json")
        let raw = try String(contentsOf: fixtures.directory.appending(path: vectors.mutation.fixture), encoding: .utf8)
        let spelledOut = #","seconds":null,"started_at":null,"ended_at":null"#
        #expect(raw.contains(spelledOut))
        let sparse = try JSONDecoder().decode(
            WireMutation.self, from: Data(raw.replacingOccurrences(of: spelledOut, with: "").utf8)
        )

        #expect(try ProtocolCanonicalizer.canonicalMutationBytes(sparse) == Data(vectors.mutation.canonicalJSON.utf8))
        #expect(try ProtocolCanonicalizer.canonicalMutationSHA256(sparse) == vectors.mutation.sha256)
    }

    /// Every fixture mutation, one per payload type, hashes as the server
    /// hashes it. The literals are pinned on the Go side too
    /// (`TestCanonicalMutationHashOfEveryFixture` in
    /// `Server/internal/protocol/canonical_test.go`).
    @Test(arguments: [
        ("baby.json", nil, "3ed382f4152d6fa67d95e5a784db2f1bcdc64c7962dfb666a9bbcb14c0eb1574"),
        ("feed-bottle.json", nil, "4d28b2f9e3d1d1e59f45de30bd7a1aa362adcc6f1b3e06efb7f3b8a97b5fde11"),
        ("feed-breast.json", nil, "6c2031e5dce8b7244b069e91ac47e0a1af39881b3c95d6c23674d186c3a20af5"),
        ("feed-combination.json", nil, "8eafe1234e1f54264882fe0e1e719d5a94c8996387ed436536abd7fd86962052"),
        ("feed-solids.json", nil, "97ed3d0ca27e29f40e72d62c42ecec0d353cb0b3f266c7c57f9a282a9b5341e7"),
        ("sleep.json", nil, "6f4dac1e58bf7297ee069a6f764a18dcbf5beba4dd133eaae4e7c05b1c10addd"),
        ("diaper.json", nil, "5970a6698e951a13edad6cad5acc1ae352da289fc62c636535d077635f41ad27"),
        ("supplement.json", nil, "5fcc3ae7f9374061cfa8f72b225fe6f8e52ca73e9241bfa25736f970421ce30e"),
        ("growth-profile.json", nil, "c5c274fb7b44c8f91f79a32a78def410d021bd7c074fb885c34ec02ab752c220"),
        ("measurement.json", nil, "e99207fb7e59b99f2629a2af12052fa690fc9266a9d057b11992b5fa7a86b510"),
        ("timer-feed.json", nil, "4b3df663c3674fc2b755e8a35d09752ede55c0088446bc4e20a3e7b9d5949c77"),
        ("timer-sleep.json", nil, "5454d2982ab7d0a1a57d7ca8301bc70faae2d347c2f3d75d79770cfa2a2d8a21"),
        ("mutations.json", "delete_baby", "e7f1d0a8928958c575ae858747f808acc12daea4ce85d5bb67662367afe43cf4"),
        ("mutations.json", "delete_record", "182467c6d9c2ba3282bd3fbc530d6069070cc35f6e113da6241aa3af3f951941"),
        ("mutations.json", "timer_bottle", "78bb831d5a7a25a6fd22f89d6bb3fe967dc5ca6c48807aff0114f31b1989cc24"),
        ("mutations.json", "timer_edit", "2722b6c67c424c981c7a41c252942b0aae5bc3291745bbccb288ebd985128e27"),
        ("mutations.json", "timer_stop", "59a59902d50e8359bc94488e267a0da55721d8be8eb77123962b09b27a372dc8"),
    ] as [(String, String?, String)])
    func mutationHashOfEveryFixtureMatchesTheServer(fixture: String, key: String?, sha256: String) throws {
        let mutation = if let key {
            try #require(try fixtures.decode([String: WireMutation].self, named: fixture)[key])
        } else {
            try fixtures.decode(WireMutation.self, named: fixture)
        }

        #expect(try ProtocolCanonicalizer.canonicalMutationSHA256(mutation) == sha256)
    }

    @Test func timerEditFixtureDecodesAndMatchesItsCanonicalVector() throws {
        let vectors = try fixtures.decode(CanonicalVectors.self, named: "canonical-vectors.json")
        let mutations = try fixtures.decode([String: WireMutation].self, named: vectors.timerEditMutation.fixture)
        let key = try #require(vectors.timerEditMutation.key)
        let edit = try #require(mutations[key])

        #expect(edit.entityKind == .timer)
        #expect(edit.operation == .update)
        #expect(edit.payloadVersion == 5)
        #expect(try ProtocolCanonicalizer.canonicalMutationBytes(edit) == Data(vectors.timerEditMutation.canonicalJSON.utf8))
        #expect(try ProtocolCanonicalizer.canonicalMutationSHA256(edit) == vectors.timerEditMutation.sha256)
    }

    @Test func rejectsNestedDuplicateKeysAndUnsafeIntegerSpellings() throws {
        #expect(throws: ProtocolCanonicalizer.Error.self) {
            try ProtocolCanonicalizer.validateJSON(Data(#"{"payload":{"note":"a","note":"b"}}"#.utf8))
        }
        for raw in ["9007199254740992", "1.0", "1e0"] {
            #expect(throws: ProtocolCanonicalizer.Error.self) {
                try ProtocolCanonicalizer.validateSafeIntegerJSON(Data(raw.utf8))
            }
        }
    }

    @Test func canonicalDecimalTimestampAndNameVectorsMatchServer() throws {
        let vectors = try fixtures.decode(CanonicalVectors.self, named: "canonical-vectors.json")

        for vector in vectors.decimals {
            #expect(try ProtocolCanonicalizer.canonicalDecimal(vector.input) == vector.canonical)
        }
        for vector in vectors.timestamps where vector.valid {
            #expect(try ProtocolCanonicalizer.canonicalTimestamp(vector.input) == vector.canonical)
        }
        for vector in vectors.unicode.normalization {
            #expect(try ProtocolCanonicalizer.normalizedBabyName(vector.input) == vector.canonical)
        }
    }

    /// The sleep v2 / timer v5 session-link content hash, computed here and
    /// pinned by the committed vector — the same one Go's
    /// `TestCanonicalJSONMatchesIndependentFixtureLiteral` checks against its
    /// own `CanonicalContentHash`.
    @Test func sleepV2AndTimerV5ContentHashesMatchTheServerVectors() throws {
        let vectors = try fixtures.decode(CanonicalVectors.self, named: "canonical-vectors.json")

        for vector in [vectors.sessionContentHashes.sleepV2, vectors.sessionContentHashes.timerV5] {
            let mutation = try JSONDecoder().decode(WireMutation.self, from: Data(vector.mutationJSON.utf8))
            #expect(mutation.entityKind.rawValue == vector.entityKind)
            #expect(mutation.payloadVersion == vector.payloadVersion)
            let sha256 = try ProtocolCanonicalizer.contentSHA256(
                entityKind: mutation.entityKind.rawValue,
                payload: mutation.payload
            )
            #expect(sha256 == vector.sha256)
        }
    }

    private static var repositoryRoot: URL {
        URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
    }
}
