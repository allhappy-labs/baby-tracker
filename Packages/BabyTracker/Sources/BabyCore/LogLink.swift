import Foundation

/// `babytracker://log?kind=solids&baby=<id>`: a Quick Log widget tile whose
/// action needs the app. The widget builds it; the app opens that baby's
/// panel.
public struct LogLink: Equatable, Sendable {
    public let kind: HomeActionKind
    public let babyID: UUID

    public init(kind: HomeActionKind, babyID: UUID) {
        self.kind = kind
        self.babyID = babyID
    }

    /// Nil for anything this version cannot act on, including a kind a later
    /// version's widget names.
    public init?(url: URL) {
        guard Self.isLogLink(url),
              let items = URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems,
              let kind = items.first(where: { $0.name == "kind" })?.value.flatMap(HomeActionKind.init(rawValue:)),
              let babyID = items.first(where: { $0.name == "baby" })?.value.flatMap(UUID.init(uuidString:))
        else { return nil }
        self.init(kind: kind, babyID: babyID)
    }

    public var url: URL {
        var components = URLComponents()
        components.scheme = "babytracker"
        components.host = "log"
        components.queryItems = [URLQueryItem(name: "kind", value: kind.rawValue),
                                 URLQueryItem(name: "baby", value: babyID.uuidString)]
        // Built from fixed parts and a UUID: it always forms a URL.
        return components.url!
    }

    /// Whether `url` is addressed to this route at all, readable or not.
    public static func isLogLink(_ url: URL) -> Bool {
        url.scheme == "babytracker" && url.host == "log"
    }
}
