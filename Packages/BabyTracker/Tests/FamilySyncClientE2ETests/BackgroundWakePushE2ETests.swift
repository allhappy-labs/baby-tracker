import BabyCore
import BabyProtocol
@testable import BabyStore
@testable import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// A backgrounded phone woken by an APNs push, through the real server.
///
/// `protocol-e2e.sh` starts the server with the push profile: the production
/// wake worker and APNs client, a throwaway signing key, and the identitytest
/// recorder in place of Apple (it never dials). iOS cannot deliver a
/// background push to a host test, and `simctl push` cannot reach this handler
/// on iOS 26.5, so the test hands the phone's composition what iOS would: the
/// device token (`graph.push`) and then the push itself (`graph.wake`).
@MainActor
@Suite("A backgrounded phone is woken by a push", .serialized)
struct BackgroundWakePushE2ETests {
    /// The caregiver's phone has registered an APNs token and holds no live
    /// connection. The owner starts a timer; the server's wake worker pushes
    /// the caregiver (recorded by the identitytest sink, never Apple); the push
    /// makes the caregiver pull, and the timer is on its phone.
    @Test func ownersWriteWakesTheBackgroundedCaregiver() async throws {
        guard let environment = E2EEnvironment.current(),
              ProcessInfo.processInfo.environment["BABY_TRACKER_E2E_APNS"] == "1" else { return }
        let race = try await Race(environment: environment, caregiverLiveHints: false) // no live connection: the hub cannot wake it
        defer { race.tearDown() }
        let (owner, caregiver, baby) = (race.owner, race.caregiver, race.babyID)
        let recorder = APNSRecorder(environment: environment)
        let token = Data((0..<32).map { UInt8($0) | 0x80 })
        let tokenHex = token.map { String(format: "%02x", $0) }.joined()

        // The join hands the registrar its session on a task of its own; once
        // it has, the token iOS delivers uploads at once. The production
        // registrar uploads through PUT /v1/devices/push-token.
        await caregiver.model.pushSettled()
        await caregiver.graph.push.deviceTokenReceived(token)
        // The registrar remembers what it uploaded only after the server took it.
        try #require(caregiver.defaults.string(forKey: PushTokenRegistrar.fingerprintKey) != nil,
                     "the registrar uploaded nothing: \(caregiver.traffic.pushTokenCalls)")
        // The session delegate hears of the finished task on its own queue,
        // possibly just after the upload's await has returned.
        let registered = ContinuousClock.now
        while caregiver.traffic.pushTokenCalls.isEmpty, ContinuousClock.now - registered < .seconds(5) {
            try await Task.sleep(for: .milliseconds(20))
        }
        try #require(caregiver.traffic.pushTokenCalls == [.init(method: "PUT", status: 204)],
                     "the token did not register through the route: \(caregiver.traffic.pushTokenCalls)")
        try await caregiver.sync() // the caregiver is at the high water: nothing to wake yet
        try #require(caregiver.traffic.liveDials.isEmpty, "the caregiver dialled the live socket: \(caregiver.traffic.liveDials)")
        // Past a whole successful wake-worker tick, by the worker's own count
        // rather than a sleep (which is vacuous when a loaded machine delays
        // or fails the tick): a token alone is not a reason to push.
        try await recorder.waitForWholeWakeTick(timeout: .seconds(20))
        try #require(try await recorder.records(token: tokenHex).isEmpty, "the caregiver was pushed before the owner wrote")

        let timer = try owner.store.startPart(.breast(.left), for: baby)
        try await owner.sync(.localWrite)

        let push = try await recorder.waitForPush(token: tokenHex, timeout: .seconds(20))
        try #require(push.path == "/3/device/\(tokenHex)")
        try #require(push.host == "sandbox.apns.identitytest.invalid", "pushed through \(push.host), not the sandbox the token registered for")
        try #require(push.headers["apns-push-type"] == "background")
        try #require(push.headers["apns-priority"] == "5")
        try #require(push.headers["apns-topic"] == "com.example.babytracker.protocol")
        try #require(push.providerTokenValid)
        try #require(push.acknowledged)
        try #require(push.payload == .object(["aps": .object(["content-available": .number(1)])]), "payload \(push.payload)")
        // Still backgrounded: nothing but the push can bring the write over.
        try #require(try caregiver.store.activeTimer(kind: .feed, for: baby) == nil, "the caregiver pulled before the push was answered")

        // What iOS does with the push: the app delegate's one pull.
        let pullsBefore = caregiver.traffic.syncRequests
        try #require(await caregiver.graph.wake.receiveSilentPush() == .newData)
        let pulls = caregiver.traffic.syncRequests - pullsBefore
        try #require(pulls == 1, "the push ran \(pulls) sync requests, not one pull")
        try #require(try caregiver.engine().lastSyncReason == .backgroundPush)
        let pulled = try #require(try caregiver.store.activeTimer(kind: .feed, for: baby))
        try #require(pulled.id == timer.id)

        // One push, never a second: the caregiver is up to date, and the
        // device is inside its 20-minute APNs window anyway. A whole
        // successful tick after the pull, by the worker's count.
        try await recorder.waitForWholeWakeTick(timeout: .seconds(20))
        let pushes = try await recorder.records(token: tokenHex)
        try #require(pushes.count == 1, "the caregiver was pushed \(pushes.count) times")
        try #require(caregiver.traffic.liveDials.isEmpty)
        // `token=` first: protocol-e2e.sh reads it to count this test's pushes
        // in the server's recorder. The token is synthetic.
        print("background-push proof: token=\(tokenHex) registered by PUT /v1/devices/push-token (204), no live connection, one push after the owner's write: host=\(push.host) push-type=background priority=5 topic=\(push.headers["apns-topic"] ?? "-") provider-token=valid payload=content-available-only; the wake handler answered newData after one pull (reason=backgroundPush) and the caregiver has the owner's timer \(timer.id)")
    }
}
