import SwiftData

/// V10 plus `ImmunizationEntry`. Every model here is the live type; a later
/// schema that changes one must freeze it in this file. V12 added a model and
/// changed none, so nothing is frozen here.
public enum SchemaV11: VersionedSchema {
    public static let versionIdentifier = Schema.Version(11, 0, 0)
    public static var models: [any PersistentModel.Type] { SchemaV10.models + [ImmunizationEntry.self] }
}
