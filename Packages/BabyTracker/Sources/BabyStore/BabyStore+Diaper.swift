import Foundation
import BabyCore

public extension BabyStore {
    /// `at` defaults to now. Every logging call accepts a correction, because
    /// remembering a change forty minutes late is the normal case.
    ///
    /// The optional detail is written HERE, before the entry is recorded,
    /// rather than by the caller with a second `update`. Two writes meant a
    /// failed second one left a saved entry behind a banner saying the save
    /// failed — and the obvious response to that banner, tapping the same
    /// button again, logged a second diaper. One write either saves everything
    /// or saves nothing, and `record` already rolls back on failure.
    @discardableResult
    func logDiaper(
        _ contents: DiaperContents,
        for babyID: UUID,
        at date: Date? = nil,
        consistency: String? = nil,
        color: String? = nil,
        note: String? = nil,
        expectedWriteContext: RealmWriteContext? = nil
    ) throws -> DiaperEntry {
        let expected = try expectedWriteContext ?? captureWriteContext()
        return try withRealmWrite(expected: expected) {
            let entry = DiaperEntry(
                babyID: babyID,
                contents: contents,
                startedAt: date ?? self.now,
                timeZoneID: TimeZone.current.identifier,
                createdBy: self.deviceID,
                realm: self.activeRealm
            )
            entry.consistency = consistency
            entry.color = color
            entry.note = note
            self.context.insert(entry)
            try self.recordLocked(.create, on: entry)
            return entry
        }
    }
}
