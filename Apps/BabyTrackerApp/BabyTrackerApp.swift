import SwiftUI
import AppIntents
import BabyFeature
import SettingsFeature

@main
struct BabyTrackerApp: App {
    /// Owns the app's one `AppEnvironment`, built at process launch when
    /// protected data is available, and takes APNs registration and
    /// background pushes (see `AppDelegate`). A Live Activity started by a
    /// push launches the app in the background, maybe with no scene at all,
    /// and its update token still has to be registered: the store, the
    /// restored account and the push registration must not wait for
    /// `RootView` to exist.
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate

    init() {
        BabyTrackerShortcuts.updateAppShortcutParameters()
        // Before anything else: a Lock Screen tap can be why this process
        // was launched, and its intent runs here. Each card action checks
        // the family's current state through a graph of its own, bound to
        // the store the intent opens.
        LiveActivityCardActions.makeGate = { store in LiveActivityBackgroundComposition.make(store: store) }
        // The tap returns once it has written; the publish runs after it.
        LiveActivityCardActions.runInBackground = LiveActivityBackgroundComposition.runInBackground
    }

    var body: some Scene {
        WindowGroup {
            RootView(environment: appDelegate.environment)
        }
    }
}
