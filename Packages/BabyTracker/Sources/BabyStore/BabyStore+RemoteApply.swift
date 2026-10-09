import BabyCore
import BabyProtocol
import Foundation
import SwiftData

/// Applying what the server sent.
///
/// The shape that matters is base-plus-overlay. The server owns the canonical
/// base; this device owns the mutations it has not yet had acknowledged. A
/// page from the server *replaces* the base and then the surviving overlay is
/// replayed on top — it is never merged into the visible row directly, because
/// that would let a confirmation of an old edit silently undo a newer one the
/// user has already seen on screen.
@MainActor
public protocol RemoteApplyStore {
    func applySyncPage(_ page: SyncResponse) throws
    func applySnapshotPage(_ page: SnapshotResponse) throws
    func finishSnapshot(snapshotID: UUID) throws
    func syncCursor(in realm: RealmID?) throws -> Cursor?
}

public extension BabyStore {
    /// Repairs a hidden local timer completion after its original ID has
    /// gained a live canonical base. The original sync page may have been
    /// consumed by an older app build, so there may be no journal page left
    /// to trigger rematerialization on this launch.
    @discardableResult
    func reconcileHiddenCanonicalCompletions() throws -> Int {
        guard realmIsShared else { return 0 }
        let expected = try captureWriteContext()
        return try withRealmWrite(expected: expected, allowsReadOnly: true) {
            let realmKey = activeRealm.encodedKey
            let deletedIDs = Set(try liveOutboxRows().filter { $0.operation == .delete }.map(\.entityID))
            let feeds = try context.fetch(FetchDescriptor<FeedEntry>(
                predicate: #Predicate { $0.realmKey == realmKey && $0.isTombstoned }
            ))
            let sleeps = try context.fetch(FetchDescriptor<SleepEntry>(
                predicate: #Predicate { $0.realmKey == realmKey && $0.isTombstoned }
            ))
            var restored = 0
            for key in feeds.map({ EntityKey(kind: WireEntityKind.feed.rawValue, id: $0.id) })
                + sleeps.map({ EntityKey(kind: WireEntityKind.sleep.rawValue, id: $0.id) }) {
                guard !deletedIDs.contains(key.id) else { continue }
                let storageKey = "\(realmKey)|\(key.kind)|\(key.id.uuidString)"
                guard let base = try context.fetch(FetchDescriptor<CanonicalEntityBase>(
                    predicate: #Predicate { $0.storageKey == storageKey }
                )).first,
                    (try? JSONSerialization.jsonObject(with: base.payload)) is [String: Any] else { continue }
                try rematerialize(key)
                restored += 1
            }
            return restored
        }
    }

    /// Applies one page of receipts and journal changes.
    ///
    /// The cursor does NOT advance while `next_page_token` is non-nil. A
    /// high-water mark is fixed for the whole response, so recording it after
    /// the first page would claim changes that have not been applied locally
    /// and every later page would be skipped forever.
    func applySyncPage(_ page: SyncResponse) throws {
        let expected = try captureWriteContext()
        try discardingTimerTransitionsOnFailure { try withRealmWrite(expected: expected, allowsReadOnly: true) {
            self.syncWriteScope?.appliedRemoteData = true
            var touched: Set<EntityKey> = []
            for outcome in page.outcomes {
                if let key = try self.applyOutcome(outcome) { touched.insert(key) }
            }
            for change in page.changes {
                if WireEntityKind(rawValue: change.entityKind) == nil, change.optional { continue }
                try self.applyCanonical(change.snapshotOrTombstone, entityID: change.entityID, version: change.entityVersion)
                touched.insert(EntityKey(kind: change.entityKind, id: change.entityID))
            }
            for key in touched { try self.rematerialize(key) }

            if page.nextPageToken == nil {
                try self.recordCursor(page.highWater)
            }
        } }
    }

    /// A snapshot page replaces bases wholesale. Used after a recovery epoch,
    /// where the server's history has been rebuilt and the client's cursor is
    /// meaningless.
    func applySnapshotPage(_ page: SnapshotResponse) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected, allowsReadOnly: true) {
            self.syncWriteScope?.appliedRemoteData = true
            for entity in page.entities {
                try self.applyCanonical(entity, entityID: entity.meta.id, version: entity.meta.entityVersion)
            }
        }
    }

    /// Closes a snapshot: the cursor moves to the snapshot's high water, and
    /// every entity is rematerialized from its new base plus whatever overlay
    /// survives.
    ///
    /// A mutation already acknowledged stays acknowledged. Re-dispatching it
    /// because it is absent from the snapshot would re-apply an edit the
    /// server deliberately dropped, which is how a recovery turns into data
    /// coming back from the dead.
    func finishSnapshot(snapshotID: UUID) throws {
        let expected = try captureWriteContext()
        try discardingTimerTransitionsOnFailure { try withRealmWrite(expected: expected, allowsReadOnly: true) {
            guard let pending = self.pendingSnapshot, pending.snapshotID == snapshotID else { return }
            self.syncWriteScope?.appliedRemoteData = true
            for key in pending.keys { try self.rematerialize(key) }
            try self.recordCursor(pending.highWater)
            self.pendingSnapshot = nil
        } }
    }

    func syncCursor(in realm: RealmID? = nil) throws -> Cursor? {
        let realmKey = (realm ?? activeRealm).encodedKey
        guard let row = try context.fetch(
            FetchDescriptor<SyncCursorState>(predicate: #Predicate { $0.realmKey == realmKey })
        ).first, row.sequence > 0 else { return nil }
        return Cursor(
            serverInstanceID: row.serverInstanceID,
            recoveryEpoch: row.recoveryEpoch,
            familyID: row.familyID,
            sequence: row.sequence
        )
    }

    /// The live mutation IDs still owed for one entity, in order.
    func pendingMutationIDs(for entityID: UUID) throws -> [UUID] {
        try liveOutboxRows()
            .filter { $0.entityID == entityID && !$0.deliveryState.isTerminal }
            .map(\.id)
    }
}

extension BabyStore {
    /// One receipt.
    ///
    /// `already_applied` is resolved through its original status for the same
    /// reason the timer reducer does: on its own it says nothing about whether
    /// the intent took effect.
    ///
    /// Returns a timer whose local state a refusal may have left behind the
    /// server's, so the caller rematerializes it: a refused switch or stop
    /// leaves the server's timer exactly as it was, and this phone must show
    /// that rather than its own unaccepted version.
    func applyOutcome(_ outcome: Outcome) throws -> EntityKey? {
        guard let row = try? outboxEntry(outcome.mutationID) else { return nil }
        let status = try outcome.requiredOriginalStatus
        let isGrowth = row.entityKind == .growthProfile || row.entityKind == .measurement
        if isGrowth, let canonical = outcome.canonicalEntity {
            guard let kind = WireEntityKind(rawValue: canonical.entityKind) else {
                throw RemoteEntityError.unsupportedKind(canonical.entityKind)
            }
            // Retried mutations return their original persisted receipt. A
            // later snapshot may already hold a newer edit or tombstone.
            // Settle the outbox below, but never replace that newer base.
            let currentVersion = try observedVersion(of: canonical.meta.id, kind: kind)
            if canonical.meta.entityVersion >= currentVersion {
                try applyCanonical(canonical, entityID: canonical.meta.id, version: canonical.meta.entityVersion)
            }
        }
        switch status {
        case .applied:
            row.deliveryState = .acknowledged
            try settleAppliedShortFeedCleanup(row, outcome: outcome)
            // Anything waiting on this may now go.
            for dependent in try liveOutboxRows() where dependent.dependencyMutationID == row.id {
                dependent.dependencyMutationID = nil
            }
            return isGrowth ? EntityKey(kind: row.entityKind.rawValue, id: row.entityID) : nil
        case .superseded, .rejected:
            row.deliveryState = .superseded
            row.supersededCode = outcome.code
            try settleRefusedTimerCommand(row, outcome: outcome)
            for dependent in try liveOutboxRows() where dependent.dependencyMutationID == row.id {
                dependent.deliveryState = .superseded
                dependent.supersededCode = outcome.code
                if row.operation == .timerStart, status == .superseded,
                   outcome.canonicalEntity?.meta.id != row.entityID,
                   dependent.operation == .timerStop {
                    try preserveCompletionOfSupersededStart(dependent)
                } else {
                    try settleRefusedTimerCommand(dependent, outcome: nil)
                }
            }
            if isGrowth {
                if outcome.code == "baby_deleted" {
                    // This device may have created the row while offline after
                    // the server deleted its child. There is no server row to
                    // cascade, so retain its raw local history as a tombstone.
                    let version = try observedVersion(of: row.entityID, kind: row.entityKind)
                    try replaceBase(entityID: row.entityID, kind: row.entityKind, version: version,
                                    payload: JSONEncoder().encode(JSONValue.null))
                    try applyTombstone(entityID: row.entityID, kind: row.entityKind)
                }
                return EntityKey(kind: row.entityKind.rawValue, id: row.entityID)
            }
            guard row.entityKind == .timer else { return nil }
            return EntityKey(kind: WireEntityKind.timer.rawValue, id: row.entityID)
        case .alreadyApplied:
            throw SyncPersistenceError.unstableTimerReceipt
        }
    }

    /// Replaces one entity's canonical base, and records a tombstone as the
    /// base rather than deleting the row outright — the overlay may still have
    /// something to say about it.
    func applyCanonical(_ entity: CanonicalEntity, entityID: UUID, version: Int64) throws {
        guard let kind = WireEntityKind(rawValue: entity.entityKind) else { throw RemoteEntityError.unsupportedKind(entity.entityKind) }
        let payload: Data
        if entity.meta.tombstonedAt != nil {
            payload = try JSONEncoder().encode(JSONValue.null)
        } else if let value = entity.payload {
            payload = try JSONEncoder().encode(value)
        } else {
            payload = try JSONEncoder().encode(JSONValue.null)
        }
        if kind == .supplement, entity.meta.tombstonedAt == nil {
            guard case let .object(fields) = entity.payload,
                  case let .string(loggedAt) = fields["logged_at"] else {
                throw RemoteEntityError.invalidSupplementPayload
            }
            _ = try SupplementTimestamp.parse(loggedAt)
        }
        if entity.meta.tombstonedAt == nil {
            if kind == .growthProfile {
                try JSONDecoder().decode(GrowthProfilePayloadV1.self, from: payload).validate(entityID: entityID)
            } else if kind == .measurement {
                _ = try JSONDecoder().decode(MeasurementPayloadV1.self, from: payload).validatedValues()
            }
        }
        try replaceBase(entityID: entityID, kind: kind, version: version, payload: payload)
        if pendingSnapshot != nil {
            pendingSnapshot?.keys.insert(EntityKey(kind: kind.rawValue, id: entityID))
        }
        if entity.meta.tombstonedAt != nil {
            try applyTombstone(entityID: entityID, kind: kind)
        } else {
            let fields = (try? JSONSerialization.jsonObject(with: payload)) as? [String: Any] ?? [:]
            let realmKey = activeRealm.encodedKey
            try insertRowIfUnseen(fields, entityID: entityID, kind: kind)
            if kind == .growthProfile, let row = try context.fetch(FetchDescriptor<GrowthProfile>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).first {
                row.lastModifiedAt = try Self.wireDate(entity.meta.acceptedAt)
            }
            if kind == .measurement, let row = try context.fetch(FetchDescriptor<MeasurementCheckIn>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).first {
                row.lastModifiedAt = try Self.wireDate(entity.meta.acceptedAt)
                row.createdBy = entity.meta.creatorAccountID.uuidString.lowercased()
            }
            if kind == .supplement, let row = try context.fetch(FetchDescriptor<SupplementEntry>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).first {
                row.lastModifiedAt = try Self.wireDate(entity.meta.acceptedAt)
                row.createdBy = entity.meta.creatorAccountID.uuidString.lowercased()
            }
        }
    }

    /// Creates the visible row for an entity this device has never had.
    ///
    /// Without this, `writeFields` only ever updated rows that already
    /// existed, so a record another caregiver created simply never appeared.
    /// Only the columns a row cannot exist without are set here; the
    /// rematerialize that follows fills in everything else from the same
    /// base, through the one field-mapping path. A deleted entity is never
    /// created — there is nothing to show — and neither is one whose payload
    /// lacks a required column.
    func insertRowIfUnseen(_ fields: [String: Any], entityID: UUID, kind: WireEntityKind) throws {
        let realm = activeRealm
        // Scoped to the realm being written: the same UUID in another realm
        // is a different record and must neither block this one nor be it.
        let realmKey = realm.encodedKey
        func uuid(_ key: String) -> UUID? { (fields[key] as? String).flatMap(UUID.init(uuidString:)) }
        func string(_ key: String) -> String? { fields[key] as? String }
        func date(_ key: String) -> Date? { (fields[key] as? String).flatMap { try? Self.wireDate($0) } }

        switch kind {
        case .baby:
            guard try context.fetch(FetchDescriptor<Baby>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).isEmpty,
                  let name = string("name"),
                  let birthDate = string("birth_date").flatMap(Self.wireBirthDate) else { return }
            context.insert(Baby(id: entityID, name: name, birthDate: birthDate, realm: realm))
        case .feed:
            guard try context.fetch(FetchDescriptor<FeedEntry>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).isEmpty,
                  let babyID = uuid("baby_id"),
                  let startedAt = date("started_at"),
                  let timeZoneID = string("time_zone_id") else { return }
            let kind = string("kind").flatMap(FeedKind.init(rawValue:)) ?? .breast
            context.insert(FeedEntry(
                id: entityID, babyID: babyID, kind: kind, startedAt: startedAt, timeZoneID: timeZoneID, realm: realm
            ))
        case .sleep:
            guard try context.fetch(FetchDescriptor<SleepEntry>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).isEmpty,
                  let babyID = uuid("baby_id"),
                  let startedAt = date("started_at"),
                  let timeZoneID = string("time_zone_id") else { return }
            context.insert(SleepEntry(id: entityID, babyID: babyID, startedAt: startedAt, timeZoneID: timeZoneID, realm: realm))
        case .growthProfile:
            guard try context.fetch(FetchDescriptor<GrowthProfile>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).isEmpty else { return }
            let payload = try JSONDecoder().decode(GrowthProfilePayloadV1.self, from: JSONSerialization.data(withJSONObject: fields))
            try payload.validate(entityID: entityID)
            context.insert(GrowthProfile(babyID: payload.babyID, referenceSex: payload.referenceSex,
                expectedDueDate: try payload.expectedDueDate.map(LocalDate.init), modifiedAt: now, realm: realm))
        case .measurement:
            guard try context.fetch(FetchDescriptor<MeasurementCheckIn>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).isEmpty else { return }
            let payload = try JSONDecoder().decode(MeasurementPayloadV1.self, from: JSONSerialization.data(withJSONObject: fields))
            context.insert(MeasurementCheckIn(id: entityID, babyID: payload.babyID,
                measuredAt: try SupplementTimestamp.parse(payload.measuredAt), timeZoneID: payload.timeZoneID,
                loggedAt: try SupplementTimestamp.parse(payload.loggedAt), values: try payload.validatedValues(), realm: realm))
        case .supplement:
            guard try context.fetch(FetchDescriptor<SupplementEntry>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).isEmpty else { return }
            guard let babyID = uuid("baby_id"), let startedAt = date("started_at"),
                  let name = string("name"), let zone = string("time_zone_id"),
                  let loggedAtString = string("logged_at"),
                  let loggedAt = try? SupplementTimestamp.parse(loggedAtString) else { throw RemoteEntityError.invalidSupplementPayload }
            context.insert(SupplementEntry(id: entityID, babyID: babyID, name: try SupplementName.normalized(name),
                startedAt: startedAt, timeZoneID: zone, loggedAt: loggedAt, realm: realm))
        case .diaper:
            guard try context.fetch(FetchDescriptor<DiaperEntry>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).isEmpty,
                  let babyID = uuid("baby_id"),
                  let startedAt = date("started_at"),
                  let timeZoneID = string("time_zone_id"),
                  let contents = string("contents").flatMap(DiaperContents.init(rawValue:)) else { return }
            context.insert(DiaperEntry(
                id: entityID, babyID: babyID, contents: contents, startedAt: startedAt, timeZoneID: timeZoneID, realm: realm
            ))
        case .timer:
            // A timer's row is created, updated and ended in one place,
            // `rematerializeTimer`, which runs for every touched timer.
            return
        }
    }

    /// The server's `birth_date` is a calendar date, not an instant.
    private static func wireBirthDate(_ value: String) -> Date? {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = .current
        let parts = value.split(separator: "-").compactMap { Int($0) }
        guard parts.count == 3 else { return nil }
        return calendar.date(from: DateComponents(year: parts[0], month: parts[1], day: parts[2], hour: 12))
    }

    /// Recomputes a visible row from its base and the overlay that survives.
    ///
    /// This is the step that stops an old confirmation regressing a newer local
    /// edit: the base becomes what the server said, then every mutation this
    /// device still owes is replayed over it in order, so the value on screen
    /// is the one the user last chose.
    func rematerialize(_ key: EntityKey) throws {
        guard let kind = WireEntityKind(rawValue: key.kind) else { return }
        let storageKey = "\(activeRealm.encodedKey)|\(kind.rawValue)|\(key.id.uuidString)"
        guard let base = try context.fetch(
            FetchDescriptor<CanonicalEntityBase>(predicate: #Predicate { $0.storageKey == storageKey })
        ).first else { return }

        if kind == .timer {
            try rematerializeTimer(key.id, basePayload: base.payload)
            return
        }
        let liveFields = (try? JSONSerialization.jsonObject(with: base.payload)) as? [String: Any]
        var fields = liveFields ?? [:]
        // A live canonical row can arrive for a completion this phone hid
        // after its timer command was refused. Its wire payload has no local
        // tombstone flag; clear the local flag before replaying any still-owed
        // delete overlay below. A canonical tombstone has a null payload.
        if liveFields != nil { fields["isTombstoned"] = false }
        for row in try liveOutboxRows() where row.entityID == key.id && row.entityKind == kind && !row.deliveryState.isTerminal {
            // A row this build cannot send — an older build's, in a shape the
            // server no longer takes — has no overlay. The next dispatch
            // quarantines it; failing here would fail every sync page.
            guard let mutation = try? row.dispatchableMutation() else { continue }
            // A delete's wire payload names only the parent baby, so the
            // tombstone comes from the operation, not from a payload field.
            if mutation.operation == .delete {
                fields["isTombstoned"] = true
                continue
            }
            let overlay = try JSONSerialization.jsonObject(with: JSONEncoder().encode(mutation.payload))
            if let overlay = overlay as? [String: Any] {
                fields.merge(overlay) { _, newer in newer }
            }
        }
        try writeFields(fields, entityID: key.id, kind: kind)
    }

    /// Writes materialized fields onto the visible row.
    ///
    /// Only the columns the wire actually carries are touched. `photoData` and
    /// `liveActivityID` have no protocol projection and are never overwritten
    /// by a remote page — losing a baby's photo to a sync would be a poor
    /// trade for a schema shortcut.
    func writeFields(_ fields: [String: Any], entityID: UUID, kind: WireEntityKind) throws {
        // Only the active realm's row. A row with the same ID in another realm
        // — the local realm a joiner kept — is not the family's to overwrite.
        let realmKey = activeRealm.encodedKey
        // Wire payloads carry every optional column, null when empty. Absent
        // means "not in this payload, leave it"; present-and-null means the
        // value was cleared and must be cleared here too.
        func optionalString(_ key: String) -> String?? {
            guard let value = fields[key] else { return .none }
            return .some(value as? String)
        }
        func optionalDate(_ key: String) throws -> Date?? {
            guard let value = optionalString(key) else { return .none }
            return .some(try value.map(Self.wireDate))
        }
        switch kind {
        case .feed:
            guard let row = try context.fetch(
                FetchDescriptor<FeedEntry>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })
            ).first else { return }
            // Parts arrive only as `components`, in the current shape.
            if fields["components"] != nil {
                row.components = try WireFeedComponent.decodeComponents("components", of: MutationFactory.jsonValue(fields))
                    .map(Self.localComponent)
            }
            // Present-and-null clears, as for sleep and diaper: a note taken
            // off on one phone must come off on the other.
            if let value = optionalString("note") { row.note = value }
            if let value = optionalString("feeding_session_id") { row.feedingSessionID = value.flatMap(UUID.init(uuidString:)) }
            if let value = fields["kind"] as? String { row.kindRaw = value }
            if let value = fields["started_at"] as? String { row.startedAt = try Self.wireDate(value) }
            if let value = try optionalDate("ended_at") { row.endedAt = value }
            if let value = fields["isTombstoned"] as? Bool { row.isTombstoned = value }
        case .sleep:
            guard let row = try context.fetch(
                FetchDescriptor<SleepEntry>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })
            ).first else { return }
            if let value = fields["started_at"] as? String { row.startedAt = try Self.wireDate(value) }
            if let value = try optionalDate("ended_at") { row.endedAt = value }
            if let value = fields["time_zone_id"] as? String { row.timeZoneID = value }
            if let value = optionalString("note") { row.note = value }
            if let value = optionalString("kind_override") { row.kindOverrideRaw = value }
            if let value = optionalString("sleep_session_id") { row.sleepSessionID = value.flatMap(UUID.init(uuidString:)) }
            if let value = fields["isTombstoned"] as? Bool { row.isTombstoned = value }
        case .growthProfile:
            guard let row = try context.fetch(FetchDescriptor<GrowthProfile>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).first else { return }
            if let value = fields["reference_sex"] as? String { row.referenceSexRaw = value }
            if let value = optionalString("expected_due_date") { row.expectedDueDateISO = value }
            if let value = fields["isTombstoned"] as? Bool { row.isTombstoned = value }
        case .measurement:
            guard let row = try context.fetch(FetchDescriptor<MeasurementCheckIn>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).first else { return }
            if let value = fields["measured_at"] as? String { row.measuredAt = try SupplementTimestamp.parse(value) }
            if let value = fields["logged_at"] as? String { row.loggedAt = try SupplementTimestamp.parse(value) }
            if let value = fields["time_zone_id"] as? String { row.timeZoneID = value }
            if fields["weight_kg"] != nil { row.weightKG = fields["weight_kg"] as? Double }
            if fields["length_cm"] != nil { row.lengthCM = fields["length_cm"] as? Double }
            if fields["head_circumference_cm"] != nil { row.headCircumferenceCM = fields["head_circumference_cm"] as? Double }
            if let value = optionalString("length_position") { row.lengthPositionRaw = value }
            if let value = fields["isTombstoned"] as? Bool { row.isTombstoned = value }
        case .supplement:
            guard let row = try context.fetch(FetchDescriptor<SupplementEntry>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })).first else { return }
            if let name = fields["name"] as? String { row.name = try SupplementName.normalized(name) }
            if let value = fields["logged_at"] as? String { row.loggedAt = try SupplementTimestamp.parse(value) }
            if let value = fields["started_at"] as? String { row.startedAt = try Self.wireDate(value) }
            if let value = fields["time_zone_id"] as? String { row.timeZoneID = value }
            if let value = optionalString("note") { row.note = value }
            if let value = fields["isTombstoned"] as? Bool { row.isTombstoned = value }
        case .diaper:
            guard let row = try context.fetch(
                FetchDescriptor<DiaperEntry>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })
            ).first else { return }
            if let value = fields["contents"] as? String { row.contentsRaw = value }
            if let value = optionalString("consistency") { row.consistency = value }
            if let value = optionalString("color") { row.color = value }
            if let value = optionalString("note") { row.note = value }
            if let value = fields["started_at"] as? String { row.startedAt = try Self.wireDate(value) }
            if let value = try optionalDate("ended_at") { row.endedAt = value }
            if let value = fields["time_zone_id"] as? String { row.timeZoneID = value }
            if let value = fields["isTombstoned"] as? Bool { row.isTombstoned = value }
        case .baby:
            guard let row = try context.fetch(
                FetchDescriptor<Baby>(predicate: #Predicate { $0.id == entityID && $0.realmKey == realmKey })
            ).first else { return }
            if let value = fields["name"] as? String { row.name = value }
            if let value = fields["birth_date"] as? String, let birthDate = Self.wireBirthDate(value) {
                row.birthDate = birthDate
                row.birthDateISO = value
            }
            if let value = fields["sort_order"] as? Int { row.sortOrder = value }
            if let value = fields["icon"] as? String { row.iconID = value }
            if let value = fields["isTombstoned"] as? Bool { row.isTombstoned = value }
        case .timer:
            // Never a field merge: see `rematerializeTimer`.
            return
        }
    }

    /// One wire component as the local model holds it. The wire decoder has
    /// already checked each kind carries its own fields.
    nonisolated static func localComponent(_ component: WireFeedComponent) throws -> FeedComponent {
        let start = try component.startedAt.map(wireDate)
        let end = try component.endedAt.map(wireDate)
        return switch component.kind {
        case .breast:
            .breast(id: component.id, side: component.side!, seconds: component.seconds!, startedAt: start, endedAt: end)
        case .bottle:
            .bottle(
                id: component.id,
                volumeML: NSDecimalNumber(decimal: component.volumeML!).doubleValue,
                contents: component.contents!,
                seconds: component.seconds,
                startedAt: start,
                endedAt: end
            )
        case .solids:
            .solids(id: component.id, foods: component.foods!, amount: component.amount!)
        }
    }

    nonisolated static func wireDate(_ value: String) throws -> Date {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let date = formatter.date(from: value) { return date }
        formatter.formatOptions = [.withInternetDateTime]
        guard let date = formatter.date(from: value) else {
            throw FeedComponentWireError.invalidShape("timestamp")
        }
        return date
    }

    func applyTombstone(entityID: UUID, kind: WireEntityKind) throws {
        try writeFields(["isTombstoned": true], entityID: entityID, kind: kind)
    }

    /// Records the durable cursor for the active realm.
    func recordCursor(_ cursor: Cursor) throws {
        let realmKey = activeRealm.encodedKey
        if let row = try context.fetch(
            FetchDescriptor<SyncCursorState>(predicate: #Predicate { $0.realmKey == realmKey })
        ).first {
            // A cursor never moves backwards except across a recovery epoch,
            // where the epoch itself is what changed.
            guard cursor.recoveryEpoch > row.recoveryEpoch || cursor.sequence >= row.sequence else { return }
            row.sequence = cursor.sequence
            row.recoveryEpoch = cursor.recoveryEpoch
            row.serverInstanceID = cursor.serverInstanceID
            row.familyID = cursor.familyID
            return
        }
        context.insert(SyncCursorState(
            realm: activeRealm,
            sequence: cursor.sequence,
            recoveryEpoch: cursor.recoveryEpoch,
            serverInstanceID: cursor.serverInstanceID,
            familyID: cursor.familyID
        ))
    }

}

public extension BabyStore {
    /// Begins a snapshot, so its pages accumulate and are committed together.
    func beginSnapshot(snapshotID: UUID, highWater: Cursor) {
        pendingSnapshot = PendingSnapshot(snapshotID: snapshotID, highWater: highWater, keys: [])
    }

    var pendingSnapshotID: UUID? { pendingSnapshot?.snapshotID }

    /// Takes the head of the dispatch queue out of circulation.
    ///
    /// One mutation the server will always refuse must not block every later
    /// one behind it. Quarantined rather than deleted: the intent the user
    /// expressed is still on the device, and a future version may be able to
    /// make sense of it.
    func quarantineNextDispatch(limit: Int) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected, allowsReadOnly: true) {
            guard let head = try self.liveOutboxRows().first(where: { !$0.deliveryState.isTerminal }) else { return }
            head.deliveryState = .quarantined
            for dependent in try self.liveOutboxRows() where dependent.dependencyMutationID == head.id {
                dependent.deliveryState = .quarantined
            }
        }
    }
}

/// A snapshot in progress. Held in memory: a snapshot that does not finish is
/// simply restarted, and half its pages are worthless without the rest.
struct PendingSnapshot {
    let snapshotID: UUID
    let highWater: Cursor
    var keys: Set<EntityKey>
}

extension BabyStore: RemoteApplyStore {}

public enum RemoteEntityError: Error, Sendable {
    case unsupportedKind(String)
    case invalidSupplementPayload
}
