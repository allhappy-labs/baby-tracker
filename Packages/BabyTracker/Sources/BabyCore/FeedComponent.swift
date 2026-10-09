import Foundation

public enum FeedComponent: Codable, Sendable, Equatable, Identifiable {
    case breast(id: UUID, side: BreastSide, seconds: Int, startedAt: Date? = nil, endedAt: Date? = nil)
    case bottle(id: UUID, volumeML: Double, contents: BottleContents, seconds: Int? = nil, startedAt: Date? = nil, endedAt: Date? = nil)
    case solids(id: UUID, foods: [String], amount: SolidsAmount)

    public var id: UUID {
        switch self {
        case let .breast(id, _, _, _, _), let .bottle(id, _, _, _, _, _), let .solids(id, _, _): id
        }
    }

    public var breastSide: BreastSide? {
        guard case let .breast(_, side, _, _, _) = self else { return nil }
        return side
    }

    public var durationSeconds: Int? {
        guard case let .breast(_, _, seconds, _, _) = self else { return nil }
        return seconds
    }

    public var bottleSeconds: Int? {
        guard case let .bottle(_, _, _, seconds, _, _) = self else { return nil }
        return seconds
    }

    public var volumeML: Double? {
        guard case let .bottle(_, volumeML, _, _, _, _) = self else { return nil }
        return volumeML
    }

    public var bottleContents: BottleContents? {
        guard case let .bottle(_, _, contents, _, _, _) = self else { return nil }
        return contents
    }

    public var startedAt: Date? {
        switch self {
        case let .breast(_, _, _, start, _), let .bottle(_, _, _, _, start, _): start
        case .solids: nil
        }
    }

    public var endedAt: Date? {
        switch self {
        case let .breast(_, _, _, _, end), let .bottle(_, _, _, _, _, end): end
        case .solids: nil
        }
    }

    public var solidFoods: [String]? {
        guard case let .solids(_, foods, _) = self else { return nil }
        return foods
    }

    public var solidsAmount: SolidsAmount? {
        guard case let .solids(_, _, amount) = self else { return nil }
        return amount
    }
}
