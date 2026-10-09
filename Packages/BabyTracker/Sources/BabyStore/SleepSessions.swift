import Foundation
import SwiftData
import BabyCore

public struct SleepSession: Equatable, Sendable {
    public let id: UUID
    public let babyID: UUID
    public let sleepIDs: [UUID]
    public let startedAt: Date
    public let endedAt: Date?
    public let asleepSeconds: Int
}

public enum SleepSessionError: Error, Equatable { case missingSleep, alreadyOneSession, firstSleepCannotSeparate }

public extension BabyStore {
    /// Sessions whose first part started in `[start, end)`. A session formed
    /// by combining parts belongs to the day of its earliest part, not its
    /// latest, so combining a nap into an earlier session cannot move it to
    /// a different day's list.
    func sleepSessions(for babyID: UUID, from start: Date, to end: Date) throws -> [SleepSession] {
        Self.projectSleepSessions(try sessionSleeps(for: babyID))
            .filter { $0.startedAt >= start && $0.startedAt < end }
    }

    /// One local transaction; every changed member gets its ordinary sleep
    /// update mutation, and a running sleep follows its session, as for feeds.
    func combineSleeps(_ sleepIDs: [UUID], for babyID: UUID) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected) {
            let sleeps = try self.sessionSleeps(for: babyID)
            let selected = sleeps.filter { sleepIDs.contains($0.id) }
            guard selected.count == Set(sleepIDs).count, selected.count >= 2 else { throw SleepSessionError.missingSleep }
            let groups = Set(selected.map(\.effectiveSleepSessionID))
            guard groups.count > 1 else { throw SleepSessionError.alreadyOneSession }
            let affected = sleeps.filter { groups.contains($0.effectiveSleepSessionID) }
            let target = affected.min(by: Self.sleepOrder)!.effectiveSleepSessionID
            for sleep in affected where sleep.effectiveSleepSessionID != target {
                sleep.sleepSessionID = target
                sleep.lastModifiedAt = self.now
                try self.recordLocked(.update, on: sleep)
            }
            for group in groups where group != target {
                try self.relinkRunningPartLocked(.sleep, for: babyID, from: group, to: target)
            }
        }
    }

    /// Splits a group at a member, keeping the earlier members together; a
    /// running sleep that continued the group goes with the later half.
    func separateSleep(at sleepID: UUID, for babyID: UUID) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected) {
            let sleeps = try self.sessionSleeps(for: babyID)
            guard let selected = sleeps.first(where: { $0.id == sleepID }) else { throw SleepSessionError.missingSleep }
            let session = selected.effectiveSleepSessionID
            let group = sleeps.filter { $0.effectiveSleepSessionID == session }.sorted(by: Self.sleepOrder)
            guard let index = group.firstIndex(where: { $0.id == sleepID }), index > 0 else {
                throw SleepSessionError.firstSleepCannotSeparate
            }
            for sleep in group[index...] {
                let link: UUID? = sleep.id == sleepID ? nil : sleepID
                if sleep.sleepSessionID != link {
                    sleep.sleepSessionID = link
                    sleep.lastModifiedAt = self.now
                    try self.recordLocked(.update, on: sleep)
                }
            }
            try self.relinkRunningPartLocked(.sleep, for: babyID, from: session, to: sleepID)
        }
    }
}

extension BabyStore {
    func sessionSleeps(for babyID: UUID) throws -> [SleepEntry] {
        let realmKey = activeRealm.encodedKey
        return try context.fetch(FetchDescriptor<SleepEntry>(predicate: #Predicate {
            $0.babyID == babyID && $0.realmKey == realmKey && !$0.isTombstoned
        }))
    }

    static func sleepOrder(_ lhs: SleepEntry, _ rhs: SleepEntry) -> Bool {
        lhs.startedAt == rhs.startedAt ? lhs.id.uuidString < rhs.id.uuidString : lhs.startedAt < rhs.startedAt
    }

    static func projectSleepSessions(_ sleeps: [SleepEntry]) -> [SleepSession] {
        Dictionary(grouping: sleeps, by: \.effectiveSleepSessionID).compactMap { id, members in
            let ordered = members.sorted(by: sleepOrder)
            guard let first = ordered.first else { return nil }
            let open = ordered.contains { $0.endedAt == nil }
            return SleepSession(
                id: id, babyID: first.babyID, sleepIDs: ordered.map(\.id), startedAt: first.startedAt,
                endedAt: open ? nil : ordered.compactMap(\.endedAt).max(),
                asleepSeconds: ordered.compactMap(\.durationSeconds).reduce(0, +)
            )
        }.sorted { $0.startedAt == $1.startedAt ? $0.id.uuidString < $1.id.uuidString : $0.startedAt < $1.startedAt }
    }
}
