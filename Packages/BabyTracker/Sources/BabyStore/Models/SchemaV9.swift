import Foundation
import SwiftData
import BabyCore

/// The baseline schema every install started from: the format cleanup
/// replaced the on-device history (v1–v8 and their migration bridges) with
/// this one version, because both phones delete the app and fresh-install
/// it. A store written by a schema the migration plan does not list is never
/// migrated: the app moves it aside and opens a fresh one, which sync
/// repopulates; the widget and intents refuse it (`IncompatibleStore`).
///
/// No longer the only version: `SchemaV10` follows it with a lightweight
/// stage. `Baby` is therefore a frozen copy of the shape V9 stored, so the
/// two versions' checksums differ (Core Data refuses "Duplicate version
/// checksums detected"). Every other model is still the live type; a later
/// schema that changes one must freeze it here too.
public enum SchemaV9: VersionedSchema {
    public static let versionIdentifier = Schema.Version(9, 0, 0)
    public static var models: [any PersistentModel.Type] {
        [
            SchemaV9.Baby.self, FeedEntry.self, SleepEntry.self, DiaperEntry.self, SupplementEntry.self,
            ActiveTimer.self, ChangeLogEntry.self, SyncOutboxEntry.self, CanonicalEntityBase.self,
            SyncCursorState.self, ImportCheckpoint.self, RealmTransitionCheckpoint.self,
            CaregiverAttribution.self, FencedLease.self, AccountAccessState.self, RealmWritePermission.self,
            GrowthProfile.self, MeasurementCheckIn.self,
        ]
    }

    /// `Baby` as V9 stored it, before `iconID`. Frozen: never edit.
    @Model public final class Baby {
        public var id: UUID = UUID()
        public var realmKey: String = RealmID.local.encodedKey
        public var name: String = ""
        public var birthDate: Date = Date()
        public var birthDateISO: String?
        public var photoData: Data?
        public var sortOrder: Int = 0
        public var lastModifiedAt: Date = Date()
        public var isTombstoned: Bool = false

        public init(id: UUID, name: String, birthDate: Date) {
            self.id = id
            self.name = name
            self.birthDate = birthDate
        }
    }
}
