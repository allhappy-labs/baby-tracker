import SwiftData

/// V9 is the baseline every install started from; V10 adds the baby icon,
/// V11 immunizations, V12 pumping.
public enum BabyStoreMigrationPlan: SchemaMigrationPlan {
    public static var schemas: [any VersionedSchema.Type] { [SchemaV9.self, SchemaV10.self, SchemaV11.self, SchemaV12.self] }
    public static var stages: [MigrationStage] {
        [.lightweight(fromVersion: SchemaV9.self, toVersion: SchemaV10.self),
         .lightweight(fromVersion: SchemaV10.self, toVersion: SchemaV11.self),
         .lightweight(fromVersion: SchemaV11.self, toVersion: SchemaV12.self)]
    }
}
