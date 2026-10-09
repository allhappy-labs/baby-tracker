import CryptoKit
import Foundation

public struct ProtocolFixtureLoader: Sendable {
    public struct Verification: Sendable, Equatable {
        public let version: String
        public let verifiedFileCount: Int
    }

    private struct Manifest: Decodable {
        let version: String
        let algorithm: String
        let files: [String: String]
    }

    public let directory: URL

    public init(directory: URL) { self.directory = directory }

    public func decode<T: Decodable>(_ type: T.Type, named name: String) throws -> T {
        try JSONDecoder().decode(type, from: Data(contentsOf: directory.appending(path: name)))
    }

    public func verifyManifest() throws -> Verification {
        let manifest = try decode(Manifest.self, named: "manifest.json")
        guard manifest.algorithm == "sha256" else { throw FixtureError.unsupportedAlgorithm(manifest.algorithm) }
        for (name, expected) in manifest.files {
            let data = try Data(contentsOf: directory.appending(path: name))
            let actual = SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
            guard actual == expected else { throw FixtureError.digestMismatch(name) }
        }
        return Verification(version: manifest.version, verifiedFileCount: manifest.files.count)
    }

    public enum FixtureError: Error, Sendable, Equatable {
        case unsupportedAlgorithm(String)
        case digestMismatch(String)
    }
}

public struct CanonicalVectors: Codable, Sendable, Equatable {
    public struct MutationVector: Codable, Sendable, Equatable {
        public let fixture: String
        /// Set when the fixture file holds several mutations by name.
        public let key: String?
        public let canonicalJSON: String
        public let sha256: String
        enum CodingKeys: String, CodingKey { case fixture, key, canonicalJSON = "canonical_json", sha256 }
    }
    public struct ScalarVector: Codable, Sendable, Equatable { public let input: String; public let canonical: String }
    public struct TimestampVector: Codable, Sendable, Equatable { public let input: String; public let canonical: String; public let valid: Bool }
    public struct UnicodeVectors: Codable, Sendable, Equatable {
        public let normalization: [ScalarVector]
        public let json: JSONEncodingVector?
    }
    /// Raw U+2028/U+2029 against the same characters typed as escaped text.
    public struct JSONEncodingVector: Codable, Sendable, Equatable {
        public let actual: String
        public let literal: String
        public let canonicalJSON: String
        public let sha256: String
        enum CodingKeys: String, CodingKey { case actual, literal, canonicalJSON = "canonical_json", sha256 }
    }
    /// A content-hash vector keyed by entity kind and payload version, shared
    /// with `TestCanonicalJSONMatchesIndependentFixtureLiteral` in
    /// `Server/internal/protocol/canonical_test.go` so a sleep v2 or timer v5
    /// session-link field cannot drift between the two hash implementations.
    public struct SessionContentHashVector: Codable, Sendable, Equatable {
        public let entityKind: String
        public let payloadVersion: Int64
        public let mutationJSON: String
        public let sha256: String
        enum CodingKeys: String, CodingKey { case entityKind = "entity_kind", payloadVersion = "payload_version", mutationJSON = "mutation_json", sha256 }
    }
    public struct SessionContentHashVectors: Codable, Sendable, Equatable {
        public let sleepV2: SessionContentHashVector
        public let timerV5: SessionContentHashVector
        enum CodingKeys: String, CodingKey { case sleepV2 = "sleep_v2", timerV5 = "timer_v5" }
    }
    public let normalizationVersion: String
    public let mutation: MutationVector
    public let timerEditMutation: MutationVector
    public let decimals: [ScalarVector]
    public let timestamps: [TimestampVector]
    public let unicode: UnicodeVectors
    public let sessionContentHashes: SessionContentHashVectors
    enum CodingKeys: String, CodingKey {
        case normalizationVersion = "normalization_version", mutation, timerEditMutation = "timer_edit_mutation"
        case decimals, timestamps, unicode, sessionContentHashes = "session_content_hashes"
    }
}

public enum ProtocolCanonicalizer {
    public enum Error: Swift.Error, Sendable, Equatable {
        case invalidJSON
        case duplicateKey(String)
        case unsafeInteger
        case invalidDecimal(String)
        case invalidTimestamp(String)
    }

    private static let maximumSafeInteger: Decimal = 9_007_199_254_740_991

    /// `canonicalMutationBytes` in `Server/internal/protocol/canonical.go`:
    /// the bytes the receipt hash covers. The payload is the server's typed
    /// projection of it (`mutationPayload`), not this device's JSON, so two
    /// spellings of the same mutation hash alike.
    public static func canonicalMutationBytes(_ mutation: WireMutation) throws -> Data {
        let payload = try mutationPayload(
            entityKind: mutation.entityKind, operation: mutation.operation, payload: mutation.payload
        )
        let value: JSONValue = .object([
            "client_mutation_time": .string(hashTimestamp(mutation.clientMutationTime)),
            "entity_id": .string(mutation.entityID.uuidString.lowercased()),
            "entity_kind": .string(mutation.entityKind.rawValue),
            "import_id": mutation.importID.map { .string($0.uuidString.lowercased()) } ?? .null,
            "observed_entity_version": .number(Decimal(mutation.observedEntityVersion)),
            "operation": .string(mutation.operation.rawValue),
            "payload": payload,
            "payload_version": .number(Decimal(mutation.payloadVersion)),
        ])
        return Data(try canonicalJSON(value).utf8)
    }

    public static func canonicalMutationSHA256(_ mutation: WireMutation) throws -> String {
        var data = Data("baby-tracker-mutation-v1\0".utf8)
        data.append(try canonicalMutationBytes(mutation))
        return SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }

    public static func canonicalDecimal(_ raw: String) throws -> String {
        guard var value = Decimal(string: raw, locale: Locale(identifier: "en_US_POSIX")) else {
            throw Error.invalidDecimal(raw)
        }
        var rounded = Decimal()
        NSDecimalRound(&rounded, &value, 6, .bankers)
        if rounded == 0 { return "0" }
        return NSDecimalString(&rounded, Locale(identifier: "en_US_POSIX"))
    }

    /// `NormalizeTimestamp`: the server's lexical form (see
    /// `lexicalTimestamp`) inside the protocol's 1900–2100 range.
    public static func canonicalTimestamp(_ raw: String) throws -> String {
        let timestamp = try lexicalTimestamp(raw)
        guard (1900...2100).contains(timestamp.year) else { throw Error.invalidTimestamp(raw) }
        return timestamp.canonical
    }

    public static func normalizedBabyName(_ raw: String) throws -> String {
        let whitespace = CharacterSet.whitespacesAndNewlines.union(CharacterSet(charactersIn: "\u{00a0}"))
        let trimmed = raw.trimmingCharacters(in: whitespace).precomposedStringWithCanonicalMapping
        return trimmed.folding(options: [.caseInsensitive], locale: Locale(identifier: "en_US_POSIX"))
            .precomposedStringWithCanonicalMapping
    }

    public static func validateSafeIntegerJSON(_ data: Data) throws {
        let raw = String(decoding: data, as: UTF8.self)
        guard raw.range(of: #"^-?(0|[1-9][0-9]*)$"#, options: .regularExpression) != nil,
              let number = Decimal(string: raw, locale: Locale(identifier: "en_US_POSIX")),
              abs(number) <= maximumSafeInteger else { throw Error.unsafeInteger }
    }

    public static func validateJSON(_ data: Data) throws {
        var parser = DuplicateKeyParser(bytes: Array(data))
        try parser.parseDocument()
    }

    static func canonicalJSON(_ value: JSONValue) throws -> String {
        switch value {
        case .null: return "null"
        case .bool(let value): return value ? "true" : "false"
        case .number(let value):
            var mutable = value
            return try canonicalDecimal(NSDecimalString(&mutable, Locale(identifier: "en_US_POSIX")))
        case .string(let value): return "\"\(escape(value))\""
        case .array(let values): return "[\(try values.map(canonicalJSON).joined(separator: ","))]"
        case .object(let object):
            return "{\(try object.keys.sorted().map { "\"\(escape($0))\":\(try canonicalJSON(object[$0]!))" }.joined(separator: ","))}"
        }
    }

    private static func escape(_ value: String) -> String {
        var result = ""
        for scalar in value.unicodeScalars {
            switch scalar.value {
            case 0x22: result += "\\\""
            case 0x5c: result += "\\\\"
            case 0x08: result += "\\b"
            case 0x0c: result += "\\f"
            case 0x0a: result += "\\n"
            case 0x0d: result += "\\r"
            case 0x09: result += "\\t"
            case 0...0x1f: result += String(format: "\\u%04x", scalar.value)
            default: result.unicodeScalars.append(scalar)
            }
        }
        return result
    }
}

private struct DuplicateKeyParser {
    let bytes: [UInt8]
    var index = 0

    mutating func parseDocument() throws {
        skipWhitespace(); try parseValue(); skipWhitespace()
        guard index == bytes.count else { throw ProtocolCanonicalizer.Error.invalidJSON }
    }

    mutating func parseValue() throws {
        skipWhitespace(); guard index < bytes.count else { throw ProtocolCanonicalizer.Error.invalidJSON }
        switch bytes[index] {
        case 0x7b: try parseObject()
        case 0x5b: try parseArray()
        case 0x22: _ = try parseString()
        case 0x74: try consume("true")
        case 0x66: try consume("false")
        case 0x6e: try consume("null")
        default: try parseNumber()
        }
    }

    mutating func parseObject() throws {
        index += 1; skipWhitespace()
        var keys = Set<String>()
        if take(0x7d) { return }
        while true {
            let key = try parseString()
            guard keys.insert(key).inserted else { throw ProtocolCanonicalizer.Error.duplicateKey(key) }
            skipWhitespace(); guard take(0x3a) else { throw ProtocolCanonicalizer.Error.invalidJSON }
            try parseValue(); skipWhitespace()
            if take(0x7d) { return }
            guard take(0x2c) else { throw ProtocolCanonicalizer.Error.invalidJSON }
            skipWhitespace()
        }
    }

    mutating func parseArray() throws {
        index += 1; skipWhitespace()
        if take(0x5d) { return }
        while true {
            try parseValue(); skipWhitespace()
            if take(0x5d) { return }
            guard take(0x2c) else { throw ProtocolCanonicalizer.Error.invalidJSON }
        }
    }

    mutating func parseString() throws -> String {
        guard take(0x22) else { throw ProtocolCanonicalizer.Error.invalidJSON }
        let start = index
        var escaped = false
        while index < bytes.count {
            let byte = bytes[index]
            if byte == 0x22 && !escaped {
                let raw = Data(bytes[(start - 1)...index])
                index += 1
                return try JSONDecoder().decode(String.self, from: raw)
            }
            if byte == 0x5c && !escaped { escaped = true } else { escaped = false }
            index += 1
        }
        throw ProtocolCanonicalizer.Error.invalidJSON
    }

    mutating func parseNumber() throws {
        let start = index
        while index < bytes.count, ![0x20, 0x09, 0x0a, 0x0d, 0x2c, 0x5d, 0x7d].contains(bytes[index]) { index += 1 }
        guard start != index else { throw ProtocolCanonicalizer.Error.invalidJSON }
        let raw = Data(bytes[start..<index])
        _ = try JSONSerialization.jsonObject(with: Data("[".utf8) + raw + Data("]".utf8))
    }

    mutating func consume(_ string: String) throws {
        let expected = Array(string.utf8)
        guard index + expected.count <= bytes.count, Array(bytes[index..<(index + expected.count)]) == expected else {
            throw ProtocolCanonicalizer.Error.invalidJSON
        }
        index += expected.count
    }

    mutating func skipWhitespace() { while index < bytes.count, [0x20, 0x09, 0x0a, 0x0d].contains(bytes[index]) { index += 1 } }
    mutating func take(_ byte: UInt8) -> Bool { guard index < bytes.count, bytes[index] == byte else { return false }; index += 1; return true }
}
