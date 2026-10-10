import Foundation
import BabyCore

public extension BabyStore {
    /// A completed sleep, entered after the fact. Live sleeps go through the
    /// timer API in Task 9.
    @discardableResult
    func logSleep(
        from start: Date,
        to end: Date?,
        for babyID: UUID,
        kindOverride: SleepKind? = nil,
        note: String? = nil,
        expectedWriteContext: RealmWriteContext? = nil
    ) throws -> SleepEntry {
        let expected = try expectedWriteContext ?? captureWriteContext()
        return try withRealmWrite(expected: expected) {
            let entry = self.sleepEntry(from: start, to: end, for: babyID)
            entry.kindOverride = kindOverride
            entry.note = note
            if let end {
                entry.sleepSessionID = try self.continuedSessionID(.sleep, for: babyID, startingAt: start)
            }
            self.context.insert(entry)
            try self.recordLocked(.create, on: entry)
            return entry
        }
    }
}

extension BabyStore {
    /// A sleep, not yet inserted or linked to a session — shared by
    /// `logSleep` and `addSleepPart`, which differ only in that.
    func sleepEntry(from start: Date, to end: Date?, for babyID: UUID) -> SleepEntry {
        let entry = SleepEntry(
            babyID: babyID,
            startedAt: start,
            timeZoneID: TimeZone.current.identifier,
            createdBy: deviceID,
            realm: activeRealm
        )
        entry.endedAt = end
        return entry
    }
}
