import Foundation
import SwiftData
import BabyCore

/// Separate from Baby so legacy records never acquire a guessed growth reference.
@Model
public final class GrowthProfile {
    public var id: UUID = UUID()
    public var realmKey: String = RealmID.local.encodedKey
    public var babyID: UUID = UUID()
    public var referenceSexRaw: String = ""
    public var expectedDueDateISO: String?
    public var lastModifiedAt: Date = Date()
    public var isTombstoned: Bool = false

    public init(babyID: UUID, referenceSex: GrowthReferenceSex, expectedDueDate: LocalDate?,
                modifiedAt: Date, realm: RealmID = .local) {
        self.id = GrowthProfileID.forBaby(babyID)
        self.babyID = babyID
        self.realmKey = realm.encodedKey
        self.referenceSexRaw = referenceSex.rawValue
        self.expectedDueDateISO = expectedDueDate?.iso8601
        self.lastModifiedAt = modifiedAt
    }

    public var realmID: RealmID { RealmID(encodedKey: realmKey) ?? .local }
    public var referenceSex: GrowthReferenceSex? { GrowthReferenceSex(rawValue: referenceSexRaw) }
    public var expectedDueDate: LocalDate? { expectedDueDateISO.flatMap { try? LocalDate($0) } }
}
