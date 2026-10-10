import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// Counts every read, so a test can show nothing touched defaults at all.
private final class RecordingDefaults: UserDefaults {
    private(set) var reads = 0
    override func object(forKey defaultName: String) -> Any? { reads += 1; return super.object(forKey: defaultName) }
    override func string(forKey defaultName: String) -> String? { reads += 1; return super.string(forKey: defaultName) }
    override func integer(forKey defaultName: String) -> Int { reads += 1; return super.integer(forKey: defaultName) }
    override func bool(forKey defaultName: String) -> Bool { reads += 1; return super.bool(forKey: defaultName) }
}

/// What the app's environment is to the gate: defaults read when it is built,
/// and a wake handler a push pulls through.
@MainActor
private final class Environment {
    let settings: SettingsViewModel
    let wake: BackgroundWakeHandler

    init(settings: SettingsViewModel, wake: BackgroundWakeHandler) {
        self.settings = settings
        self.wake = wake
    }
}

/// One device: the sentinel's directory outlives each process; the lock
/// state, the builds, the defaults reads and the pulls are counted across them.
@MainActor
private final class Device {
    let directory: URL
    let sentinel: FirstUnlockSentinel
    let defaults: RecordingDefaults
    /// `UIApplication.isProtectedDataAvailable`: unlocked right now.
    var unlocked = false
    var builds = 0
    var pulls = 0

    init() throws {
        directory = FileManager.default.temporaryDirectory
            .appending(path: "protected-launch-\(UUID().uuidString)", directoryHint: .isDirectory)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        sentinel = FirstUnlockSentinel(url: directory.appending(path: "BabyTracker.first-unlock"))
        defaults = try #require(RecordingDefaults(suiteName: "protected-launch-\(UUID().uuidString)"))
    }

    /// A new process: the app delegate's gate over this device.
    func launch() -> ProtectedDataLaunch<Environment> {
        ProtectedDataLaunch(isProtectedDataAvailable: { [unowned self] in unlocked }, sentinel: sentinel) { [unowned self] in
            builds += 1
            return Environment(
                settings: SettingsViewModel(defaults: defaults),
                wake: BackgroundWakeHandler(sync: { [unowned self] in pulls += 1; return true })
            )
        }
    }

    /// `AppDelegate.didReceiveRemoteNotification`: gate, then one pull.
    func push(_ launch: ProtectedDataLaunch<Environment>) async -> BackgroundWakeResult {
        guard let environment = launch.valueIfUnlockedSinceBoot() else { return .noData }
        return await environment.wake.receiveSilentPush()
    }

    /// The device restarted: the sentinel is still on disk, but its contents
    /// cannot be read until the first unlock. Owner-unreadable stands in for
    /// the class key the kernel does not have yet.
    func restart() throws {
        unlocked = false
        try FileManager.default.setAttributes([.posixPermissions: 0o000], ofItemAtPath: sentinel.url.path)
    }

    /// The first unlock: the class key is back, and the contents with it.
    func unlock() {
        unlocked = true
        try? FileManager.default.setAttributes([.posixPermissions: 0o644], ofItemAtPath: sentinel.url.path)
    }

    func remove() {
        try? FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: sentinel.url.path)
        try? FileManager.default.removeItem(at: directory)
    }
}

/// The app's environment is built once the device has been unlocked since it
/// started, locked again or not, and never before.
@MainActor
@Suite("Protected data launch")
struct ProtectedDataLaunchTests {
    /// The common case: a phone unlocked since it started, now locked in a
    /// pocket. A push relaunches the terminated app; it builds and pulls once,
    /// though `isProtectedDataAvailable` says no.
    @Test func aPushToALockedPhoneUnlockedSinceItStartedBuildsAndPulls() async throws {
        let device = try Device()
        defer { device.remove() }
        device.unlock()
        _ = try #require(device.launch().valueIfUnlockedSinceBoot()) // used while unlocked
        #expect(device.builds == 1)

        device.unlocked = false // locked; the app is terminated
        let relaunched = device.launch()
        #expect(relaunched.valueIfUnlockedSinceBoot() != nil) // didFinishLaunching
        #expect(await device.push(relaunched) == .newData)
        #expect(device.builds == 2) // one per process
        #expect(device.pulls == 1)
        #expect(relaunched.valueNow() === relaunched.value) // the first scene adopts it
        #expect(device.builds == 2)
    }

    /// The protected-data callback marks the device too: a process that only
    /// ever saw it unlocked through the callback lets the next locked launch in.
    @Test func theCallbackLeavesTheMarkALaterLockedLaunchReads() async throws {
        let device = try Device()
        defer { device.remove() }
        let first = device.launch()
        #expect(await device.push(first) == .noData) // never marked: shut
        _ = first.valueNow() // protected data became available
        #expect(device.builds == 1)

        let relaunched = device.launch() // locked again, terminated, pushed
        #expect(await device.push(relaunched) == .newData)
        #expect(device.builds == 2)
        #expect(device.pulls == 1)
    }

    /// Before the first unlock after a restart, a launch and a push build
    /// nothing and read no defaults, whether the sentinel was never written or
    /// was written before the restart and cannot be read now. The push
    /// answers `.noData` without a pull. Once unlocked, the next ask builds
    /// the one value.
    @Test(arguments: [false, true])
    func nothingIsBuiltOrReadBeforeTheFirstUnlock(markedBeforeTheRestart: Bool) async throws {
        let device = try Device()
        defer { device.remove() }
        if markedBeforeTheRestart {
            #expect(device.sentinel.mark())
            try device.restart()
            // Its existence is not the signal: only its contents are protected.
            #expect(FileManager.default.fileExists(atPath: device.sentinel.url.path))
        }

        let launch = device.launch()
        #expect(launch.valueIfUnlockedSinceBoot() == nil) // didFinishLaunching
        #expect(await device.push(launch) == .noData)
        #expect(device.builds == 0)
        #expect(device.defaults.reads == 0)
        #expect(device.pulls == 0)

        device.unlock()
        let built = try #require(launch.valueIfUnlockedSinceBoot())
        #expect(device.builds == 1)
        #expect(device.defaults.reads > 0)
        #expect(launch.valueIfUnlockedSinceBoot() === built)
        #expect(launch.valueNow() === built)
        #expect(device.builds == 1)
        #expect(device.sentinel.isReadable()) // marked for the next locked launch
    }

    /// The protected-data callback or the first scene builds it, whichever
    /// comes first, exactly once. Built, it is handed out from then on.
    @Test func theCallbackOrASceneBuildsExactlyOnce() throws {
        let device = try Device()
        defer { device.remove() }
        let launch = device.launch()

        let first = launch.valueNow() // protected data became available
        let scene = launch.valueNow() // then the first scene connects
        #expect(first === scene)
        #expect(launch.valueIfUnlockedSinceBoot() === first) // a later push
        #expect(launch.value === first)
        #expect(device.builds == 1)
    }

    /// The sentinel's class is the store's and the defaults': readable from
    /// the first unlock on, locked or not. "Complete" would shut the gate
    /// whenever the phone is locked.
    @Test func theSentinelIsProtectedUntilTheFirstUnlockOnly() {
        #expect(FirstUnlockSentinel.protection == .completeFileProtectionUntilFirstUserAuthentication)
        #expect(FirstUnlockSentinel.app.url.lastPathComponent == "BabyTracker.first-unlock")
    }
}
