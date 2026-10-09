import AppIntents
import BabyStore
import BabyFeature

struct WidgetBaby: AppEntity {
    static let typeDisplayRepresentation = TypeDisplayRepresentation(name: "Baby")
    static let defaultQuery = WidgetBabyQuery()

    let id: String
    let name: String

    var displayRepresentation: DisplayRepresentation {
        DisplayRepresentation(title: "\(name)")
    }
}

struct WidgetBabyQuery: EntityQuery {
    @MainActor
    func entities(for identifiers: [String]) async throws -> [WidgetBaby] {
        let babies = try WidgetStore.make().babies()
        return babies.filter { identifiers.contains($0.id.uuidString) }
            .map { WidgetBaby(id: $0.id.uuidString, name: $0.name) }
    }

    @MainActor
    func suggestedEntities() async throws -> [WidgetBaby] {
        try WidgetStore.make().babies().map {
            WidgetBaby(id: $0.id.uuidString, name: $0.name)
        }
    }

    @MainActor
    func defaultResult() async -> WidgetBaby? {
        guard let store = try? WidgetStore.make(),
              let baby = try? WidgetStore.currentBaby(in: store) else { return nil }
        return WidgetBaby(id: baby.id.uuidString, name: baby.name)
    }
}

struct BabyWidgetConfiguration: WidgetConfigurationIntent {
    static let title: LocalizedStringResource = "Choose baby"
    static let description = IntentDescription("Choose the baby this widget follows.")

    @Parameter(title: "Baby")
    var baby: WidgetBaby?

    init() {}
}
