import Foundation
import SwiftData
import BabyCore

@Model
public final class SleepEntry {
    public var id: UUID = UUID()
    public var realmKey: String = RealmID.local.encodedKey
    public var babyID: UUID = UUID()
    public var startedAt: Date = Date()
    public var endedAt: Date?
    public var timeZoneID: String = "UTC"
    public var note: String?
    public var createdBy: String = ""
    public var lastModifiedAt: Date = Date()
    public var isTombstoned: Bool = false

    /// Set only when the user overrides the derived classification.
    public var kindOverrideRaw: String?

    /// The sleep session this part continues; nil means it starts its own.
    /// Synced as `sleep_session_id`.
    public var sleepSessionID: UUID?

    public init(
        id: UUID = UUID(),
        babyID: UUID,
        startedAt: Date,
        timeZoneID: String,
        createdBy: String = "",
        realm: RealmID = .local
    ) {
        self.id = id
        self.realmKey = realm.encodedKey
        self.babyID = babyID
        self.startedAt = startedAt
        self.timeZoneID = timeZoneID
        self.createdBy = createdBy
        self.lastModifiedAt = startedAt
        self.isTombstoned = false
    }

    public var realmID: RealmID { RealmID(encodedKey: realmKey) ?? .local }

    public var kindOverride: SleepKind? {
        get { kindOverrideRaw.flatMap(SleepKind.init(rawValue:)) }
        set { kindOverrideRaw = newValue?.rawValue }
    }

    /// The override wins; otherwise derive from the local start hour.
    public var kind: SleepKind {
        kindOverride ?? SleepClassifier.classify(startedAt: startedAt, timeZoneID: timeZoneID)
    }

    public var durationSeconds: Int? {
        endedAt.map { Int($0.timeIntervalSince(startedAt)) }
    }

    public var effectiveSleepSessionID: UUID { sleepSessionID ?? id }
}
