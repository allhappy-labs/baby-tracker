import AppIntents
import WidgetKit
import BabyCore
import BabyStore
import BabyFeature

enum SiriDiaperKind: String, AppEnum {
    case wet, dirty, mixed

    static let typeDisplayRepresentation: TypeDisplayRepresentation = "Diaper type"
    static let caseDisplayRepresentations: [Self: DisplayRepresentation] = [
        .wet: "Wet", .dirty: "Dirty", .mixed: "Mixed"
    ]

    var contents: DiaperContents {
        switch self {
        case .wet: .wet
        case .dirty: .dirty
        case .mixed: .mixed
        }
    }
}

enum SiriBreastSide: String, AppEnum {
    case left, right

    static let typeDisplayRepresentation: TypeDisplayRepresentation = "Breast side"
    static let caseDisplayRepresentations: [Self: DisplayRepresentation] = [
        .left: "Left", .right: "Right"
    ]

    var side: BreastSide {
        switch self {
        case .left: .left
        case .right: .right
        }
    }
}

struct SiriLogDiaperIntent: AppIntent {
    static let title: LocalizedStringResource = "Log diaper"
    static let description = IntentDescription("Logs a wet, dirty, or mixed diaper for the selected baby.")

    @Parameter(title: "Diaper type") var kind: SiriDiaperKind?

    static var parameterSummary: some ParameterSummary {
        Summary("Log \(\.$kind) diaper")
    }

    init() { kind = nil }
    init(kind: SiriDiaperKind) { self.kind = kind }

    @MainActor
    func perform() async throws -> some IntentResult & ProvidesDialog {
        guard let kind else {
            return .result(dialog: "Choose wet, dirty, or mixed before logging a diaper.")
        }
        do {
            let store = try WidgetStore.make()
            guard let baby = try WidgetStore.currentBaby(in: store) else {
                return .result(dialog: "No baby is selected. Nothing was logged.")
            }
            _ = try store.logDiaper(kind.contents, for: baby.id)
            WidgetCenter.shared.reloadAllTimelines()
            return .result(dialog: IntentDialog("Logged a \(kind.contents.displayName.lowercased()) diaper for \(baby.name)."))
        } catch WritePermissionError.readOnlyCaregiver {
            return .result(dialog: "This caregiver cannot log diapers. Nothing was changed.")
        } catch {
            return .result(dialog: "Could not log the diaper. Nothing was changed.")
        }
    }
}

struct SiriStartBreastIntent: AppIntent {
    static let title: LocalizedStringResource = "Start breast feeding"
    static let description = IntentDescription("Starts a breast-feeding timer on the selected side.")

    @Parameter(title: "Breast side") var side: SiriBreastSide?

    static var parameterSummary: some ParameterSummary {
        Summary("Start \(\.$side) breast feeding")
    }

    init() { side = nil }
    init(side: SiriBreastSide) { self.side = side }

    @MainActor
    func perform() async throws -> some IntentResult & ProvidesDialog {
        guard let side else {
            return .result(dialog: "Choose left or right breast before starting feeding.")
        }
        do {
            let store = try WidgetStore.make()
            guard let baby = try WidgetStore.currentBaby(in: store) else {
                return .result(dialog: "No baby is selected. Feeding was not started.")
            }
            // Starting the other side now stops whatever part is running and
            // joins it, the same as the Quick Log widget's L/R. Only the same
            // side already running is refused.
            if let running = try store.activeTimer(kind: .feed, for: baby.id),
               running.currentSide == side.side, running.currentBottleStartedAt == nil {
                return .result(dialog: IntentDialog("Feeding for \(baby.name) is already running on the \(side.side.displayName.lowercased()) breast."))
            }
            _ = try await OutOfAppFeedActions.startBreast(side.side, for: baby.id, in: store)
            WidgetCenter.shared.reloadAllTimelines()
            return .result(dialog: IntentDialog("Started \(side.side.displayName.lowercased()) breast feeding for \(baby.name)."))
        } catch WritePermissionError.readOnlyCaregiver {
            return .result(dialog: "This caregiver cannot start feeding. Nothing was changed.")
        } catch {
            return .result(dialog: "Could not start feeding. Nothing was changed.")
        }
    }
}

struct SiriStopFeedIntent: AppIntent {
    static let title: LocalizedStringResource = "Stop feeding"
    static let description = IntentDescription("Ends and saves the selected baby's running feed.")

    @MainActor
    func perform() async throws -> some IntentResult & ProvidesDialog {
        do {
            let store = try WidgetStore.make()
            guard let baby = try WidgetStore.currentBaby(in: store) else {
                return .result(dialog: "No baby is selected. No feeding was saved.")
            }
            guard let timer = try store.activeTimer(kind: .feed, for: baby.id) else {
                return .result(dialog: IntentDialog("No feeding is running for \(baby.name). Nothing was saved."))
            }
            // Same path as the Live Activity's Stop and the Quick Log
            // widget: a running timed bottle is saved at the usual amount
            // and the app offers "Set amount" for it next time it is open.
            try await OutOfAppFeedActions.stopFeed(timer, in: store)
            WidgetCenter.shared.reloadAllTimelines()
            return .result(dialog: IntentDialog("Saved the feeding for \(baby.name)."))
        } catch WritePermissionError.readOnlyCaregiver {
            return .result(dialog: "This caregiver cannot stop feeding. Nothing was changed.")
        } catch {
            return .result(dialog: "Could not save the feeding. The timer is still running.")
        }
    }
}

struct SiriStartSleepIntent: AppIntent {
    static let title: LocalizedStringResource = "Start sleep"
    static let description = IntentDescription("Starts a sleep timer for the selected baby.")

    @MainActor
    func perform() async throws -> some IntentResult & ProvidesDialog {
        do {
            let store = try WidgetStore.make()
            guard let baby = try WidgetStore.currentBaby(in: store) else {
                return .result(dialog: "No baby is selected. Sleep was not started.")
            }
            if try store.activeTimer(kind: .sleep, for: baby.id) != nil {
                return .result(dialog: IntentDialog("Sleep for \(baby.name) is already running."))
            }
            let timer = try store.startSleep(for: baby.id)
            await LiveActivityController.adoptOrStart(for: timer, in: store)
            WidgetCenter.shared.reloadAllTimelines()
            return .result(dialog: IntentDialog("Started sleep for \(baby.name)."))
        } catch WritePermissionError.readOnlyCaregiver {
            return .result(dialog: "This caregiver cannot start sleep. Nothing was changed.")
        } catch {
            return .result(dialog: "Could not start sleep. Nothing was changed.")
        }
    }
}

struct SiriStopSleepIntent: AppIntent {
    static let title: LocalizedStringResource = "Wake up"
    static let description = IntentDescription("Ends and saves the selected baby's running sleep.")

    @MainActor
    func perform() async throws -> some IntentResult & ProvidesDialog {
        do {
            let store = try WidgetStore.make()
            guard let baby = try WidgetStore.currentBaby(in: store) else {
                return .result(dialog: "No baby is selected. No sleep was saved.")
            }
            guard let timer = try store.activeTimer(kind: .sleep, for: baby.id) else {
                return .result(dialog: IntentDialog("No sleep is running for \(baby.name). Nothing was saved."))
            }
            try await LiveActivityController.stopping(timer, in: store) { _ = try store.stopSleep(timer) }
            WidgetCenter.shared.reloadAllTimelines()
            return .result(dialog: "Woke up.")
        } catch WritePermissionError.readOnlyCaregiver {
            return .result(dialog: "This caregiver cannot stop sleep. Nothing was changed.")
        } catch {
            return .result(dialog: "Could not save the sleep. The timer is still running.")
        }
    }
}
