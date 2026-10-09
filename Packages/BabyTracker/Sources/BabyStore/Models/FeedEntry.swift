import Foundation
import SwiftData
import BabyCore

@Model
public final class FeedEntry {
    public var id: UUID = UUID()
    public var realmKey: String = RealmID.local.encodedKey
    public var babyID: UUID = UUID()
    public var startedAt: Date = Date()
    public var endedAt: Date?
    public var feedingSessionID: UUID?
    public var timeZoneID: String = "UTC"
    public var note: String?
    public var createdBy: String = ""
    public var lastModifiedAt: Date = Date()
    /// Tombstone. MUST NOT be named `isDeleted` — that name is reserved by
    /// NSManagedObject and SwiftData silently resets it to false on save().
    public var isTombstoned: Bool = false

    /// How the feed started. Drives icon and default edit sheet only — never
    /// which fields are populated. Aggregates must read the fields themselves.
    public var kindRaw: String = FeedKind.breast.rawValue
    public var components: [FeedComponent] = []

    public init(
        id: UUID = UUID(),
        babyID: UUID,
        kind: FeedKind,
        startedAt: Date,
        timeZoneID: String,
        createdBy: String = "",
        realm: RealmID = .local
    ) {
        self.id = id
        self.realmKey = realm.encodedKey
        self.babyID = babyID
        self.kindRaw = kind.rawValue
        self.startedAt = startedAt
        self.timeZoneID = timeZoneID
        self.createdBy = createdBy
        self.lastModifiedAt = startedAt
        self.isTombstoned = false
    }

    public var realmID: RealmID { RealmID(encodedKey: realmKey) ?? .local }

    public var effectiveFeedingSessionID: UUID { feedingSessionID ?? id }

    public var kind: FeedKind {
        get { FeedKind(rawValue: kindRaw) ?? .breast }
        set { kindRaw = newValue.rawValue }
    }

    /// A combination feed has both breast intervals and a bottle volume.
    public var isCombo: Bool {
        components.contains { $0.breastSide != nil } && components.contains { $0.bottleContents != nil }
    }

    public var totalBreastSeconds: Int {
        components.compactMap(\.durationSeconds).reduce(0, +)
    }

    public var totalBottleVolumeML: Double {
        components.compactMap(\.volumeML).reduce(0, +)
    }

    public var bottleComponents: [FeedComponent] {
        components.filter { $0.bottleContents != nil }
    }

    public var lastSideUsed: BreastSide? {
        components.reversed().compactMap(\.breastSide).first
    }
}
