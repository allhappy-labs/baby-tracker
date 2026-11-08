import AppIntents
import Foundation
import WidgetKit
import BabyFeature
import BabyStore

/// Stop on the pump card. It ends the card's own session only: a card left
/// over from an earlier session never stops the one running now.
/// Like every Lock Screen control it is a `LiveActivityIntent` with no
/// `openAppWhenRun`, so it runs on a locked iPhone without bringing the app
/// forward.
struct StopPumpIntent: LiveActivityIntent {
    static let title: LocalizedStringResource = "Stop pumping"
    static let description = IntentDescription("Ends the running pumping session.")
    static let authenticationPolicy: IntentAuthenticationPolicy = .alwaysAllowed
    static let isDiscoverable = false

    @Parameter(title: "Session")
    var pumpID: String?

    init() {}

    init(pumpID: UUID) {
        self.pumpID = pumpID.uuidString
    }

    @MainActor
    func perform() async throws -> some IntentResult {
        let store = try WidgetStore.make()
        try await OutOfAppPumpActions.stop(pumpID: pumpID.flatMap(UUID.init(uuidString:)), in: store)
        WidgetCenter.shared.reloadAllTimelines()
        return .result()
    }
}
