import SwiftData

/// V9 plus `Baby.iconID`, the synced switcher icon. Every model here is the
/// live type; a later schema that changes one must freeze it in this file.
/// V11 added a model and changed none, so nothing is frozen here.
public enum SchemaV10: VersionedSchema {
    public static let versionIdentifier = Schema.Version(10, 0, 0)
    public static var models: [any PersistentModel.Type] {
        [
            Baby.self, FeedEntry.self, SleepEntry.self, DiaperEntry.self, SupplementEntry.self,
            ActiveTimer.self, ChangeLogEntry.self, SyncOutboxEntry.self, CanonicalEntityBase.self,
            SyncCursorState.self, ImportCheckpoint.self, RealmTransitionCheckpoint.self,
            CaregiverAttribution.self, FencedLease.self, AccountAccessState.self, RealmWritePermission.self,
            GrowthProfile.self, MeasurementCheckIn.self,
        ]
    }
}
