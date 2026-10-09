import AppIntents
import Darwin
import WidgetKit
import BabyCore
import BabyStore
import BabyFeature

/// The Lock Screen and Dynamic Island buttons of a card that belongs to a
/// family realm.
///
/// The card passes its own target (`LiveActivityActionTarget`) with every
/// button. Before anything is written, the target is checked against the
/// family's current state by the gate the app installs at launch
/// (`LiveActivityCardActions.makeGate`, the background composition): an
/// obsolete card — another caregiver stopped its timer and started the next,
/// the family changed, this phone became a viewer — never mutates what runs
/// now. When freshness cannot be established, nothing is written: the reason
/// is parked for the app, and the intent asks iOS to continue in the app
/// (`continueInForeground`), which once open says what to do. The tapped
/// action is never replayed.
///
/// A button without a target — the Home Screen widget, Siri, a card with no
/// realm — is another intent altogether (`LogIntents.swift`) and keeps its
/// own path; only the card intents (`LiveActivityCardIntents.swift`) come here.
@MainActor
enum LiveActivityActions {
    static func run(_ action: LiveActivityCardAction, babyID: String?, target parameter: String?) async throws -> LiveActivityActionOutcome {
        var timing = ActionTiming()
        AppLog.widgets.notice("card action: began \(timing.sinceLaunch, privacy: .public) ms after process launch")
        let pending = PendingQuickLogStore()
        // A card intent whose target is missing or does not read back is
        // never acted on as if it had none: that path acts on whatever runs.
        guard let target = LiveActivityActionTarget(parameterValue: parameter),
              let baby = babyID.flatMap(UUID.init(uuidString:)) else {
            pending.parkActivityHandoff(.unverifiable)
            return .handedOff(.unverifiable)
        }
        let store = try WidgetStore.make()
        let opened = timing.lap()
        guard let gate = LiveActivityCardActions.makeGate?(store) else {
            pending.parkActivityHandoff(.refreshFailed)
            return .handedOff(.refreshFailed)
        }
        let built = timing.lap()
        let outcome = try await LiveActivityCardActions.perform(action, target: target, babyID: baby, in: store, gate: gate, pending: pending)
        let performed = timing.lap()
        WidgetCenter.shared.reloadAllTimelines()
        AppLog.widgets.notice("""
            card action: \(String(describing: outcome), privacy: .public), pushed card \(target.isPushed, privacy: .public); \
            store \(opened) ms, gate \(built) ms, check and write \(performed) ms, total \(timing.total) ms
            """)
        return outcome
    }
}

/// Where a card action's time goes, for the log. Durations only.
private struct ActionTiming {
    private let start = ContinuousClock.now
    private var last = ContinuousClock.now

    /// Milliseconds since the previous lap.
    mutating func lap() -> Int64 {
        let now = ContinuousClock.now
        defer { last = now }
        return Self.milliseconds(now - last)
    }

    var total: Int64 { Self.milliseconds(ContinuousClock.now - start) }

    /// How long the process had been running when the tap reached it: a cold
    /// launch for the tap shows here, not in the laps.
    var sinceLaunch: Int64 {
        var info = kinfo_proc()
        var size = MemoryLayout<kinfo_proc>.stride
        var name: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_PID, getpid()]
        guard sysctl(&name, 4, &info, &size, nil, 0) == 0 else { return -1 }
        let started = info.kp_proc.p_un.__p_starttime
        let launched = Double(started.tv_sec) + Double(started.tv_usec) / 1_000_000
        return Int64((Date().timeIntervalSince1970 - launched) * 1_000)
    }

    private static func milliseconds(_ duration: Duration) -> Int64 {
        duration.components.seconds * 1_000 + duration.components.attoseconds / 1_000_000_000_000_000
    }
}

/// What the app says once a handed-off action has opened it.
enum LiveActivityHandoffCopy {
    static let message = String(localized: "Open Baby Tracker to refresh this activity before changing it.")
    static let ok = String(localized: "OK")
}

extension LiveActivityIntent {
    /// After a handoff: asks iOS to bring the app forward, where the parked
    /// reason is shown. Only the app opens — the action is not performed
    /// there either. Where the system cannot continue in the foreground, the
    /// reason waits for the next time the app is opened.
    @MainActor
    func continueInAppIfHandedOff(_ outcome: LiveActivityActionOutcome) async {
        guard case .handedOff = outcome, systemContext.currentMode.canContinueInForeground else { return }
        do {
            try await continueInForeground(
                IntentDialog("Open Baby Tracker to refresh this activity before changing it."),
                alwaysConfirm: false
            )
        } catch {
            // Declined or unavailable: the reason is still parked.
        }
    }
}
