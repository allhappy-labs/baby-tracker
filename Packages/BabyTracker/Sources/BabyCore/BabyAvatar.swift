/// A baby's face in the switcher. The stored id is the raw value of one of
/// the eight set cases and travels in the baby's wire payload as `icon`.
/// `neutral` is drawn for a baby whose Boys/Girls is not known yet; it is
/// never stored.
public enum BabyAvatar: String, CaseIterable, Sendable {
    case boy1 = "boy.1", boy2 = "boy.2", boy3 = "boy.3", boy4 = "boy.4"
    case girl1 = "girl.1", girl2 = "girl.2", girl3 = "girl.3", girl4 = "girl.4"
    case neutral

    public init(iconID: String?) {
        guard let iconID, Self.storedIDs.contains(iconID), let avatar = BabyAvatar(rawValue: iconID) else {
            self = .neutral
            return
        }
        self = avatar
    }

    public static let storedIDs: [String] = allCases.filter { $0 != .neutral }.map(\.rawValue)

    public static func ids(for sex: GrowthReferenceSex) -> [String] {
        let prefix = sex == .boys ? "boy." : "girl."
        return storedIDs.filter { $0.hasPrefix(prefix) }
    }

    /// The lowest-numbered icon of the set no sibling uses; once all four are
    /// taken, the one the fewest siblings use, lowest number on a tie.
    /// `siblings` is the other living babies in the same realm.
    public static func assign(sex: GrowthReferenceSex, siblings: [String?]) -> String {
        let candidates = ids(for: sex)
        let used = siblings.compactMap { $0 }
        return candidates.min { a, b in
            let countA = used.filter { $0 == a }.count, countB = used.filter { $0 == b }.count
            return countA != countB ? countA < countB : candidates.firstIndex(of: a)! < candidates.firstIndex(of: b)!
        }!
    }
}
