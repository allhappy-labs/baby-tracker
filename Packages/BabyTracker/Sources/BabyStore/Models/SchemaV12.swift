import SwiftData

/// V11 plus `PumpEntry`. Every model here is the live type; a later schema
/// that changes one must freeze it in this file.
public enum SchemaV12: VersionedSchema {
    public static let versionIdentifier = Schema.Version(12, 0, 0)
    public static var models: [any PersistentModel.Type] { SchemaV11.models + [PumpEntry.self] }
}
