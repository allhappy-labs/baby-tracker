import BabyCore
import BabyProtocol
@testable import BabyStore
import Foundation
import Testing
@testable import BabySync
@testable import SettingsFeature

/// Live Activity push through the real composition: the gate before signed
/// phones and Apple.
///
/// Three phones are built with the app's own graph (`FamilySyncComposition
/// .assemble`) against the container `protocol-e2e.sh` built from this tree.
/// Each has the production push driver, coordinator, registration ledger and
/// `LiveActivityPushClient`; only ActivityKit is a scripted board, and only
/// Apple is the identitytest recorder inside the server, which keeps the
/// production APNs transport (provider token, headers, payload) and hands each
/// request to the harness. Every payload the recorder took is decoded here
/// with the app's own `TimerActivityContentState`.
///
/// The owner runs care, and its own card is requested by the app's card
/// controller and adopted by the server. The joiner never syncs: everything
/// its cards show comes from the server's worker. A third phone treats the
/// same server as self-hosted and must never register anything; it is also
/// the partner whose change must reach the owner's own card.
///
/// Inert without `BABY_TRACKER_E2E_APNS=1`, which `protocol-e2e.sh` sets only
/// for the push-enabled profile; the script then requires the last proof line.
@MainActor
@Suite("Live Activity push through a live server", .serialized)
struct LiveActivityPushE2ETests {
    // Synthetic APNs tokens, recognisable in the recorder and absent from logs.
    static let ownerStart = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
    static let ownerFeed = "a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2"
    static let joinerStart = "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
    static let joinerFeed = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
    static let joinerSleep = "b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3"
    static let selfHostedStart = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
    static let topic = "com.example.babytracker.protocol.push-type.liveactivity"

    @Test func twoCaregiversCardsArePushedThroughTheRealComposition() async throws {
        guard let environment = E2EEnvironment.current(), ProcessInfo.processInfo.environment["BABY_TRACKER_E2E_APNS"] == "1" else { return }
        let live = try LiveServer(environment)
        let apple = APNsRecorder(base: environment.baseURL, session: live.session)
        // Care is dated against the wall clock, which the server's worker
        // projects breaks and deadlines on, but behind it: each part must run
        // long enough to be kept (a breast part under 30 s is discarded), and
        // the steps move the clock forward to a set distance behind now.
        let clock = TestDateProvider(Date().addingTimeInterval(-600))
        let scratch = FileManager.default.temporaryDirectory
            .appending(path: "live-activity-push-e2e-\(UUID().uuidString)", directoryHint: .isDirectory)
        try FileManager.default.createDirectory(at: scratch, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: scratch) }

        let ownerBoard = ScriptedActivityKit(), joinerBoard = ScriptedActivityKit(), selfHostedBoard = ScriptedActivityKit()
        let ownerLedger = Box<LiveActivityRegistrationStore>(), joinerLedger = Box<LiveActivityRegistrationStore>(),
            selfHostedLedger = Box<LiveActivityRegistrationStore>()
        let joinerProbes = ProbeCounter(), selfHostedProbes = ProbeCounter()
        let identity = live.identityClient()
        // The owner's card controller: its own push state (what the app's
        // process shares) and the cards it asks ActivityKit for.
        let ownerPushState = LiveActivityPushState(), ownerCards = MemoryCardBoard()

        let owner = try Phone(name: "push-owner", live: live, clock: clock, directory: scratch, appleSubject: UUID().uuidString.lowercased(),
            liveActivityPush: Self.wiring(board: ownerBoard, ledger: ownerLedger, builtIn: live.endpoint, directory: scratch.appending(path: "owner-push"),
                                          pushState: ownerPushState))
        // The joiner's private network is down for its first probe, as a
        // phone's tailnet often is right after it wakes: registration is late,
        // and Apple has nothing to do with it.
        let joiner = try Phone(name: "push-joiner", live: live, clock: clock, directory: scratch, appleSubject: UUID().uuidString.lowercased(),
            liveHintsEnabled: false,
            liveActivityPush: Self.wiring(board: joinerBoard, ledger: joinerLedger, builtIn: live.endpoint, directory: scratch.appending(path: "joiner-push"),
                                          probe: { origin in
                                              if joinerProbes.next() == 1 { throw URLError(.cannotConnectToHost) }
                                              return try await identity.serverInfo(origin)
                                          }))
        // To this phone the same server is a self-hosted one: its built-in
        // server is the production endpoint.
        let selfHosted = try Phone(name: "push-self-hosted", live: live, clock: clock, directory: scratch, appleSubject: UUID().uuidString.lowercased(),
            liveHintsEnabled: false,
            liveActivityPush: Self.wiring(board: selfHostedBoard, ledger: selfHostedLedger, builtIn: try #require(FamilySyncComposition.hostedEndpoint),
                                          directory: scratch.appending(path: "self-hosted-push"),
                                          probe: { origin in
                                              _ = selfHostedProbes.next()
                                              return try await identity.serverInfo(origin)
                                          }))
        defer { owner.forgetDefaults(); joiner.forgetDefaults(); selfHosted.forgetDefaults() }
        let ownerPush = try #require(owner.push), joinerPush = try #require(joiner.push), selfHostedPush = try #require(selfHosted.push)

        // MARK: 1-2. A family of three, over the app's own sign-in and invitation path.

        _ = try await owner.signIn(displayName: "Push Owner", live: live)
        try await owner.model.createFamily(name: "E2E live activity push")
        let familyKey = try owner.familyKey()
        let baby = try owner.store.addBaby(name: "Push Baby", birthDate: clock.now.addingTimeInterval(-30 * 86_400), referenceSex: .girls, expectedDueDate: nil)
        try await owner.sync(.localWrite)
        proof(1, "owner signed in, created family=\(familyKey.familyID) and synced baby=\(baby.id)")
        for phone in [joiner, selfHosted] {
            let invitation = try InvitationParser.parse(try await owner.model.createInvitation().shareURL.absoluteString)
            _ = try await phone.signIn(displayName: "Push \(phone.name)", live: live)
            try await phone.model.acceptInvitation(invitation)
            #expect(try phone.familyKey().familyID == familyKey.familyID)
        }
        let joinerSyncs = joiner.traffic.syncRequests
        let info = try await identity.serverInfo(live.endpoint.origin)
        #expect(info.advertisesLiveActivityPush, "the push profile's server does not advertise the capability")
        proof(2, "joiner and a third phone joined through invitations; the server advertises \(LiveActivityContract.capability); joiner sync requests so far=\(joinerSyncs)")

        // MARK: 3. The owner's phone registers automatically at launch.

        ownerBoard.send(.pushToStartToken(Self.ownerStart))
        await ownerPush.launch()
        await Self.settle(ownerPush, ownerBoard)
        #expect(try ownerLedger.value.read().device?.accepted == true, "the owner's device registration was not accepted")
        // The third phone launches too: a self-hosted server is never probed
        // for push and never handed a token, capability or not.
        selfHostedBoard.send(.pushToStartToken(Self.selfHostedStart))
        await selfHostedPush.launch()
        await Self.settle(selfHostedPush, selfHostedBoard)
        #expect(selfHostedProbes.count == 0, "a self-hosted server was probed for push")
        #expect(try selfHostedLedger.value.read().device == nil, "a self-hosted server was sent a registration")
        proof(3, "owner registered through LiveActivityPushClient with no settings step; the self-hosted phone made \(selfHostedProbes.count) push probes and no registration")

        // MARK: 4. The owner starts a feed; its own card is local, and adopted.

        let leftPart = try owner.store.startPart(.breast(.left), for: baby.id)
        try await owner.sync(.localWrite)
        let feedScope = try LiveActivityScope(serverInstanceID: live.endpoint.serverInstanceID, recoveryEpoch: Int64(familyKey.recoveryEpoch),
                                              familyID: familyKey.familyID, babyID: baby.id, kindRaw: "feed")
        // The app asks for the owner's own card the moment the part starts.
        await LiveCards.$board.withValue(ownerCards) {
            await LiveCards.$pushState.withValue(ownerPushState) {
                await LiveActivityController.adoptOrStart(for: leftPart, in: owner.store)
            }
        }
        let ownerCard = try #require(ownerCards.cards().first)
        #expect(ownerCard.scope == feedScope && ownerCard.state.remote == nil && ownerCards.requests.last?.isPushEnabled == true)
        // ActivityKit issues its update token; the coordinator registers it.
        ownerBoard.send(.activity(LiveActivityObservation(activityID: ownerCard.id, babyID: baby.id, kindRaw: "feed", scope: feedScope,
                                                          state: .active, pushToken: Self.ownerFeed)))
        await Self.settle(ownerPush, ownerBoard)
        #expect(try ownerLedger.value.read().activities[ownerCard.id]?.accepted == true)
        let adopted = try await apple.next("the owner's own card adopted") { $0.token == Self.ownerFeed }
        Self.requireApple(adopted)
        let ownerRemote = try #require(adopted.aps.contentState.remote)
        #expect(adopted.aps.event == "update" && ownerRemote.timerID == leftPart.id && adopted.aps.contentState.activityKind == .breastLeft)
        proof(4, "owner started feed timer=\(leftPart.id); its own card registered bound to that timer and the server adopted it (update on generation \(ownerRemote.generationID), no start)")

        // MARK: 5. The joiner registers late.

        joinerBoard.send(.pushToStartToken(Self.joinerStart))
        await joinerPush.launch()
        await Self.settle(joinerPush, joinerBoard)
        #expect(joinerProbes.count == 2, "joiner probes=\(joinerProbes.count)")
        #expect(try joinerLedger.value.read().device?.accepted == true, "the joiner never registered after its network came back")
        proof(5, "joiner's first probe found no network; the driver's retry registered it (\(joinerProbes.count) probes)")

        // MARK: 6. The running feed is pushed to start on the joiner.

        let feedStart = try await apple.next("the running feed's push-to-start") { $0.token == Self.joinerStart && $0.aps.event == "start" }
        Self.requireApple(feedStart)
        let feedRemote = try #require(feedStart.aps.contentState.remote)
        #expect(feedStart.headers["apns-expiration"] == "0" && feedStart.headers["apns-priority"] == "10")
        #expect(feedStart.aps.attributesType == "TimerActivityAttributes" && feedStart.aps.inputPushToken == 1)
        #expect(feedStart.aps.alert?.title.locKey == "LIVE_ACTIVITY_FEED_START_TITLE" && feedStart.aps.alert?.body.locArgs == ["Push Baby"])
        let attributes = try #require(feedStart.aps.attributes)
        #expect(attributes.babyID == baby.id && attributes.kindRaw == "feed" && attributes.scope == feedScope && attributes.babyName == "Push Baby")
        #expect(feedStart.aps.contentState.activityKind == .breastLeft && feedStart.aps.contentState.phase == .running)
        #expect(feedRemote.timerID == leftPart.id && feedRemote.scope == feedScope && feedRemote.canWrite)
        #expect(abs(feedStart.aps.contentState.startedAt.timeIntervalSince(leftPart.startedAt)) < 0.001)
        proof(6, "recorder took POST \(feedStart.path.prefix(12))… topic=\(feedStart.headers["apns-topic"] ?? "-") with a valid ES256 provider token; decoded TimerActivityContentState kind=\(feedStart.aps.contentState.activityKind) phase=\(feedStart.aps.contentState.phase) generation=\(feedRemote.generationID)")

        // MARK: 7. ActivityKit made the joiner's card; it registers its token.

        let joinerFeedCard = LiveActivityObservation(activityID: "joiner-feed", babyID: baby.id, kindRaw: "feed", scope: attributes.scope,
                                                     remote: feedRemote, state: .active, pushToken: Self.joinerFeed)
        joinerBoard.send(.activity(joinerFeedCard))
        joinerBoard.setInventory([joinerFeedCard])
        await Self.settle(joinerPush, joinerBoard)
        let confirmed = try await apple.next("the joiner's card confirmed") { $0.token == Self.joinerFeed }
        Self.requireApple(confirmed)
        #expect(confirmed.aps.event == "update" && confirmed.aps.contentState.remote?.generationID == feedRemote.generationID)
        proof(7, "joiner registered its pushed card; the server answered with \(confirmed.aps.event) on generation \(feedRemote.generationID), not another start")

        // MARK: 8. The owner pauses: a feed break on the joiner's card.

        clock.catchUp(lagging: 120)
        let pausedLeft = try #require(try owner.store.stopFeed(leftPart))
        try await owner.sync(.localWrite)
        let pause = try await apple.next("the pause") { $0.token == Self.joinerFeed && $0.aps.contentState.phase == .feedBreak }
        Self.requireApple(pause)
        #expect(pause.aps.event == "update" && pause.aps.contentState.remote?.generationID == feedRemote.generationID)
        #expect(pause.aps.contentState.parts.count == 1 && pause.aps.contentState.parts.first?.side == "left")
        let breakEnds = try #require(pause.aps.contentState.breakEndsAt)
        let pausedEnd = try #require(pausedLeft.endedAt)
        #expect(abs(breakEnds.timeIntervalSince(pausedEnd) - 5 * 60) < 0.001, "break ends \(breakEnds), feed ended \(pausedEnd)")
        #expect(pause.aps.staleDate != nil)
        proof(8, "owner paused; joiner's card shows a feed break ending \(Int(breakEnds.timeIntervalSince(pausedEnd))) s after the part (its own 5-minute limit)")

        // MARK: 9. The other side, in the same session: the same card.

        clock.catchUp(lagging: 90)
        let rightPart = try owner.store.startPart(.breast(.right), for: baby.id)
        try await owner.sync(.localWrite)
        let resumed = try await apple.next("the other side") { $0.token == Self.joinerFeed && $0.aps.contentState.activityKind == .breastRight }
        Self.requireApple(resumed)
        #expect(resumed.aps.event == "update" && resumed.aps.contentState.phase == .running)
        #expect(resumed.aps.contentState.remote?.generationID == feedRemote.generationID && resumed.aps.contentState.remote?.timerID == rightPart.id)
        proof(9, "owner started the right side in the same session; the joiner's card updated in place (timer=\(rightPart.id), same generation)")

        // MARK: 10. A start whose acknowledgment is lost.

        try await apple.loseNextAcknowledgment(to: Self.joinerStart)
        clock.catchUp(lagging: 60)
        let sleepTimer = try owner.store.startSleep(for: baby.id)
        try await owner.sync(.localWrite)
        let lost = try await apple.next("the sleep start") { $0.token == Self.joinerStart && $0.aps.event == "start" && $0.aps.contentState.activityKind == .sleep }
        #expect(!lost.acknowledged)
        let lostRemote = try #require(lost.aps.contentState.remote)
        // Nothing has repeated the lost start by the time the joiner comes to
        // the foreground: the recorder holds that one attempt and no other.
        let beforeProof = try await apple.requests().filter { $0.token == Self.joinerStart && $0.aps.contentState.remote?.timerID == sleepTimer.id }
        #expect(beforeProof.count == 1 && beforeProof.first?.headers["apns-id"] == lost.headers["apns-id"],
                "starts before the absence proof=\(beforeProof.count)")
        // The joiner comes to the foreground; ActivityKit lists only its feed
        // card, which proves the sleep card absent for exactly one retry.
        joinerBoard.foreground = true
        await joinerPush.sceneBecameActive()
        await Self.settle(joinerPush, joinerBoard)
        joinerBoard.foreground = false
        let retry = try await apple.next("the retried start", after: lost.sequence) { $0.token == Self.joinerStart && $0.aps.event == "start" }
        Self.requireApple(retry)
        let retryRemote = try #require(retry.aps.contentState.remote)
        #expect(retry.headers["apns-id"] != lost.headers["apns-id"] && retryRemote.timerID == sleepTimer.id)
        let joinerSleepCard = LiveActivityObservation(activityID: "joiner-sleep", babyID: baby.id, kindRaw: "sleep", scope: try #require(retry.aps.attributes?.scope),
                                                      remote: retryRemote, state: .active, pushToken: Self.joinerSleep)
        joinerBoard.send(.activity(joinerSleepCard))
        joinerBoard.setInventory([joinerFeedCard, joinerSleepCard])
        await Self.settle(joinerPush, joinerBoard)
        _ = try await apple.next("the retried card confirmed") { $0.token == Self.joinerSleep }
        let starts = try await apple.requests().filter { $0.token == Self.joinerStart && $0.aps.contentState.remote?.timerID == sleepTimer.id }
        #expect(starts.count == 2, "sleep starts=\(starts.count)")
        proof(10, "the sleep start reached the recorder unacknowledged (generation \(lostRemote.generationID)); the recorder still held only that attempt when the joiner came to the foreground (\(beforeProof.count) start); after its inventory proved the card absent, exactly one retry (new apns-id), then the card registered")

        // MARK: 11. The owner stops the sleep and the feed.

        clock.catchUp()
        _ = try owner.store.stopSleep(sleepTimer)
        _ = try owner.store.stopFeed(rightPart)
        try await owner.sync(.localWrite)
        let sleepEnd = try await apple.next("the sleep end") { $0.token == Self.joinerSleep && $0.aps.event == "end" }
        Self.requireApple(sleepEnd)
        #expect(sleepEnd.aps.dismissalDate == sleepEnd.aps.timestamp - 1 && sleepEnd.aps.contentState.remote?.generationID == retryRemote.generationID)
        let secondPause = try await apple.next("the second pause") { $0.token == Self.joinerFeed && $0.aps.contentState.phase == .feedBreak && $0.aps.contentState.parts.count == 2 }
        Self.requireApple(secondPause)
        #expect(secondPause.aps.contentState.suggestedSide == "left" && secondPause.aps.contentState.remote?.generationID == feedRemote.generationID)
        let ownerPause = try await apple.next("the owner's own card in the break") {
            $0.token == Self.ownerFeed && $0.aps.contentState.phase == .feedBreak && $0.aps.contentState.parts.count == 2
        }
        Self.requireApple(ownerPause)
        proof(11, "owner stopped the sleep (end with dismissal-date, decoded) and the feed (break listing both sides, suggesting left, on both phones' cards)")

        // MARK: 12. A partner's change reaches the owner's own card.

        // The third caregiver — a family member whose phone treats this
        // server as self-hosted, so pushes nothing itself — picks the session
        // up within the break on the other side.
        clock.catchUp()
        try await selfHosted.sync()
        let partnerPart = try selfHosted.store.startPart(.breast(.left), for: baby.id)
        try await selfHosted.sync(.localWrite)
        let partnerChange = try await apple.next("the partner's part on the owner's own card", after: ownerPause.sequence) {
            $0.token == Self.ownerFeed && $0.aps.contentState.remote?.timerID == partnerPart.id
        }
        Self.requireApple(partnerChange)
        #expect(partnerChange.aps.event == "update" && partnerChange.aps.contentState.phase == .running
                && partnerChange.aps.contentState.activityKind == .breastLeft
                && partnerChange.aps.contentState.remote?.generationID == ownerRemote.generationID)
        let joinerResumed = try await apple.next("the partner's part on the joiner's card", after: ownerPause.sequence) {
            $0.token == Self.joinerFeed && $0.aps.contentState.remote?.timerID == partnerPart.id
        }
        #expect(joinerResumed.aps.event == "update" && joinerResumed.aps.contentState.remote?.generationID == feedRemote.generationID)
        proof(12, "the partner resumed the feed on the left (timer=\(partnerPart.id)); the owner's own card received it as an update on its generation, as did the joiner's")

        // MARK: 13. What never happened.

        let all = try await apple.requests()
        #expect(!all.contains { $0.token == Self.ownerStart }, "the owner was pushed a start for its own timer")
        #expect(!all.contains { $0.token == Self.selfHostedStart }, "a self-hosted phone's token reached the server")
        #expect(all.allSatisfy { $0.method == "POST" && $0.host == "sandbox.apns.identitytest.invalid" && $0.providerTokenValid })
        #expect(joiner.traffic.syncRequests == joinerSyncs, "the joiner synced: \(joinerSyncs) → \(joiner.traffic.syncRequests)")
        let selfHostedDevice = try selfHostedLedger.value.read().device
        #expect(selfHostedProbes.count == 0 && selfHostedDevice == nil)
        // The phones' own ledgers, which the harness checks against Postgres:
        // devices the server holds, their APNs environment, cards it accepted.
        let phoneLedgers = [try ownerLedger.value.read(), try joinerLedger.value.read()]
        let devices = phoneLedgers.compactMap(\.device)
        let registered = devices.filter { $0.accepted && !$0.revoked }.count
        let sandbox = devices.filter { $0.environment == .sandbox }.count
        let cards = phoneLedgers.reduce(0) { $0 + $1.activities.values.filter(\.accepted).count }
        #expect(registered == 2 && sandbox == 2 && cards == 3, "ledgers: registrations=\(registered) sandbox=\(sandbox) cards=\(cards)")
        print("live-activity ledger: family=\(familyKey.familyID.uuidString.lowercased()) registrations=\(registered) sandbox=\(sandbox) cards=\(cards)")
        print("live-activity tokens: \([Self.ownerStart, Self.ownerFeed, Self.joinerStart, Self.joinerFeed, Self.joinerSleep, Self.selfHostedStart].joined(separator: " "))")
        proof(13, "\(all.count) APNs requests decoded; none to the owner's push-to-start token, none from the self-hosted phone; the joiner made no sync request after joining (\(joinerSyncs))")
    }

    // MARK: Wiring

    private static func wiring(
        board: ScriptedActivityKit, ledger: Box<LiveActivityRegistrationStore>, builtIn: ConfirmedEndpoint, directory: URL,
        probe: (@Sendable (ServerOrigin) async throws -> ServerInfo)? = nil, pushState: LiveActivityPushState = LiveActivityPushState()
    ) -> (any KeychainBackend) -> FamilySyncComposition.LiveActivityPushWiring {
        { keychain in
            let store = LiveActivityRegistrationStore(directory: directory, keychain: keychain)
            ledger.value = store
            return FamilySyncComposition.LiveActivityPushWiring(
                events: board, store: store, builtIn: builtIn, environment: SandboxBuild(), probe: probe, makeClient: nil,
                background: InlineLiveActivityPushExecution(), feedBreakMinutes: { 5 },
                isForeground: { board.foreground }, pushState: pushState
            )
        }
    }

    /// Every board event handled, every driver evaluation finished, the pump idle.
    private static func settle(_ driver: LiveActivityPushDriver, _ board: ScriptedActivityKit) async {
        await driver.coordinator.waitForHandledEvents(board.emitted)
        await driver.waitUntilSettled()
    }

    /// What every request must look like on the wire, as Apple would see it.
    private static func requireApple(_ request: PushedRequest) {
        #expect(request.method == "POST" && request.host == "sandbox.apns.identitytest.invalid" && request.path == "/3/device/\(request.token)")
        #expect(request.headers["apns-push-type"] == "liveactivity" && request.headers["apns-topic"] == topic)
        #expect(request.providerTokenValid && request.providerTokenKeyID == "E2EKEY0001" && request.providerTokenIssuer == "E2ETEAM001")
        #expect(UUID(uuidString: request.headers["apns-id"] ?? "") != nil && request.acknowledged)
        #expect(request.aps.timestamp > 0)
    }
}

private func proof(_ step: Int, _ line: String) {
    print("live-activity proof: step \(step): \(line)")
}

// MARK: - ActivityKit, scripted

/// The phone's ActivityKit: push-to-start and per-card tokens, the cards on
/// screen, and whether the app is in the foreground. Events sent before the
/// coordinator subscribes are replayed to it, as ActivityKit replays a token.
private final class ScriptedActivityKit: LiveActivityPushEventSource, @unchecked Sendable {
    private let lock = NSLock()
    private var continuations: [AsyncStream<LiveActivityPushEvent>.Continuation] = []
    private var buffered: [LiveActivityPushEvent] = []
    private var cards: [LiveActivityObservation] = []
    private var sent = 0
    private var inForeground = false

    var emitted: Int { lock.withLock { sent } }
    var foreground: Bool {
        get { lock.withLock { inForeground } }
        set { lock.withLock { inForeground = newValue } }
    }

    func events() -> AsyncStream<LiveActivityPushEvent> {
        let (stream, continuation) = AsyncStream.makeStream(of: LiveActivityPushEvent.self)
        lock.withLock {
            continuations.append(continuation)
            for event in buffered { continuation.yield(event) }
            buffered.removeAll()
        }
        return stream
    }

    func send(_ event: LiveActivityPushEvent) {
        lock.withLock {
            sent += 1
            if continuations.isEmpty { buffered.append(event) }
            for continuation in continuations { continuation.yield(event) }
        }
    }

    func setInventory(_ value: [LiveActivityObservation]) { lock.withLock { cards = value } }
    func inventory() -> [LiveActivityObservation] { lock.withLock { cards } }
    func activitiesEnabled() -> Bool { true }
}

/// A signed development build: the simulator's environment.
private struct SandboxBuild: LiveActivityAPNsEnvironmentResolving {
    func environment() -> LiveActivityAPNsEnvironment? { .sandbox }
}

private final class ProbeCounter: @unchecked Sendable {
    private let lock = NSLock()
    private var calls = 0
    var count: Int { lock.withLock { calls } }
    func next() -> Int { lock.withLock { calls += 1; return calls } }
}

private final class Box<Value>: @unchecked Sendable {
    private let lock = NSLock()
    private var stored: Value?
    var value: Value {
        get { lock.withLock { stored! } }
        set { lock.withLock { stored = newValue } }
    }
}

private extension TestDateProvider {
    /// Moves the phones' clock up to `lagging` seconds behind the wall
    /// clock (never backwards).
    func catchUp(lagging: TimeInterval = 0) {
        let behind = Date().addingTimeInterval(-lagging).timeIntervalSince(now)
        if behind > 0 { advance(by: behind) }
    }
}

// MARK: - Apple, as the recorder saw it

/// One request the server's APNs transport made, with its JSON payload decoded
/// by the app's own types.
struct PushedRequest: Decodable {
    struct APS: Decodable {
        struct Alert: Decodable {
            struct Text: Decodable {
                let locKey: String
                let locArgs: [String]?
                enum CodingKeys: String, CodingKey { case locKey = "loc-key", locArgs = "loc-args" }
            }
            let title: Text
            let body: Text
        }
        /// `TimerActivityAttributes` is iOS-only; these are its stored fields.
        struct Attributes: Decodable, Equatable {
            let babyID: UUID
            let babyName: String
            let kindRaw: String
            let scope: LiveActivityScope
        }
        let event: String
        let timestamp: Int64
        let contentState: TimerActivityContentState
        let attributesType: String?
        let attributes: Attributes?
        let alert: Alert?
        let inputPushToken: Int?
        let staleDate: Int64?
        let dismissalDate: Int64?
        enum CodingKeys: String, CodingKey {
            case event, timestamp, alert, attributes
            case contentState = "content-state", attributesType = "attributes-type", inputPushToken = "input-push-token"
            case staleDate = "stale-date", dismissalDate = "dismissal-date"
        }
    }
    struct Payload: Decodable { let aps: APS }

    let sequence: Int
    let method: String
    let host: String
    let path: String
    let token: String
    let headers: [String: String]
    let providerTokenValid: Bool
    let providerTokenKeyID: String
    let providerTokenIssuer: String
    let acknowledged: Bool
    let payload: Payload
    var aps: APS { payload.aps }

    enum CodingKeys: String, CodingKey {
        case sequence, method, host, path, token, headers, payload, acknowledged
        case providerTokenValid = "provider_token_valid", providerTokenKeyID = "provider_token_key_id", providerTokenIssuer = "provider_token_issuer"
    }
}

/// The identitytest recorder behind `/__identitytest/apns`, over the same
/// pinned TLS as the phones.
@MainActor
struct APNsRecorder {
    let base: String
    let session: URLSession

    func requests() async throws -> [PushedRequest] {
        let (data, response) = try await session.data(from: try #require(URL(string: base + "/__identitytest/apns")))
        guard (response as? HTTPURLResponse)?.statusCode == 200 else {
            throw E2EFailure("APNs recorder unavailable: \(String(decoding: data, as: UTF8.self))")
        }
        struct Envelope: Decodable { let requests: [PushedRequest] }
        return try JSONDecoder().decode(Envelope.self, from: data).requests
    }

    /// The first request after `sequence` that matches, waiting for the worker.
    /// The bound is the promptness this gate proves: one-second polls, a send
    /// bounded at five seconds, stale leases released at once (e4e01a5).
    func next(_ what: String, after sequence: Int = 0, where matches: (PushedRequest) -> Bool) async throws -> PushedRequest {
        let deadline = Date().addingTimeInterval(15)
        while Date() < deadline {
            if let found = try await requests().first(where: { $0.sequence > sequence && matches($0) }) {
                return found
            }
            try await Task.sleep(for: .milliseconds(250))
        }
        let seen = try await requests().map { "\($0.sequence):\($0.aps.event):\($0.token.prefix(4))" }
        throw E2EFailure("no APNs request for \(what) within 15 s; recorder has \(seen)")
    }

    func loseNextAcknowledgment(to token: String) async throws {
        var request = URLRequest(url: try #require(URL(string: base + "/__identitytest/apns/lose-ack")))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONSerialization.data(withJSONObject: ["token": token])
        let (_, response) = try await session.data(for: request)
        guard (response as? HTTPURLResponse)?.statusCode == 204 else { throw E2EFailure("lose-ack refused") }
    }
}
