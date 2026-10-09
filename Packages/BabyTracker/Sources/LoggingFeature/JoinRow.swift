import SwiftUI
import Observation
import BabyCore
import BabyStore
import BabyUI

/// The Join row's strings.
public enum JoinRowCopy {
    /// "Join the 07:30 feed": the previous session's start, as History shows it.
    public static func joinFeed(at time: String) -> String { LoggingL10n.format("Join the %@ feed", time) }
    public static func joinSleep(at time: String) -> String { LoggingL10n.format("Join the %@ sleep", time) }
    /// "2h 8m before": from the previous session's end to this one's start.
    /// A time, not an age, so never "ago" or "just now".
    public static func before(_ gap: String) -> String { LoggingL10n.format("%@ before", gap) }
}

/// Offers to join a session to the newest earlier one of the same kind:
/// "Join the 07:30 feed · 2h 8m before". Solids are never offered, and with
/// no earlier session there is nothing to offer, so `previous` is nil and the
/// row is not shown (Review Focus 3).
///
/// Joining ignores the break limit and may cross a sleep (Home ruling R27);
/// Split undoes it, so it asks no confirmation. After one join the row hides
/// until its sheet opens again: it would otherwise offer the session before
/// that one in the same place, and a double tap would chain both joins
/// (final review I3).
@MainActor @Observable
public final class JoinRowModel {
    private let store: BabyStore
    private let babyID: UUID
    public let kind: SessionKind
    /// The session's own id, as the session sheet holds it, or — from
    /// `init(partOf:)` — one entry of the session.
    public let rootID: UUID
    /// Whether `rootID` is an entry's id rather than a session's. When one id
    /// is both — parts that continued a joined-away session keep its first
    /// part's id as their key (final review I2) — a session id means the
    /// session keyed by it and an entry id the session the entry is in.
    private let namesEntry: Bool

    /// Nil hides the row.
    public private(set) var previous: PreviousSession?
    /// True once this row has joined; it then offers nothing more.
    public private(set) var hasJoined: Bool
    public private(set) var label: String = ""
    public private(set) var gapText: String = ""
    /// A live member of this session, for the combine call, which takes
    /// entries rather than session ids.
    private var member: UUID?

    /// The row for the session `rootID` names, as the session sheet has it.
    public convenience init(store: BabyStore, babyID: UUID, kind: SessionKind, rootID: UUID) {
        self.init(store: store, babyID: babyID, kind: kind, id: rootID, namesEntry: false, hasJoined: false)
    }

    /// The session sheet's row for the session it follows to after a join:
    /// still spent.
    convenience init(store: BabyStore, babyID: UUID, kind: SessionKind, rootID: UUID, hasJoined: Bool) {
        self.init(store: store, babyID: babyID, kind: kind, id: rootID, namesEntry: false, hasJoined: hasJoined)
    }

    /// The row for the session `entryID` is part of, as a lone part's
    /// editor has it.
    public convenience init(store: BabyStore, babyID: UUID, kind: SessionKind, partOf entryID: UUID) {
        self.init(store: store, babyID: babyID, kind: kind, id: entryID, namesEntry: true, hasJoined: false)
    }

    private init(store: BabyStore, babyID: UUID, kind: SessionKind, id: UUID, namesEntry: Bool, hasJoined: Bool) {
        self.store = store
        self.babyID = babyID
        self.kind = kind
        self.rootID = id
        self.namesEntry = namesEntry
        self.hasJoined = hasJoined
        reload()
    }

    /// Re-reads both sessions; the sheet calls it on every `store.revision`.
    public func reload() {
        guard !hasJoined, let session = currentSession(),
              let previous = try? store.previousSession(kind, before: session.id, for: babyID) else {
            clear()
            return
        }
        self.previous = previous
        member = session.member
        let time = Self.clockTime(previous.startedAt, zone: zone(of: previous.rootID))
        label = kind == .feed ? JoinRowCopy.joinFeed(at: time) : JoinRowCopy.joinSleep(at: time)
        let gap = Int(session.startedAt.timeIntervalSince(previous.endedAt))
        gapText = JoinRowCopy.before(ElapsedFormatter.total(gap))
    }

    /// Links this session to the previous one: the previous session's last
    /// part and this session's first become one session, through the same
    /// combine calls History has always used.
    public func join() throws {
        guard let previous, let member else { return }
        switch kind {
        case .feed: try store.combineFeeds([previous.lastMemberID, member], for: babyID)
        case .sleep: try store.combineSleeps([previous.lastMemberID, member], for: babyID)
        }
        hasJoined = true
        reload()
    }

    private func clear() {
        previous = nil
        member = nil
        label = ""
        gapText = ""
    }

    /// This session's id, start and a live member of it: the session keyed
    /// by `rootID` or holding it, in the order `namesEntry` gives.
    private func currentSession() -> (id: UUID, startedAt: Date, member: UUID)? {
        switch kind {
        case .feed:
            let sessions = (try? store.feedingSessions(for: babyID, from: .distantPast, to: .distantFuture)) ?? []
            guard let session = find(in: sessions, id: \.id, members: \.feedIDs),
                  let first = session.feedIDs.first else { return nil }
            return (session.id, session.startedAt, first)
        case .sleep:
            let sessions = (try? store.sleepSessions(for: babyID, from: .distantPast, to: .distantFuture)) ?? []
            guard let session = find(in: sessions, id: \.id, members: \.sleepIDs),
                  let first = session.sleepIDs.first else { return nil }
            return (session.id, session.startedAt, first)
        }
    }

    private func find<Session>(in sessions: [Session], id: KeyPath<Session, UUID>,
                               members: KeyPath<Session, [UUID]>) -> Session? {
        let keyed = sessions.first { $0[keyPath: id] == rootID }
        let holding = sessions.first { $0[keyPath: members].contains(rootID) }
        return namesEntry ? holding ?? keyed : keyed ?? holding
    }

    /// The zone the previous session's first part was logged in, so the time
    /// reads as it does on that session's own row.
    private func zone(of entryID: UUID) -> String {
        switch try? store.entry(id: entryID, for: babyID) {
        case .feed(let feed)?: feed.timeZoneID
        case .sleep(let sleep)?: sleep.timeZoneID
        default: TimeZone.current.identifier
        }
    }

    static func clockTime(_ date: Date, zone: String) -> String {
        var style = Date.FormatStyle(date: .omitted, time: .shortened)
        style.timeZone = TimeZone(identifier: zone) ?? .gmt
        return date.formatted(style)
    }
}

/// The row itself: a link glyph, "Join the 07:30 feed", and the gap in grey
/// on the right. Draws nothing when there is no earlier session to join.
public struct JoinRow: View {
    private let model: JoinRowModel
    @Environment(\.errorReporter) private var errorReporter

    public init(model: JoinRowModel) {
        self.model = model
    }

    public var body: some View {
        if model.previous != nil {
            Button(action: join) {
                HStack(spacing: 10) {
                    Image(systemName: "link")
                        .foregroundStyle(model.kind == .feed ? Palette.feed : Palette.sleep)
                        .accessibilityHidden(true)
                    Text(model.label)
                        .foregroundStyle(Palette.primaryText)
                        .frame(maxWidth: .infinity, alignment: .leading)
                    Text(model.gapText)
                        .font(.subheadline)
                        .foregroundStyle(Palette.secondaryText)
                }
                .frame(minHeight: 44)
                .contentShape(.rect)
            }
            .buttonStyle(.plain)
            .accessibilityElement(children: .combine)
            .accessibilityIdentifier("joinRow")
        }
    }

    private func join() {
        do {
            try model.join()
            errorReporter.clear()
        } catch {
            let subject = model.kind == .feed ? ErrorSubject.feed : ErrorSubject.sleep
            errorReporter.report(ErrorBannerCopy.saveFailed(what: subject), error: error, category: AppLog.store)
        }
    }
}
