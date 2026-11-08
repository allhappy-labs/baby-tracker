import Foundation

/// Names one logged entry by its kind and id, so a screen can say which
/// entry it means without holding the SwiftData object, which the other
/// phone may change or delete while that screen is open.
public enum EntryRef: Hashable, Sendable {
    case feed(UUID), sleep(UUID), diaper(UUID), supplement(UUID), measurement(UUID), immunization(UUID), pump(UUID)

    public var id: UUID {
        switch self {
        case let .feed(id), let .sleep(id), let .diaper(id), let .supplement(id), let .measurement(id), let .immunization(id), let .pump(id): id
        }
    }
}
