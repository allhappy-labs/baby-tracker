import CryptoKit
import Foundation

/// The server's content hash (`CanonicalContentHash` in
/// `Server/internal/protocol/canonical.go`), ported exactly.
///
/// An import manifest promises each record's content by this hash, and the
/// server recomputes it from the imported mutation and refuses the record as
/// `import_manifest_mismatch` when the two differ. A hash of this device's own
/// JSON bytes can never match: the server hashes a per-kind projection with
/// fixed keys, normalized text and times, and `baby_id` left out.
///
/// The payload is projected exactly as `canonicalPayloadValue(payload, false)`
/// does it: every field the server's struct has is present (null when absent),
/// nothing else is, text is NFC, timestamps are UTC microseconds, and decimals
/// are six-place canonical. There is one projection per kind, its current
/// shape, and the envelope's `payload_version` is always 1: the server hashes
/// content, not the wire version it arrived in.
public extension ProtocolCanonicalizer {
    static func canonicalContentBytes(entityKind: String, payload: JSONValue) throws -> Data {
        let value: JSONValue = .object([
            "entity_kind": .string(entityKind),
            "payload": try contentPayload(entityKind: entityKind, payload: payload),
            "payload_version": .number(1),
        ])
        return Data(try canonicalJSON(value).utf8)
    }

    static func contentSHA256(entityKind: String, payload: JSONValue) throws -> String {
        var input = Data("baby-tracker-content-v1\0".utf8)
        input.append(try canonicalContentBytes(entityKind: entityKind, payload: payload))
        return SHA256.hash(data: input).map { String(format: "%02x", $0) }.joined()
    }
}

extension ProtocolCanonicalizer {
    static func contentPayload(entityKind: String, payload: JSONValue) throws -> JSONValue {
        guard case let .object(fields) = payload else { throw Error.invalidJSON }
        let p = ContentFields(fields)
        switch entityKind {
        case "baby":
            var object: [String: JSONValue] = [
                "birth_date": p.text("birth_date"),
                "name": .string(displayName(p.string("name") ?? "")),
                "sort_order": p.integer("sort_order"),
            ]
            // Mirrors the server: present only when set, so existing hashes hold.
            if p.string("icon") != nil { object["icon"] = p.text("icon") }
            return .object(object)
        case "feed":
            return .object([
                "components": try p.components("components", timed: true),
                "ended_at": p.optionalTime("ended_at"),
                "feeding_session_id": p.optionalText("feeding_session_id"),
                "kind": p.raw("kind"),
                "note": p.optionalText("note"),
                "started_at": p.time("started_at"),
                "time_zone_id": p.text("time_zone_id"),
            ])
        case "sleep":
            return .object([
                "ended_at": p.optionalTime("ended_at"),
                "kind_override": p.optionalText("kind_override"),
                "note": p.optionalText("note"),
                "sleep_session_id": p.optionalText("sleep_session_id"),
                "started_at": p.time("started_at"),
                "time_zone_id": p.text("time_zone_id"),
            ])
        case "growth_profile":
            return .object([
                "reference_sex": p.raw("reference_sex"),
                "expected_due_date": p.optionalText("expected_due_date"),
            ])
        case "measurement":
            return .object([
                "measured_at": p.time("measured_at"), "logged_at": p.time("logged_at"),
                "time_zone_id": p.text("time_zone_id"), "weight_kg": p.optionalNumber("weight_kg"),
                "length_cm": p.optionalNumber("length_cm"), "head_circumference_cm": p.optionalNumber("head_circumference_cm"),
                "length_position": p.optionalText("length_position"),
            ])
        case "immunization":
            return .object([
                "dose": p.optionalNumber("dose"),
                "given_at": p.time("given_at"),
                "note": p.optionalText("note"),
                "time_zone_id": p.text("time_zone_id"),
                "vaccine": .string(displayName(p.string("vaccine") ?? "")),
            ])
        case "pump":
            return .object([
                "ended_at": p.optionalTime("ended_at"),
                "note": p.optionalText("note"),
                "side": p.text("side"),
                "started_at": p.time("started_at"),
                "time_zone_id": p.text("time_zone_id"),
                "volume_ml": p.optionalNumber("volume_ml"),
            ])
        case "supplement":
            return .object([
                "logged_at": p.time("logged_at"),
                "name": .string(displayName(p.string("name") ?? "")),
                "note": p.optionalText("note"),
                "started_at": p.time("started_at"),
                "time_zone_id": p.text("time_zone_id"),
            ])
        case "diaper":
            return .object([
                "color": p.optionalText("color"),
                "consistency": p.optionalText("consistency"),
                // Not normalized by the server: a closed vocabulary.
                "contents": p.raw("contents"),
                "ended_at": p.optionalTime("ended_at"),
                "note": p.optionalText("note"),
                "started_at": p.time("started_at"),
                "time_zone_id": p.text("time_zone_id"),
            ])
        case "timer":
            return .object([
                "completed_components": try p.components("completed_components", timed: true),
                "current_bottle_started_at": p.optionalTime("current_bottle_started_at"),
                "current_side": p.optionalText("current_side"),
                "current_side_started_at": p.optionalTime("current_side_started_at"),
                "session_id": p.optionalText("session_id"),
                "started_at": p.time("started_at"),
                "time_zone_id": p.text("time_zone_id"),
                "timer_kind": p.raw("timer_kind"),
            ])
        default:
            throw Error.invalidJSON
        }
    }

    /// `canonicalPayloadValue(payload, true)`: the payload as the receipt
    /// hash covers it. The payload type follows from the kind and the
    /// operation, as `decodeMutationPayload` picks it; a record's projection
    /// is its content projection plus `baby_id`. Fields are derived, not
    /// copied, so a part's absent `seconds` or step bounds hash as null
    /// exactly as the server's typed structs write them.
    static func mutationPayload(entityKind: WireEntityKind, operation: WireOperation, payload: JSONValue) throws -> JSONValue {
        guard case let .object(fields) = payload else { throw Error.invalidJSON }
        let p = ContentFields(fields)
        func withBaby(_ projection: JSONValue) throws -> JSONValue {
            guard case var .object(result) = projection else { throw Error.invalidJSON }
            result["baby_id"] = p.raw("baby_id")
            return .object(result)
        }
        switch (entityKind, operation) {
        case (.timer, .timerStart):
            return try withBaby(contentPayload(entityKind: entityKind.rawValue, payload: payload))
        case (.timer, .timerFinishBottle):
            return try withBaby(.object([
                "component_id": p.raw("component_id"),
                "contents": p.optionalText("contents"),
                "finished_at": p.time("finished_at"),
                "volume_ml": p.optionalNumber("volume_ml"),
            ]))
        case (.timer, .timerStop):
            var result: [String: JSONValue] = [
                "completed_record_id": p.raw("completed_record_id"),
                "note": p.optionalText("note"),
                "sleep_kind_override": p.optionalText("sleep_kind_override"),
                "stopped_at": p.time("stopped_at"),
            ]
            if let component = p.string("component_id"), !component.isEmpty { result["component_id"] = .string(component) }
            if fields["cleanup_short_breast_segments"] == .bool(true) { result["cleanup_short_breast_segments"] = .bool(true) }
            return try withBaby(.object(result))
        case (.timer, .update):
            // A timer edit's parts hash without bottle seconds or step bounds.
            return try withBaby(.object([
                "components": try p.components("components", timed: false),
                "session_id": p.optionalText("session_id"),
                "started_at": p.optionalTime("started_at"),
            ]))
        case (.timer, _), (_, .timerStart), (_, .timerFinishBottle), (_, .timerStop):
            // No such payload: the server refuses the mutation before hashing it.
            throw Error.invalidJSON
        case (_, .delete):
            // `DeletePayload`: `baby_id` when the payload names one, which
            // every delete but a baby's does.
            return .object(p.string("baby_id").map { ["baby_id": .string($0)] } ?? [:])
        case (.baby, .create), (.baby, .update), (.pump, .create), (.pump, .update):
            return try contentPayload(entityKind: entityKind.rawValue, payload: payload)
        case (_, .create), (_, .update):
            return try withBaby(contentPayload(entityKind: entityKind.rawValue, payload: payload))
        }
    }

    /// `NormalizeDisplayName`: NFC, then Unicode 15 white space trimmed. A
    /// name that trims to nothing falls back to plain NFC, as the server's
    /// hash does.
    static func displayName(_ raw: String) -> String {
        let nfc = raw.precomposedStringWithCanonicalMapping
        let scalars = Array(nfc.unicodeScalars)
        guard let first = scalars.firstIndex(where: { !isUnicode15WhiteSpace($0) }),
              let last = scalars.lastIndex(where: { !isUnicode15WhiteSpace($0) }) else { return nfc }
        var trimmed = String.UnicodeScalarView()
        trimmed.append(contentsOf: scalars[first...last])
        return String(trimmed)
    }

    /// `isUnicode15WhiteSpace` in `Server/internal/protocol/normalization.go`.
    static func isUnicode15WhiteSpace(_ scalar: Unicode.Scalar) -> Bool {
        switch scalar.value {
        case 0x09...0x0d, 0x20, 0x85, 0xa0, 0x1680, 0x2000...0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000: true
        default: false
        }
    }

    /// `normalizeTimestampForHash`: a parseable RFC 3339 time becomes UTC
    /// microseconds (ties to even) with no range check; anything else is
    /// hashed as NFC text.
    static func hashTimestamp(_ raw: String) -> String {
        (try? lexicalTimestamp(raw).canonical) ?? raw.precomposedStringWithCanonicalMapping
    }

    /// `normalizeTimestampLexical`: the server's accepted spelling — one to
    /// nine fraction digits, `Z` or a `±HH:MM` offset of at most 14 hours —
    /// converted to UTC and rounded to the microsecond, ties to even.
    ///
    /// Anchored with `\z`, not `$`: ICU's `$` also matches before a trailing
    /// newline, and Go's RE2 does not, so a value with one would otherwise be
    /// hashed as a time here and as text on the server.
    static func lexicalTimestamp(_ raw: String) throws -> (canonical: String, year: Int) {
        let pattern = #"^([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\.([0-9]{1,9}))?(Z|([+-])(0[0-9]|1[0-4]):([0-5][0-9]))\z"#
        let regex = try NSRegularExpression(pattern: pattern)
        let range = NSRange(raw.startIndex..<raw.endIndex, in: raw)
        guard let match = regex.firstMatch(in: raw, range: range),
              let prefixRange = Range(match.range(at: 1), in: raw) else { throw Error.invalidTimestamp(raw) }
        func group(_ index: Int) -> String? { Range(match.range(at: index), in: raw).map { String(raw[$0]) } }
        let formatter = DateFormatter()
        formatter.calendar = Calendar(identifier: .gregorian)
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.timeZone = TimeZone(secondsFromGMT: 0)
        formatter.dateFormat = "yyyy-MM-dd'T'HH:mm:ss"
        formatter.isLenient = false
        guard var second = formatter.date(from: String(raw[prefixRange])) else { throw Error.invalidTimestamp(raw) }
        if let sign = group(4), let hours = group(5).flatMap(Int.init), let minutes = group(6).flatMap(Int.init) {
            // Go refuses +14:xx other than +14:00.
            guard hours < 14 || minutes == 0 else { throw Error.invalidTimestamp(raw) }
            let offset = TimeInterval(hours * 3600 + minutes * 60)
            second.addTimeInterval(sign == "+" ? -offset : offset)
        }
        let digits = group(2) ?? ""
        let nanos = Int((digits + String(repeating: "0", count: 9 - digits.count)).prefix(9)) ?? 0
        var micros = nanos / 1000
        let remainder = nanos % 1000
        if remainder > 500 || (remainder == 500 && micros % 2 == 1) { micros += 1 }
        if micros == 1_000_000 { second.addTimeInterval(1); micros = 0 }
        let year = Calendar(identifier: .gregorian).dateComponents(in: TimeZone(secondsFromGMT: 0)!, from: second).year ?? 0
        return ("\(formatter.string(from: second)).\(String(format: "%06d", micros))Z", year)
    }
}

/// Reads one payload the way Go's struct decoding sees it: a missing optional
/// is null, a missing required string is empty, a missing collection is empty.
private struct ContentFields {
    let fields: [String: JSONValue]
    init(_ fields: [String: JSONValue]) { self.fields = fields }

    func string(_ key: String) -> String? {
        if case let .string(value) = fields[key] { return value }
        return nil
    }

    func raw(_ key: String) -> JSONValue { .string(string(key) ?? "") }
    func text(_ key: String) -> JSONValue { .string((string(key) ?? "").precomposedStringWithCanonicalMapping) }
    func optionalText(_ key: String) -> JSONValue {
        string(key).map { .string($0.precomposedStringWithCanonicalMapping) } ?? .null
    }
    func time(_ key: String) -> JSONValue { .string(ProtocolCanonicalizer.hashTimestamp(string(key) ?? "")) }
    func optionalTime(_ key: String) -> JSONValue {
        string(key).map { .string(ProtocolCanonicalizer.hashTimestamp($0)) } ?? .null
    }
    func optionalNumber(_ key: String) -> JSONValue {
        if case let .number(value) = fields[key] { return .number(value) }
        return .null
    }
    func integer(_ key: String) -> JSONValue {
        if case let .number(value) = fields[key] { return .number(value) }
        return .number(0)
    }
    func texts(_ key: String) -> JSONValue {
        guard case let .array(values) = fields[key] else { return .array([]) }
        return .array(values.map { value in
            if case let .string(raw) = value { return .string(raw.precomposedStringWithCanonicalMapping) }
            return value
        })
    }

    /// `mapComponents`: `id` and `kind`, plus only the fields that kind owns.
    /// A timed part (a feed's or a running timer's) also has its bottle
    /// seconds and its step bounds; a timer edit's parts do not.
    func components(_ key: String, timed: Bool) throws -> JSONValue {
        guard case let .array(values) = fields[key] else { return .array([]) }
        return .array(try values.map { value in
            guard case let .object(component) = value else { throw ProtocolCanonicalizer.Error.invalidJSON }
            let item = ContentFields(component)
            var result: [String: JSONValue] = ["id": item.raw("id"), "kind": item.raw("kind")]
            switch item.string("kind") {
            case "breast":
                result["side"] = item.string("side").map(JSONValue.string) ?? .null
                result["seconds"] = item.optionalNumber("seconds")
                if timed {
                    result["started_at"] = item.optionalTime("started_at")
                    result["ended_at"] = item.optionalTime("ended_at")
                }
            case "bottle":
                result["volume_ml"] = item.optionalNumber("volume_ml")
                result["contents"] = item.optionalText("contents")
                if timed {
                    result["seconds"] = item.optionalNumber("seconds")
                    result["started_at"] = item.optionalTime("started_at")
                    result["ended_at"] = item.optionalTime("ended_at")
                }
            case "solids":
                result["foods"] = item.texts("foods")
                result["amount"] = item.optionalText("amount")
            default:
                break
            }
            return .object(result)
        })
    }
}
