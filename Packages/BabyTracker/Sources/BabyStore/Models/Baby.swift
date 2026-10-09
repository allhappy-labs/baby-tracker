import Foundation
import SwiftData
import BabyCore

@Model
public final class Baby {
    public var id: UUID = UUID()
    public var realmKey: String = RealmID.local.encodedKey
    public var name: String = ""
    public var birthDate: Date = Date()
    public var birthDateISO: String?
    public var photoData: Data?
    /// The switcher icon (`BabyAvatar` id, e.g. "girl.2"), synced as `icon`.
    /// Nil until assigned.
    public var iconID: String?
    public var sortOrder: Int = 0
    public var lastModifiedAt: Date = Date()
    public var isTombstoned: Bool = false

    public init(id: UUID = UUID(), name: String, birthDate: Date, sortOrder: Int = 0, realm: RealmID = .local) {
        self.id = id
        self.realmKey = realm.encodedKey
        self.name = name
        self.birthDate = birthDate
        self.birthDateISO = try? LocalDate(displaying: birthDate, in: .current).iso8601
        self.sortOrder = sortOrder
        self.lastModifiedAt = Date()
        self.isTombstoned = false
    }

    public var realmID: RealmID { RealmID(encodedKey: realmKey) ?? .local }
    public var localBirthDate: LocalDate? { birthDateISO.flatMap { try? LocalDate($0) } }
}
