import Foundation
import Testing
import BabyProtocol
@testable import BabyStore

@Suite("Live Activity payload contracts")
struct LiveActivityPayloadTests {
    @Test func legacyPayloadDecodes() throws {
        let raw = #"{"startedAt":780000000,"sideLabel":"Left","activityKind":"breastLeft","canSwitchSide":true}"#
        let state = try JSONDecoder().decode(TimerActivityContentState.self, from: Data(raw.utf8))
        #expect(state.remote == nil)
        #expect(state.omittedParts == 0)
        #expect(state.phase == .running)
        #expect(state.sideLabel == "Left")
        #expect(state.localizedCaption == "Left")
    }

    @Test func scopeCannotCrossFamily() throws {
        let server = UUID(), family = UUID(), baby = UUID()
        let scope = try LiveActivityScope(serverInstanceID: server, recoveryEpoch: 1, familyID: family, babyID: baby, kindRaw: "feed")
        #expect(scope.matches(serverInstanceID: server, recoveryEpoch: 1, familyID: family))
        #expect(!scope.matches(serverInstanceID: server, recoveryEpoch: 1, familyID: UUID()))
        #expect(!scope.matches(serverInstanceID: UUID(), recoveryEpoch: 1, familyID: family))
        #expect(!scope.matches(serverInstanceID: server, recoveryEpoch: 2, familyID: family))
        #expect(scope.matchesTarget(babyID: baby, kindRaw: "feed"))
        #expect(!scope.matchesTarget(babyID: baby, kindRaw: "sleep"))
        #expect(!scope.matchesTarget(babyID: UUID(), kindRaw: "feed"))
    }

    @Test func remoteTypedCaptionIgnoresServerLanguage() throws {
        let scope = try LiveActivityScope(serverInstanceID: UUID(), recoveryEpoch: 1, familyID: UUID(), babyID: UUID(), kindRaw: "feed")
        let remote = try LiveActivityRevision(scope: scope, generationID: UUID(), familySequence: 3,
            presentationVersion: 2, presentationTimestamp: 1_790_985_600, canWrite: true)
        let running = TimerActivityContentState(startedAt: Date(timeIntervalSinceReferenceDate: 812_678_400),
            sideLabel: "server language", activityKind: .breastLeft, remote: remote)
        #expect(running.sideLabel == "server language")
        #expect(running.localizedCaption == FeedActivityKind.breastLeft.label)
        let paused = TimerActivityContentState(startedAt: running.startedAt, sideLabel: "server language", activityKind: .paused, remote: remote)
        #expect(paused.localizedCaption == FeedActivityKind.paused.label)
        let feedBreak = TimerActivityContentState(startedAt: running.startedAt, sideLabel: "server language", activityKind: .mixedBottle, phase: .feedBreak, remote: remote)
        #expect(feedBreak.localizedCaption == "Break")
    }

    @Test func remoteRoundTripSeparatesFoundationDatesFromAPNsTimestamp() throws {
        let scope = try LiveActivityScope(serverInstanceID: UUID(), recoveryEpoch: 1, familyID: UUID(), babyID: UUID(), kindRaw: "feed")
        let remote = try LiveActivityRevision(scope: scope, generationID: UUID(), timerID: UUID(), sessionID: UUID(), familySequence: 3,
            presentationVersion: 2, presentationTimestamp: 1_790_985_600, canWrite: false)
        let state = TimerActivityContentState(startedAt: Date(timeIntervalSince1970: 1_790_985_600), sideLabel: "",
            activityKind: .mixedBottle, phase: .feedBreak, breakEndsAt: Date(timeIntervalSince1970: 1_790_987_400),
            parts: [PartLabel(side: "left", seconds: 60), PartLabel(side: nil, seconds: 90)], suggestedSide: "right", omittedParts: 3, remote: remote)
        let data = try JSONEncoder().encode(state)
        let object = try #require(JSONSerialization.jsonObject(with: data) as? [String: Any])
        #expect(object["startedAt"] as? Double == 812_678_400)
        let metadata = try #require(object["remote"] as? [String: Any])
        #expect(metadata["presentationTimestamp"] as? Int64 == 1_790_985_600)
        #expect(metadata["generationID"] as? String == remote.generationID.uuidString.lowercased())
        #expect(try JSONDecoder().decode(TimerActivityContentState.self, from: data) == state)
    }

    @Test func fixtureParity() throws {
        let fixtures = ProtocolFixtureLoader(directory: Self.fixtureDirectory)
        let states = try fixtures.decode([String: TimerActivityContentState].self, named: "content-states.json")
        #expect(states.count == 8)
        let left = try #require(states["breast-left"])
        #expect(left.startedAt.timeIntervalSince1970 == 1_790_985_600)
        #expect(left.remote?.presentationTimestamp == 1_790_985_600)
        #expect(states["edited-start"]?.startedAt.timeIntervalSince1970 == 1_790_985_570.5)
        #expect(states["mixed-break"]?.parts == [PartLabel(side: "left", seconds: 60), PartLabel(side: nil, seconds: 90)])
        #expect(states["mixed-break"]?.omittedParts == 3)
        #expect(states["sleep"]?.remote?.canWrite == false)
        let raw = try Data(contentsOf: Self.fixtureDirectory.appending(path: "content-states.json"))
        #expect(try JSONDecoder().decode(JSONValue.self, from: JSONEncoder().encode(states)) == JSONDecoder().decode(JSONValue.self, from: raw))
        let legacy = try fixtures.decode(TimerActivityContentState.self, named: "legacy-content.json")
        #expect(legacy.remote == nil)
        #expect(legacy.omittedParts == 0)
    }

    private static var fixtureDirectory: URL {
        URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            .appending(path: "Server/api/fixtures/live-activity-v1")
    }
}

// MARK: - What the card renders

extension LiveActivityPayloadTests {
    static func remote(canWrite: Bool) throws -> LiveActivityRevision {
        let scope = try LiveActivityScope(serverInstanceID: UUID(), recoveryEpoch: 1, familyID: UUID(), babyID: UUID(), kindRaw: "feed")
        return try LiveActivityRevision(scope: scope, generationID: UUID(), familySequence: 3, presentationVersion: 2,
                                        presentationTimestamp: 1_790_985_600, canWrite: canWrite)
    }

    /// A viewer's card shows the feed but offers nothing to tap; a
    /// caregiver's card and any card without server metadata keep their
    /// controls.
    /// A card this phone wrote while the family said viewer is read-only too,
    /// and says so on the wire only then: the server never writes the flag,
    /// and a card without it keeps today's shape exactly.
    @Test func aViewersOwnCardIsReadOnlyAndTheFlagIsWrittenOnlyWhenSet() throws {
        let start = Date(timeIntervalSinceReferenceDate: 780_000_000)
        let viewer = TimerActivityContentState(startedAt: start, sideLabel: "", activityKind: .sleep, readOnly: true)
        let caregiver = TimerActivityContentState(startedAt: start, sideLabel: "", activityKind: .sleep)
        #expect(viewer.isReadOnly)
        #expect(!viewer.offersControls(now: start))
        #expect(!caregiver.isReadOnly)

        let encoder = JSONEncoder()
        guard case .object(let viewerFields) = try JSONDecoder().decode(JSONValue.self, from: encoder.encode(viewer)),
              case .object(let caregiverFields) = try JSONDecoder().decode(JSONValue.self, from: encoder.encode(caregiver)) else {
            Issue.record("content states did not encode as objects")
            return
        }
        #expect(viewerFields["readOnly"] == .bool(true))
        #expect(caregiverFields["readOnly"] == nil)
        #expect(try JSONDecoder().decode(TimerActivityContentState.self, from: encoder.encode(viewer)) == viewer)
        #expect(try JSONDecoder().decode(TimerActivityContentState.self, from: encoder.encode(caregiver)) == caregiver)
    }

    @Test func aViewersPushedCardIsReadOnly() throws {
        let start = Date(timeIntervalSinceReferenceDate: 812_678_400)
        let viewer = TimerActivityContentState(startedAt: start, sideLabel: "", activityKind: .breastLeft, remote: try Self.remote(canWrite: false))
        let caregiver = TimerActivityContentState(startedAt: start, sideLabel: "", activityKind: .breastLeft, remote: try Self.remote(canWrite: true))
        let local = TimerActivityContentState(startedAt: start, sideLabel: "Left", activityKind: .breastLeft)
        #expect(viewer.isReadOnly)
        #expect(!caregiver.isReadOnly)
        #expect(!local.isReadOnly)
        #expect(!viewer.offersControls(now: start))
        #expect(caregiver.offersControls(now: start))
        #expect(local.offersControls(now: start))
    }

    /// The caption a card shows in every phase: typed and localized on this
    /// phone for pushed cards, the saved label for an older card.
    @Test func theCaptionIsTypedForPushedCardsAndSavedForOlderOnes() throws {
        let start = Date(timeIntervalSinceReferenceDate: 812_678_400)
        let legacy = TimerActivityContentState(startedAt: start, sideLabel: "Feeding · Left breast", activityKind: .breastLeft)
        #expect(legacy.lockScreenCaption == "Feeding · Left breast")
        let pushed = TimerActivityContentState(startedAt: start, sideLabel: "Linke Brust", activityKind: .formulaBottle,
                                               remote: try Self.remote(canWrite: true))
        #expect(pushed.lockScreenCaption == FeedActivityKind.formulaBottle.label)
        let legacyBreak = TimerActivityContentState(startedAt: start, sideLabel: "Pause", activityKind: .breastLeft, phase: .feedBreak)
        #expect(legacyBreak.lockScreenCaption == "Break")
        let awake = TimerActivityContentState(startedAt: start, sideLabel: "", activityKind: .sleep, phase: .sleepBreak)
        #expect(awake.lockScreenCaption == "Awake")
    }

    /// The server sends at most eight parts and counts the rest; the card
    /// says how many earlier parts it leaves out, adding any it drops for
    /// width.
    @Test func earlierPartsAreCountedOnTheCard() {
        let start = Date(timeIntervalSinceReferenceDate: 812_678_400)
        let parts = (0..<8).map { PartLabel(side: $0.isMultiple(of: 2) ? "left" : nil, seconds: 60 * $0) }
        let state = TimerActivityContentState(startedAt: start, sideLabel: "", activityKind: .breastLeft, phase: .feedBreak,
                                              parts: parts, omittedParts: 3)
        #expect(state.earlierParts(droppingForWidth: 0) == 3)
        #expect(state.earlierParts(droppingForWidth: 2) == 5)
        #expect(TimerActivityContentState.earlierPartsBadge(5) == "+5")
        #expect(TimerActivityContentState.earlierPartsBadge(0) == nil)
        #expect(TimerActivityContentState.earlierPartsAccessibilityLabel(5) == "Earlier parts: 5")
    }

    /// What the app writes on a card the server already pushed carries the
    /// acknowledged revision it was built from, and round-trips.
    @Test func anAppliedStateCarriesItsAcknowledgedRevision() throws {
        let revision = try Self.remote(canWrite: true)
        let state = TimerActivityContentState(startedAt: Date(timeIntervalSinceReferenceDate: 812_678_400), sideLabel: "",
                                              activityKind: .breastRight, remote: revision)
        let decoded = try JSONDecoder().decode(TimerActivityContentState.self, from: JSONEncoder().encode(state))
        #expect(decoded.remote == revision)
        #expect(decoded.remote?.presentationDate == Date(timeIntervalSince1970: 1_790_985_600))
    }
}
