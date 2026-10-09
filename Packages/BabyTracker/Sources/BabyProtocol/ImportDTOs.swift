import Foundation

public struct ImportResolveFixture: Codable, Sendable, Equatable {
    public let request: ImportResolveRequest
    public let response: ImportResolveResponse
}

public struct ImportResolveRequest: Codable, Sendable, Equatable {
    public init(
        protocolVersion: String, capabilities: [String], importID: UUID, importMode: String,
        normalizationVersion: String, babies: [ImportBaby], records: [RecordManifest],
        totalRecordCount: Int64, chunkIndex: Int64, finalize: Bool
    ) {
        self.protocolVersion = protocolVersion
        self.capabilities = capabilities
        self.importID = importID
        self.importMode = importMode
        self.normalizationVersion = normalizationVersion
        self.babies = babies
        self.records = records
        self.totalRecordCount = totalRecordCount
        self.chunkIndex = chunkIndex
        self.finalize = finalize
    }

    public let protocolVersion: String
    public let capabilities: [String]
    public let importID: UUID
    public let importMode: String
    public let normalizationVersion: String
    public let babies: [ImportBaby]
    public let records: [RecordManifest]
    public let totalRecordCount: Int64
    public let chunkIndex: Int64
    public let finalize: Bool

    enum CodingKeys: String, CodingKey {
        case protocolVersion = "protocol_version", capabilities, importID = "import_id", importMode = "import_mode"
        case normalizationVersion = "normalization_version", babies, records
        case totalRecordCount = "total_record_count", chunkIndex = "chunk_index", finalize
    }
}

public struct ImportBaby: Codable, Sendable, Equatable {
    public let localBabyID: UUID
    public let name: String
    public let birthDate: String
    public let sortOrder: Int64
    public init(localBabyID: UUID, name: String, birthDate: String, sortOrder: Int64) {
        self.localBabyID = localBabyID
        self.name = name
        self.birthDate = birthDate
        self.sortOrder = sortOrder
    }

    enum CodingKeys: String, CodingKey { case localBabyID = "local_baby_id", name, birthDate = "birth_date", sortOrder = "sort_order" }
}

public struct RecordManifest: Codable, Sendable, Equatable {
    public let recordID: UUID
    public let entityKind: String
    public let localBabyID: UUID
    public let contentSHA256: String
    public init(recordID: UUID, entityKind: String, localBabyID: UUID, contentSHA256: String) {
        self.recordID = recordID
        self.entityKind = entityKind
        self.localBabyID = localBabyID
        self.contentSHA256 = contentSHA256
    }

    enum CodingKeys: String, CodingKey { case recordID = "record_id", entityKind = "entity_kind", localBabyID = "local_baby_id", contentSHA256 = "content_sha256" }
}

public struct ImportResolveResponse: Codable, Sendable, Equatable {
    public init(
        importID: UUID, resolverRevision: Int64, normalizationVersion: String,
        mappings: [BabyMapping], conflicts: [ImportConflict], conflictCount: Int64,
        conflictsComplete: Bool, nextConflictPageToken: String?, mappingComplete: Bool,
        nextChunkIndex: Int64, manifestComplete: Bool
    ) {
        self.importID = importID
        self.resolverRevision = resolverRevision
        self.normalizationVersion = normalizationVersion
        self.mappings = mappings
        self.conflicts = conflicts
        self.conflictCount = conflictCount
        self.conflictsComplete = conflictsComplete
        self.nextConflictPageToken = nextConflictPageToken
        self.mappingComplete = mappingComplete
        self.nextChunkIndex = nextChunkIndex
        self.manifestComplete = manifestComplete
    }

    public let importID: UUID
    public let resolverRevision: Int64
    public let normalizationVersion: String
    public let mappings: [BabyMapping]
    public let conflicts: [ImportConflict]
    public let conflictCount: Int64
    public let conflictsComplete: Bool
    public let nextConflictPageToken: String?
    public let mappingComplete: Bool
    public let nextChunkIndex: Int64
    public let manifestComplete: Bool

    enum CodingKeys: String, CodingKey {
        case importID = "import_id", resolverRevision = "resolver_revision", normalizationVersion = "normalization_version"
        case mappings, conflicts, conflictCount = "conflict_count", conflictsComplete = "conflicts_complete"
        case nextConflictPageToken = "next_conflict_page_token", mappingComplete = "mapping_complete"
        case nextChunkIndex = "next_chunk_index", manifestComplete = "manifest_complete"
    }
}

public struct BabyMapping: Codable, Sendable, Equatable {
    public let localBabyID: UUID
    public let sharedBabyID: UUID
    public init(localBabyID: UUID, sharedBabyID: UUID) {
        self.localBabyID = localBabyID
        self.sharedBabyID = sharedBabyID
    }

    enum CodingKeys: String, CodingKey { case localBabyID = "local_baby_id", sharedBabyID = "shared_baby_id" }
}

public struct ImportConflict: Codable, Sendable, Equatable {
    public let conflictID: UUID
    public let kind: String
    public let localBabyID: UUID
    public let recordID: UUID?
    public let candidateBabyIDs: [UUID]
    public let candidateVersions: [Int64]
    public init(
        conflictID: UUID, kind: String, localBabyID: UUID, recordID: UUID?,
        candidateBabyIDs: [UUID], candidateVersions: [Int64]
    ) {
        self.conflictID = conflictID
        self.kind = kind
        self.localBabyID = localBabyID
        self.recordID = recordID
        self.candidateBabyIDs = candidateBabyIDs
        self.candidateVersions = candidateVersions
    }

    enum CodingKeys: String, CodingKey {
        case conflictID = "conflict_id", kind, localBabyID = "local_baby_id", recordID = "record_id"
        case candidateBabyIDs = "candidate_baby_ids", candidateVersions = "candidate_versions"
    }
}
