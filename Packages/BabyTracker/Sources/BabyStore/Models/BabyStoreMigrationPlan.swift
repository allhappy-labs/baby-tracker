import SwiftData

/// V9 is the baseline every install started from; V10 adds the baby icon.
public enum BabyStoreMigrationPlan: SchemaMigrationPlan {
    public static var schemas: [any VersionedSchema.Type] { [SchemaV9.self, SchemaV10.self] }
    public static var stages: [MigrationStage] {
        [.lightweight(fromVersion: SchemaV9.self, toVersion: SchemaV10.self)]
    }
}
