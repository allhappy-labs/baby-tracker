import BabyCore
import BabyProtocol
import CryptoKit
import Foundation

/// Where an import has got to.
///
/// Durable and ordered. Every one of these is a point a crash can happen at,
/// and resuming from the wrong one either loses the user's local data or
/// duplicates it into the family — so the phase is persisted before the work
/// it describes, never after.
public enum ImportPhase: String, Codable, Sendable, CaseIterable, Comparable {
    case endpointConfirmed, membershipReady, manifestFrozen, resolverRequested
    case mappingDurable, referencesRewritten, sharedRealmExposed, draining, complete, conflict

    private var order: Int { Self.allCases.firstIndex(of: self) ?? 0 }
    public static func < (lhs: Self, rhs: Self) -> Bool { lhs.order < rhs.order }
}

public struct EntityKey: Codable, Sendable, Hashable {
    public let kind: String
    public let id: UUID
    public init(kind: String, id: UUID) { self.kind = kind; self.id = id }
}

public struct FrozenImportManifestChunk: Codable, Sendable, Equatable {
    public let ordinal: Int
    public let manifestPayload: Data
    public let manifestHash: Data
    public init(ordinal: Int, manifestPayload: Data, manifestHash: Data) {
        self.ordinal = ordinal
        self.manifestPayload = manifestPayload
        self.manifestHash = manifestHash
    }
}

/// The immutable picture of local data at the moment the import began.
///
/// Frozen, because a retry must resend the *same* bytes under the same hash.
/// If a lost chunk were rebuilt from live rows, a record the user deleted in
/// the meantime would vanish from the retry and the server would accept a
/// different import than the one it half-received.
public struct FrozenImportManifest: Codable, Sendable, Equatable {
    public let sourceRevision: Int64
    public let chunks: [FrozenImportManifestChunk]
    /// The entity payloads as they were at freeze, keyed by kind and ID.
    public let sourceBases: [EntityKey: Data]

    public init(sourceRevision: Int64, chunks: [FrozenImportManifestChunk], sourceBases: [EntityKey: Data]) {
        self.sourceRevision = sourceRevision
        self.chunks = chunks
        self.sourceBases = sourceBases
    }

    /// `EntityKey` is not a string, so the default keyed container cannot
    /// encode the dictionary. Stored as pairs instead of stringifying the key,
    /// which would have to be parsed back and could collide.
    private enum CodingKeys: String, CodingKey { case sourceRevision, chunks, sourceBases }
    private struct BasePair: Codable { let key: EntityKey; let payload: Data }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        sourceRevision = try container.decode(Int64.self, forKey: .sourceRevision)
        chunks = try container.decode([FrozenImportManifestChunk].self, forKey: .chunks)
        sourceBases = try container.decode([BasePair].self, forKey: .sourceBases)
            .reduce(into: [:]) { $0[$1.key] = $1.payload }
    }

    public func encode(to encoder: any Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(sourceRevision, forKey: .sourceRevision)
        try container.encode(chunks, forKey: .chunks)
        // Sorted so the encoding is deterministic and two freezes of the same
        // rows produce the same bytes.
        let pairs = sourceBases
            .map { BasePair(key: $0.key, payload: $0.value) }
            .sorted { ($0.key.kind, $0.key.id.uuidString) < ($1.key.kind, $1.key.id.uuidString) }
        try container.encode(pairs, forKey: .sourceBases)
    }
}

/// Which import this is. Immutable from chunk 0.
///
/// The two modes are not interchangeable. Creating a family keeps every local
/// baby ID, including two that happen to share a normalized name and date —
/// they are two real children. Joining one matches against babies that already
/// exist, where that same pair is genuinely ambiguous and must be asked about
/// rather than guessed.
public enum ImportMode: String, Codable, Sendable {
    case preserveLocalIDs = "preserve_local_ids"
    case matchShared = "match_shared"
}

public enum OwnerConflictChoice: String, Codable, Sendable {
    case useExisting = "use_existing"
    case keepLocal = "keep_local"
}

public struct ImportConflictResolution: Codable, Sendable, Equatable {
    public let choice: OwnerConflictChoice
    public let canonicalBabyID: UUID?

    public init(choice: OwnerConflictChoice, canonicalBabyID: UUID?) {
        self.choice = choice
        self.canonicalBabyID = canonicalBabyID
    }

    /// The only two encodable shapes. `use_existing` without a target would
    /// point at nothing; `keep_local` with one would silently contradict
    /// itself.
    public var isValid: Bool {
        (choice == .useExisting && canonicalBabyID != nil) || (choice == .keepLocal && canonicalBabyID == nil)
    }
}

public enum ImportConflictError: Error, Sendable, Equatable {
    case ambiguousLocalBabies
}

public enum ImportStoreError: Error, Sendable, Equatable {
    case nonInjectiveMapping
    case importModeChanged
    case missingCheckpoint(UUID)
    case wrongPhase(expected: ImportPhase, actual: ImportPhase)
    case missingFrozenManifest(UUID)
    case invalidResolution
}

/// The durable state of one import.
public struct ImportCheckpointState: Codable, Sendable, Equatable {
    public var checkpointID: UUID
    public var phase: ImportPhase
    public var importMode: ImportMode
    public var completeBabyIDMap: [UUID: UUID]
    public var targetRealmKey: String?
    public var resolverRevision: Int64?
    public var conflictCount: Int?
    public var conflictsComplete: Bool
    public var nextConflictPageToken: String?

    public init(
        checkpointID: UUID,
        phase: ImportPhase,
        importMode: ImportMode,
        completeBabyIDMap: [UUID: UUID] = [:],
        targetRealmKey: String? = nil,
        resolverRevision: Int64? = nil,
        conflictCount: Int? = nil,
        conflictsComplete: Bool = false,
        nextConflictPageToken: String? = nil
    ) {
        self.checkpointID = checkpointID
        self.phase = phase
        self.importMode = importMode
        self.completeBabyIDMap = completeBabyIDMap
        self.targetRealmKey = targetRealmKey
        self.resolverRevision = resolverRevision
        self.conflictCount = conflictCount
        self.conflictsComplete = conflictsComplete
        self.nextConflictPageToken = nextConflictPageToken
    }
}

/// Checks a resolver response before any local reference is rewired.
public enum ImportStoreValidator {
    /// The final baby mapping must be injective.
    ///
    /// Two local babies collapsing onto one shared baby would merge two
    /// children's records irreversibly, and the rewrite happens in one
    /// transaction with no way back. Rejected before persistence, not after.
    public static func validate(_ mapping: [UUID: UUID]) throws {
        let targets = Set(mapping.values)
        guard targets.count == mapping.count else { throw ImportStoreError.nonInjectiveMapping }
    }

    /// The mode is fixed at chunk 0 and every later chunk or resume must agree.
    public static func validate(mode: ImportMode, against stored: ImportMode?) throws {
        guard let stored else { return }
        guard stored == mode else { throw ImportStoreError.importModeChanged }
    }

    public static func validate(_ resolution: ImportConflictResolution) throws {
        guard resolution.isValid else { throw ImportStoreError.invalidResolution }
    }

    /// Page tokens are bounded before transport rather than after a rejection.
    public static let maximumPageTokenBytes = 512
    public static func validatePageToken(_ token: String?) throws {
        guard let token else { return }
        guard Data(token.utf8).count <= maximumPageTokenBytes else { throw ImportStoreError.invalidResolution }
    }
}

/// The canonical hash of one manifest chunk's bytes.
enum ManifestHash {
    static func of(_ payload: Data) -> Data { Data(SHA256.hash(data: payload)) }
}

public extension FrozenImportManifest {
    /// The babies the resolver matches against, drawn from the frozen bases so
    /// a retry sends what the first attempt sent.
    ///
    /// Name and birth date go over as they were stored. The client does no
    /// normalization or matching of its own: P2 owns the normalizer, and a
    /// client that pre-folded names would disagree with the server about which
    /// two babies are the same child.
    func babies() throws -> [ImportBaby] {
        try sourceBases
            .filter { $0.key.kind == WireEntityKind.baby.rawValue }
            .sorted { $0.key.id.uuidString < $1.key.id.uuidString }
            .map { key, payload in
                let fields = try JSONSerialization.jsonObject(with: payload) as? [String: Any] ?? [:]
                return ImportBaby(
                    localBabyID: key.id,
                    name: fields["name"] as? String ?? "",
                    birthDate: fields["birth_date"] as? String ?? "",
                    sortOrder: Int64(fields["sort_order"] as? Int ?? 0)
                )
            }
    }

    /// Every non-baby entity, with the content hash the server compares
    /// against what it already holds. An equal hash is a confirmed no-op; an
    /// unequal one is an explicit conflict rather than a silent overwrite.
    ///
    /// The hash is the server's `CanonicalContentHash` of the payload exactly
    /// as the import mutation will carry it, because the server recomputes it
    /// from that mutation and refuses the record when the two differ.
    func records() throws -> [RecordManifest] {
        var manifests: [RecordManifest] = []
        for (key, payload) in sourceBases
            .filter({ $0.key.kind != WireEntityKind.baby.rawValue })
            .sorted(by: { ($0.key.kind, $0.key.id.uuidString) < ($1.key.kind, $1.key.id.uuidString) }) {
            let fields = (try? JSONSerialization.jsonObject(with: payload)) as? [String: Any] ?? [:]
            guard let babyID = (fields["baby_id"] as? String).flatMap(UUID.init(uuidString:)) else { continue }
            manifests.append(RecordManifest(
                recordID: key.id,
                entityKind: key.kind,
                localBabyID: babyID,
                contentSHA256: try ProtocolCanonicalizer.contentSHA256(
                    entityKind: key.kind,
                    payload: JSONDecoder().decode(JSONValue.self, from: payload)
                )
            ))
        }
        return manifests
    }
}
