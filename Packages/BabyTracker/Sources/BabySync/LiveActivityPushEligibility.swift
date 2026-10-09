import BabyProtocol
import Foundation

/// Whether this device may hand ActivityKit push tokens to its server.
///
/// Exactly one server qualifies: the built-in one, matched on origin AND
/// instance, with the live probe agreeing on both and advertising the push
/// capability. A self-hosted server keeps today's behaviour: no token is ever
/// relayed anywhere, to it or to the built-in server.
public enum LiveActivityPushEligibility {
    public static func isEligible(
        endpoint: ConfirmedEndpoint,
        info: ServerInfo,
        builtIn: ConfirmedEndpoint,
        hasFamily: Bool,
        activitiesAllowed: Bool
    ) -> Bool {
        isBuiltInPushServer(endpoint: endpoint, info: info, builtIn: builtIn) && hasFamily && activitiesAllowed
    }

    /// The server half of eligibility, without the device's permission.
    public static func isBuiltInPushServer(endpoint: ConfirmedEndpoint, info: ServerInfo, builtIn: ConfirmedEndpoint) -> Bool {
        isBuiltInServer(endpoint: endpoint, info: info, builtIn: builtIn) && info.advertisesLiveActivityPush
    }

    /// The built-in server's identity alone — origin AND instance, with the
    /// live probe agreeing on both — whether or not it advertises push right
    /// now. It honours a removal without push, so it is still where a
    /// registration it holds is taken down while its push worker is stopped.
    public static func isBuiltInServer(endpoint: ConfirmedEndpoint, info: ServerInfo, builtIn: ConfirmedEndpoint) -> Bool {
        endpoint == builtIn
            && info.serverInstanceID == builtIn.serverInstanceID
            && info.canonicalOrigin == builtIn.origin.canonicalString
    }

    /// The family session a registration belongs to, if any. Only an active
    /// shared family has the authority the server checks; a family in
    /// recovery, a signed-out or a pre-family account has none.
    public static func familyKey(for mode: FamilySyncMode) -> FamilySessionKey? {
        if case .shared(let key) = mode { return key }
        return nil
    }
}

/// Everything one registration session is bound to. Replaced, never mutated,
/// when the realm changes; the coordinator cancels the old session's work so
/// it can never upload into the new realm.
public struct LiveActivityPushContext: Sendable, Equatable {
    public let familyKey: FamilySessionKey
    public let endpoint: ConfirmedEndpoint
    /// The live `GET /v1/server` answer that proved the capability.
    public let serverInfo: ServerInfo
    public let activitiesAllowed: Bool
    public let feedBreakMinutes: Int
    public let environment: LiveActivityAPNsEnvironment

    /// Nil unless the endpoint is the built-in server with the probe agreeing
    /// on its identity, the session key belongs to that endpoint and the
    /// server's current epoch, and the signed build carries an APNs
    /// environment. Fails closed on every other input. The capability itself
    /// is not required: without it the context can only take a registration
    /// down (`pushAdvertised`, `canRegister`), never register one.
    public init?(
        familyKey: FamilySessionKey,
        endpoint: ConfirmedEndpoint,
        serverInfo: ServerInfo,
        builtIn: ConfirmedEndpoint,
        activitiesAllowed: Bool,
        feedBreakMinutes: Int,
        environment: LiveActivityAPNsEnvironment?
    ) {
        guard let environment,
              LiveActivityPushEligibility.isBuiltInServer(endpoint: endpoint, info: serverInfo, builtIn: builtIn),
              familyKey.canonicalOrigin == endpoint.origin.canonicalString,
              familyKey.serverInstanceID == endpoint.serverInstanceID,
              familyKey.recoveryEpoch == serverInfo.recoveryEpoch,
              familyKey.recoveryEpoch >= 1, familyKey.recoveryEpoch <= UInt64(LiveActivityContract.maximumCounter)
        else { return nil }
        self.familyKey = familyKey
        self.endpoint = endpoint
        self.serverInfo = serverInfo
        self.activitiesAllowed = activitiesAllowed
        self.feedBreakMinutes = feedBreakMinutes
        self.environment = environment
    }

    /// Whether the live probe advertised `live-activity-push-v1`. Without it
    /// the server is not pushing: no device, inventory or card goes to it, only
    /// the removals this installation still owes it.
    public var pushAdvertised: Bool { serverInfo.advertisesLiveActivityPush }

    /// Whether this context may register, as opposed to only take down.
    public var canRegister: Bool { activitiesAllowed && pushAdvertised }

    /// What identifies a realm for cancellation. A changed APNs environment is
    /// a transition too: the server ends tokens registered under the old one.
    struct Realm: Equatable {
        let familyKey: FamilySessionKey
        let endpoint: ConfirmedEndpoint
        let environment: LiveActivityAPNsEnvironment
    }

    var realm: Realm { Realm(familyKey: familyKey, endpoint: endpoint, environment: environment) }

    var recoveryEpoch: Int64 { Int64(familyKey.recoveryEpoch) }

    func contains(_ scope: LiveActivityScope) -> Bool {
        scope.serverInstanceID == endpoint.serverInstanceID
            && scope.recoveryEpoch == recoveryEpoch
            && scope.familyID == familyKey.familyID
    }

    func replacing(feedBreakMinutes: Int? = nil, activitiesAllowed: Bool? = nil) -> LiveActivityPushContext {
        LiveActivityPushContext(
            copying: self,
            feedBreakMinutes: feedBreakMinutes ?? self.feedBreakMinutes,
            activitiesAllowed: activitiesAllowed ?? self.activitiesAllowed
        )
    }

    private init(copying other: LiveActivityPushContext, feedBreakMinutes: Int, activitiesAllowed: Bool) {
        familyKey = other.familyKey
        endpoint = other.endpoint
        serverInfo = other.serverInfo
        environment = other.environment
        self.feedBreakMinutes = feedBreakMinutes
        self.activitiesAllowed = activitiesAllowed
    }
}
