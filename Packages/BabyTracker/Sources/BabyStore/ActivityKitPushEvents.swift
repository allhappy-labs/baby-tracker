import Foundation
import os
public import BabyProtocol
#if os(iOS)
import ActivityKit
#endif

/// Where a Live Activity is in its life, as the push registration needs it.
///
/// Platform-neutral so the registration lifecycle can be tested on the macOS
/// host the package suite runs on.
public enum LiveActivityObservedState: String, Sendable, Codable, Equatable {
    case pending, active, stale, ended, dismissed
    /// A state this build does not know (a future ActivityKit addition).
    /// Neither live nor finished: nothing is registered, ended or dismissed
    /// on its account.
    case unknown

    /// Still on screen and still able to take updates.
    public var isLive: Bool { self == .pending || self == .active || self == .stale }
}

/// Everything known about one card at one moment: its identity, the realm it
/// was started in, the server revision it last showed, its state, and its
/// update token once ActivityKit has made one.
///
/// A missing `pushToken` means "not issued yet", never "gone": token creation
/// is asynchronous, and a remotely started card is reported before its token.
public struct LiveActivityObservation: Sendable, Equatable {
    public var activityID: String
    public var babyID: UUID
    public var kindRaw: String
    public var scope: LiveActivityScope?
    public var remote: LiveActivityRevision?
    public var state: LiveActivityObservedState
    /// Lowercase hex. Secret: never logged, never written outside the Keychain.
    public var pushToken: String?

    public init(
        activityID: String, babyID: UUID, kindRaw: String, scope: LiveActivityScope?,
        remote: LiveActivityRevision? = nil, state: LiveActivityObservedState = .active, pushToken: String? = nil
    ) {
        self.activityID = activityID
        self.babyID = babyID
        self.kindRaw = kindRaw
        self.scope = scope
        self.remote = remote
        self.state = state
        self.pushToken = pushToken
    }
}

/// One thing ActivityKit told this process.
public enum LiveActivityPushEvent: Sendable, Equatable {
    /// The app-wide push-to-start token: its current value, then rotations.
    case pushToStartToken(String)
    /// A card was discovered (existing, started here, or started by a push),
    /// or its token, content or state changed. Always the whole latest view.
    case activity(LiveActivityObservation)
    /// The user switched Live Activities on or off for this app in Settings.
    case activitiesEnabled(Bool)
}

/// The ActivityKit seam. Production observes the real streams; the suites
/// drive the same lifecycle from a scripted source.
public protocol LiveActivityPushEventSource: Sendable {
    /// Push-to-start, discovery, update-token, content and state events for
    /// existing and new activities, for as long as the stream is iterated.
    func events() -> AsyncStream<LiveActivityPushEvent>
    /// `Activity.activities` right now. Only a foreground read of this list
    /// may attest that a card is absent.
    func inventory() -> [LiveActivityObservation]
    func activitiesEnabled() -> Bool
}

public enum LiveActivityPushEvents {
    /// The source for this platform: ActivityKit on an iPhone, inert elsewhere.
    public static var system: any LiveActivityPushEventSource {
        #if os(iOS)
        ActivityKitPushEvents()
        #else
        InertLiveActivityPushEvents()
        #endif
    }

    /// APNs device tokens are sent as lowercase hex.
    public static func hex(_ token: Data) -> String {
        token.map { String(format: "%02x", $0) }.joined()
    }
}

/// No ActivityKit: nothing is ever observed, and nothing is ever enabled.
public struct InertLiveActivityPushEvents: LiveActivityPushEventSource {
    public init() {}
    public func events() -> AsyncStream<LiveActivityPushEvent> { AsyncStream { $0.finish() } }
    public func inventory() -> [LiveActivityObservation] { [] }
    public func activitiesEnabled() -> Bool { false }
}

#if os(iOS)
/// The real source. `Activity` and its sequences are not `Sendable`, so each
/// activity is observed inside one task through an unchecked box and only
/// `Sendable` observations leave it. Nothing here logs a token.
public struct ActivityKitPushEvents: LiveActivityPushEventSource {
    public init() {}

    public func events() -> AsyncStream<LiveActivityPushEvent> {
        AsyncStream { continuation in
            let task = Task.detached {
                await withDiscardingTaskGroup { group in
                    group.addTask {
                        // Yields the current token first, then every rotation.
                        for await token in Activity<TimerActivityAttributes>.pushToStartTokenUpdates {
                            continuation.yield(.pushToStartToken(LiveActivityPushEvents.hex(token)))
                        }
                    }
                    group.addTask {
                        let authorization = ActivityAuthorizationInfo()
                        continuation.yield(.activitiesEnabled(authorization.areActivitiesEnabled))
                        for await enabled in authorization.activityEnablementUpdates {
                            continuation.yield(.activitiesEnabled(enabled))
                        }
                    }
                    group.addTask {
                        await Self.observeActivities(continuation)
                    }
                }
                continuation.finish()
            }
            continuation.onTermination = { _ in task.cancel() }
        }
    }

    public func inventory() -> [LiveActivityObservation] {
        Activity<TimerActivityAttributes>.activities.map { Self.observation(of: $0, token: $0.pushToken.map(LiveActivityPushEvents.hex)) }
    }

    public func activitiesEnabled() -> Bool {
        ActivityAuthorizationInfo().areActivitiesEnabled
    }

    private struct Box: @unchecked Sendable {
        let activity: Activity<TimerActivityAttributes>
    }

    /// Existing cards first, then every new one — a card the app started, or
    /// one a push-to-start created while the app was not running.
    private static func observeActivities(_ continuation: AsyncStream<LiveActivityPushEvent>.Continuation) async {
        await withDiscardingTaskGroup { observers in
            var seen = Set<String>()
            for activity in Activity<TimerActivityAttributes>.activities where seen.insert(activity.id).inserted {
                let box = Box(activity: activity)
                observers.addTask { await observe(box, continuation) }
            }
            for await activity in Activity<TimerActivityAttributes>.activityUpdates where seen.insert(activity.id).inserted {
                let box = Box(activity: activity)
                observers.addTask { await observe(box, continuation) }
            }
        }
    }

    private static func observe(_ box: Box, _ continuation: AsyncStream<LiveActivityPushEvent>.Continuation) async {
        let initial = observation(of: box.activity, token: box.activity.pushToken.map(LiveActivityPushEvents.hex))
        let latest = OSAllocatedUnfairLock(initialState: initial)
        continuation.yield(.activity(initial))
        @Sendable func publish(_ change: @Sendable (inout LiveActivityObservation) -> Void) {
            continuation.yield(.activity(latest.withLock { current in
                change(&current)
                return current
            }))
        }
        await withDiscardingTaskGroup { group in
            group.addTask {
                for await token in box.activity.pushTokenUpdates {
                    let hex = LiveActivityPushEvents.hex(token)
                    publish { $0.pushToken = hex }
                }
            }
            group.addTask {
                for await state in box.activity.activityStateUpdates {
                    let observed = observedState(state)
                    publish { $0.state = observed }
                }
            }
            group.addTask {
                for await content in box.activity.contentUpdates {
                    let remote = content.state.remote
                    publish { $0.remote = remote }
                }
            }
        }
    }

    private static func observation(of activity: Activity<TimerActivityAttributes>, token: String?) -> LiveActivityObservation {
        LiveActivityObservation(
            activityID: activity.id,
            babyID: activity.attributes.babyID,
            kindRaw: activity.attributes.kindRaw,
            scope: activity.attributes.scope,
            remote: activity.content.state.remote,
            state: observedState(activity.activityState),
            pushToken: token
        )
    }

    private static func observedState(_ state: ActivityState) -> LiveActivityObservedState {
        switch state {
        case .pending: .pending
        case .active: .active
        case .stale: .stale
        case .ended: .ended
        case .dismissed: .dismissed
        // Fail closed: an unrecognised state must not register a token as
        // active, nor end or dismiss a card the server still serves.
        @unknown default: .unknown
        }
    }
}
#endif
