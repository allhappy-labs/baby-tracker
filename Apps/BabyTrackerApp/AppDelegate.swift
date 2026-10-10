import BabySync
import os
import SettingsFeature
import UIKit

/// The app's only UIKit delegate. It owns the one `AppEnvironment`, and takes
/// APNs registration and background pushes.
///
/// The environment is built at process launch when it can be, not when a
/// scene connects. A background push can relaunch a terminated app without
/// connecting any scene, so `RootView` never exists then, yet the store and
/// the family graph must exist for the push to do its one pull. `RootView`
/// adopts this instance rather than building its own.
///
/// Never before the device's first unlock since it started
/// (`ProtectedDataLaunch`): a push that relaunches the app then would
/// otherwise build it from unreadable defaults and keep it. Then it is built
/// when protected data becomes available or the first scene connects,
/// whichever comes first, and a push before that answers `.noData`. A push to
/// a phone that has been unlocked since it started and is locked again builds
/// it and pulls: everything it reads stays readable after the first unlock.
///
/// The push carries no data; the app answers it with one ordinary sync pull.
/// The delegate implements nothing else (no `UNUserNotificationCenterDelegate`,
/// no ActivityKit), so Live Activities and the widget keep their own paths.
@MainActor
final class AppDelegate: NSObject, UIApplicationDelegate {
    private lazy var launch = ProtectedDataLaunch(
        isProtectedDataAvailable: { UIApplication.shared.isProtectedDataAvailable },
        sentinel: .app,
        make: { AppEnvironment() }
    )
    private let log = Logger(subsystem: "com.olhapi.babytracker", category: "push")
    /// One registration failure is worth a line; every later one says the same.
    private var loggedRegistrationFailure = false

    /// For the first scene. A connected scene implies an unlocked device, so
    /// this builds the environment if nothing has yet.
    var environment: AppEnvironment { launch.valueNow() }

    func application(
        _ application: UIApplication, didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil
    ) -> Bool {
        _ = launch.valueIfUnlockedSinceBoot()
        return true
    }

    func applicationProtectedDataDidBecomeAvailable(_ application: UIApplication) {
        _ = launch.valueNow()
    }

    func application(_ application: UIApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        // Registration is only ever asked for by a built environment.
        Task { await launch.value?.push?.deviceTokenReceived(deviceToken) }
    }

    /// Expected on every launch of a build without push provisioning (an
    /// unsigned or simulator build), so it is a debug line, once per launch.
    func application(_ application: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: any Error) {
        guard !loggedRegistrationFailure else { return }
        loggedRegistrationFailure = true
        log.debug("remote notification registration failed: \((error as NSError).code, privacy: .public)")
    }

    func application(_ application: UIApplication, didReceiveRemoteNotification userInfo: [AnyHashable: Any]) async -> UIBackgroundFetchResult {
        log.notice("background push received")
        guard let environment = launch.valueIfUnlockedSinceBoot() else {
            log.notice("background push answered: not unlocked since boot")
            return .noData
        }
        // A store that failed to open in an earlier background launch gets
        // another chance now that the device has been unlocked. A no-op when
        // it is open.
        environment.retry()
        // Nil only when the store still would not open: nothing to sync into.
        guard let wake = environment.wake else {
            log.notice("background push answered: no store")
            return .noData
        }
        let result: UIBackgroundFetchResult = switch await wake.receiveSilentPush() {
        case .newData: .newData
        case .noData: .noData
        case .failed: .failed
        }
        log.notice("background push answered: \(result.rawValue, privacy: .public)")
        return result
    }
}
