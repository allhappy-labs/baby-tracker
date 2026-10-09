import Foundation
public import BabyProtocol

/// What a Lock Screen or Dynamic Island button acts on, as the card itself
/// knows it: the realm, baby and kind the card belongs to and — for a card
/// the server has pushed to — the lifecycle it shows and the family cursor
/// it was built at.
///
/// The button carries it explicitly. Without it an intent can only act on
/// whatever runs for the baby *now*, which after another caregiver's stop
/// and start, a family switch or a demotion is not what the card showed.
public struct LiveActivityActionTarget: Codable, Hashable, Sendable {
    public let scope: LiveActivityScope
    /// The server's logical activity generation. Nil for a token card no push
    /// has reached yet: it shows only what this phone wrote.
    public let generationID: UUID?
    /// The running timer the card shows; nil on a break card.
    public let timerID: UUID?
    /// The feed session whose break the card shows.
    public let sessionID: UUID?
    /// The family cursor the card's content was built at: the phone must have
    /// applied at least this much before it can vouch for the card.
    public let minimumFamilySequence: Int64

    public init(scope: LiveActivityScope, generationID: UUID?, timerID: UUID?, sessionID: UUID?, minimumFamilySequence: Int64) {
        self.scope = scope
        self.generationID = generationID
        self.timerID = timerID
        self.sessionID = sessionID
        self.minimumFamilySequence = max(0, minimumFamilySequence)
    }

    /// A card the server pushed to, or the app wrote with an acknowledged
    /// revision.
    public init(revision: LiveActivityRevision) {
        self.init(scope: revision.scope, generationID: revision.generationID, timerID: revision.timerID,
                  sessionID: revision.sessionID, minimumFamilySequence: revision.familySequence)
    }

    /// What a card's buttons carry: its revision when it has one, its realm
    /// scope alone for a token card no push has reached, and nothing for a
    /// card with no realm — local tracking, a self-hosted server, an earlier
    /// build — which keeps today's path.
    public static func card(scope: LiveActivityScope?, remote: LiveActivityRevision?) -> LiveActivityActionTarget? {
        if let remote { return LiveActivityActionTarget(revision: remote) }
        return scope.map { LiveActivityActionTarget(scope: $0, generationID: nil, timerID: nil, sessionID: nil, minimumFamilySequence: 0) }
    }

    /// Whether the card carries server content to verify before acting.
    public var isPushed: Bool { generationID != nil }

    /// Whether the store's current lifecycle for the card's baby and kind is
    /// the one the card shows: its running timer, or — on a break card — the
    /// break of its session with nothing running. The server's generation
    /// is one per such lifecycle (`Server/internal/liveactivity/lifecycle.go`,
    /// `sameLifecycle`), so this is the generation check too.
    public func names(_ lifecycle: LiveActivityReconciliation.Lifecycle) -> Bool {
        if let timerID { return lifecycle.timerID == timerID }
        guard let sessionID else { return false }
        return lifecycle.timerID == nil && lifecycle.sessionID == sessionID
    }

    // MARK: Intent parameter

    /// App Intent parameters cannot carry this type, so the button carries it
    /// as one string.
    public var parameterValue: String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        return (try? encoder.encode(self)).flatMap { String(data: $0, encoding: .utf8) } ?? ""
    }

    /// Nil for anything that does not read back as a whole, valid target.
    public init?(parameterValue: String?) {
        guard let parameterValue, let data = parameterValue.data(using: .utf8),
              let decoded = try? JSONDecoder().decode(Self.self, from: data) else { return nil }
        self = decoded
    }

    private enum CodingKeys: String, CodingKey {
        case scope, generationID, timerID, sessionID, minimumFamilySequence
    }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        let sequence = try container.decode(Int64.self, forKey: .minimumFamilySequence)
        guard sequence >= 0 else {
            throw DecodingError.dataCorruptedError(forKey: .minimumFamilySequence, in: container, debugDescription: "Negative cursor")
        }
        self.init(scope: try container.decode(LiveActivityScope.self, forKey: .scope),
                  generationID: try container.decodeIfPresent(UUID.self, forKey: .generationID),
                  timerID: try container.decodeIfPresent(UUID.self, forKey: .timerID),
                  sessionID: try container.decodeIfPresent(UUID.self, forKey: .sessionID),
                  minimumFamilySequence: sequence)
    }
}

/// Why a card action was handed to the app instead of being performed.
/// Persisted, never the action itself: nothing replays a stale tap.
public enum LiveActivityHandoffReason: String, Codable, Sendable, CaseIterable {
    /// The bounded catch-up did not get through: offline, refused, or out of time.
    case refreshFailed
    /// The card shows family state this phone has not applied.
    case cardAhead
    /// The realm or family session changed while the action was being checked.
    case sessionChanged
    /// The card's target could not be read or checked.
    case unverifiable
}

/// What a card action may do, decided against current family state.
public enum LiveActivityActionDecision: Equatable, Sendable {
    /// Act, inside this realm generation: the store's write boundary refuses
    /// the mutation if the realm moved since, and rechecks write authority.
    case ready(RealmWriteContext)
    /// What the card shows has already ended or been replaced: reconcile the
    /// card, write nothing.
    case alreadyFinished
    /// This phone may not write in the family now.
    case readOnly
    /// The card belongs to another family, server or recovery epoch.
    case wrongRealm
    /// Freshness or identity cannot be established here: write nothing and
    /// hand off to the app.
    case openApp(reason: LiveActivityHandoffReason)
}

/// The checks and the bounded network work around one card action. The
/// production gate is the background composition the App Intent builds; the
/// widget extension has none.
@MainActor
public protocol LiveActivityActionGate: AnyObject {
    func prepare(target: LiveActivityActionTarget, timeout: Duration) async -> LiveActivityActionDecision
    /// After an authorized mutation: one bounded sync to publish it, run after
    /// the intent has returned. Offline, the mutation stays in the ordinary
    /// durable outbox.
    func publish(timeout: Duration) async
    /// Stops every transport and renewal task the gate started. Called before
    /// the intent returns, or after the publish when there is one; later
    /// calls do nothing.
    func finish()
}
