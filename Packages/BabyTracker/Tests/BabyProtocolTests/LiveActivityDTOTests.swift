import Foundation
import Testing
@testable import BabyProtocol

@Suite("Live Activity registration contracts")
struct LiveActivityDTOTests {
    private let installation = "11111111-1111-4111-8111-111111111111"
    private var deviceJSON: String {
        #"{"protocol_version":"v1","capabilities":["live-activity-push-v1"],"installation_id":"11111111-1111-4111-8111-111111111111","registration_version":1,"apns_environment":"sandbox","payload_version":1,"push_to_start_token":"aabb","feed_break_minutes":30}"#
    }
    private var scopeJSON: String {
        #"{"server_instance_id":"22222222-2222-4222-8222-222222222222","recovery_epoch":1,"family_id":"33333333-3333-4333-8333-333333333333","baby_id":"44444444-4444-4444-8444-444444444444","kind_raw":"feed"}"#
    }
    private var tokenJSON: String {
        #"{"protocol_version":"v1","capabilities":["live-activity-push-v1"],"installation_id":"11111111-1111-4111-8111-111111111111","activity_id":"activity-1","token":"aabb","token_version":1,"scope":\#(scopeJSON),"state":"active"}"#
    }

    @Test func foregroundInventoryBounds() throws {
        let proof = try LiveActivityAbsentGeneration(generationID: UUID(uuidString: installation)!, observedAttemptID: UUID(uuidString: "22222222-2222-4222-8222-222222222222")!)
        let inventory = try LiveActivityForegroundInventory(inventoryVersion: 1, absentGenerations: [proof])
        let registration = try LiveActivityDeviceRegistration(installationID: UUID(uuidString: installation)!, registrationVersion: 1, apnsEnvironment: .sandbox, pushToStartToken: "aabb", foregroundInventory: inventory)
        let encoded = try JSONEncoder().encode(registration)
        #expect(try JSONDecoder().decode(LiveActivityDeviceRegistration.self, from: encoded) == registration)
        #expect(throws: (any Error).self) { try LiveActivityForegroundInventory(inventoryVersion: 0, absentGenerations: [proof]) }
        #expect(throws: (any Error).self) { try LiveActivityForegroundInventory(inventoryVersion: 1, absentGenerations: [proof, proof]) }
    }

    @Test func registrationBounds() throws {
        let decoder = JSONDecoder()
        let valid = try decoder.decode(LiveActivityDeviceRegistration.self, from: Data(deviceJSON.utf8))
        #expect(valid.feedBreakMinutes == 30)
        let missingPreference = deviceJSON.replacingOccurrences(of: #","feed_break_minutes":30"#, with: "")
        #expect(try decoder.decode(LiveActivityDeviceRegistration.self, from: Data(missingPreference.utf8)).feedBreakMinutes == 30)
        for minutes in [5, 120] {
            let raw = deviceJSON.replacingOccurrences(of: "\"feed_break_minutes\":30", with: "\"feed_break_minutes\":\(minutes)")
            #expect(try decoder.decode(LiveActivityDeviceRegistration.self, from: Data(raw.utf8)).feedBreakMinutes == minutes)
        }
        let maximumToken = deviceJSON.replacingOccurrences(of: "aabb", with: String(repeating: "ab", count: 1_024))
        #expect(try decoder.decode(LiveActivityDeviceRegistration.self, from: Data(maximumToken.utf8)).pushToStartToken.count == 2_048)
        for (old, new) in [
            ("aabb", ""), ("aabb", "abc"), ("aabb", "AABB"), ("aabb", "zz"),
            ("aabb", String(repeating: "ab", count: 1_025)),
            ("\"feed_break_minutes\":30", "\"feed_break_minutes\":6"),
            ("\"feed_break_minutes\":30", "\"feed_break_minutes\":0"),
            ("\"feed_break_minutes\":30", "\"feed_break_minutes\":125"),
            ("\"payload_version\":1", "\"payload_version\":2"),
            (installation, "00000000-0000-0000-0000-000000000000"),
            (installation, "not-a-uuid"),
            ("\"registration_version\":1", "\"registration_version\":0"),
            ("\"registration_version\":1", "\"registration_version\":1.5"),
            ("\"registration_version\":1", "\"registration_version\":9007199254740992"),
            ("sandbox", "development"), ("v1", "v2"), ("live-activity-push-v1", "records-v1")
        ] {
            #expect(throws: (any Error).self) {
                try decoder.decode(LiveActivityDeviceRegistration.self, from: Data(deviceJSON.replacingOccurrences(of: old, with: new).utf8))
            }
        }
    }

    @Test func tokenRegistrationBounds() throws {
        let decoder = JSONDecoder()
        #expect(try decoder.decode(LiveActivityTokenRegistration.self, from: Data(tokenJSON.utf8)).knownGenerationID == nil)
        for state in ["active", "ended", "dismissed"] {
            #expect(try decoder.decode(LiveActivityTokenRegistration.self, from: Data(tokenJSON.replacingOccurrences(of: "active", with: state).utf8)).state.rawValue == state)
        }
        for (old, new) in [
            ("activity-1", ""), ("aabb", "ABC"), ("active", "unknown"),
            ("\"token_version\":1", "\"token_version\":0"),
            ("\"token_version\":1", "\"token_version\":9007199254740992"),
            (installation, "not-a-uuid"), ("feed", "diaper"),
            ("\"recovery_epoch\":1", "\"recovery_epoch\":0")
        ] {
            #expect(throws: (any Error).self) {
                try decoder.decode(LiveActivityTokenRegistration.self, from: Data(tokenJSON.replacingOccurrences(of: old, with: new).utf8))
            }
        }
        for field in ["known_generation_id", "timer_id", "session_id"] {
            for value in ["not-a-uuid", ""] {
                let raw = tokenJSON.dropLast() + ",\"\(field)\":\"\(value)\"}"
                #expect(throws: (any Error).self) { try decoder.decode(LiveActivityTokenRegistration.self, from: Data(raw.utf8)) }
            }
        }
    }

    @Test func canonicalRegistrationEncoding() throws {
        let registration = try JSONDecoder().decode(LiveActivityTokenRegistration.self, from: Data(tokenJSON.utf8))
        let object = try #require(JSONSerialization.jsonObject(with: JSONEncoder().encode(registration)) as? [String: Any])
        #expect(object["installation_id"] as? String == installation)
        let scope = try #require(object["scope"] as? [String: Any])
        #expect(scope["server_instance_id"] as? String == "22222222-2222-4222-8222-222222222222")
        #expect(scope["kind_raw"] as? String == "feed")
        #expect(object["account_id"] == nil)
    }

    @Test func revisionBounds() throws {
        let scope = try JSONDecoder().decode(LiveActivityTokenRegistration.self, from: Data(tokenJSON.utf8)).scope
        let id = UUID(uuidString: installation)!
        let revision = try LiveActivityRevision(scope: scope, generationID: id, familySequence: 0,
            presentationVersion: 1, presentationTimestamp: 1_790_985_600, canWrite: false)
        #expect(revision.familySequence == 0)
        #expect(revision.canWrite == false)
        for (sequence, version, timestamp) in [(-1, 1, 0), (0, 0, 0), (0, 1, -1), (9_007_199_254_740_992, 1, 0)] as [(Int64, Int64, Int64)] {
            #expect(throws: (any Error).self) {
                try LiveActivityRevision(scope: scope, generationID: id, familySequence: sequence,
                    presentationVersion: version, presentationTimestamp: timestamp, canWrite: true)
            }
        }
    }

    @Test func receiptRejectsUnrelatedRealmAndInvalidVersions() throws {
        let raw = #"{"protocol_version":"v1","server_instance_id":"22222222-2222-4222-8222-222222222222","recovery_epoch":1,"accepted_registration_version":1,"accepted_token_version":2,"generations":[{"activity_id":"activity-1","scope":\#(scopeJSON),"generation_id":"55555555-5555-4555-8555-555555555555"}]}"#
        let receipt = try JSONDecoder().decode(LiveActivityRegistrationReceipt.self, from: Data(raw.utf8))
        #expect(receipt.acceptedTokenVersion == 2)
        #expect(receipt.generations.first?.generationID.uuidString.lowercased() == "55555555-5555-4555-8555-555555555555")
        for body in [
            raw.replacingOccurrences(of: "\"accepted_registration_version\":1", with: "\"accepted_registration_version\":0"),
            raw.replacingOccurrences(of: "\"accepted_token_version\":2", with: "\"accepted_token_version\":9007199254740992"),
            raw.replacingOccurrences(of: "\"recovery_epoch\":1,\"accepted_registration", with: "\"recovery_epoch\":2,\"accepted_registration"),
            raw.replacingOccurrences(of: "\"scope\":{\"server_instance_id\":\"22222222-2222-4222-8222-222222222222\"", with: "\"scope\":{\"server_instance_id\":\"66666666-6666-4666-8666-666666666666\""),
            raw.replacingOccurrences(of: "55555555-5555-4555-8555-555555555555", with: "not-a-uuid")
        ] {
            #expect(throws: (any Error).self) { try JSONDecoder().decode(LiveActivityRegistrationReceipt.self, from: Data(body.utf8)) }
        }
    }

    @Test func fixtureParity() throws {
        let fixtures = ProtocolFixtureLoader(directory: Self.fixtureDirectory)
        let device = try fixtures.decode(LiveActivityDeviceRegistration.self, named: "device-registration.json")
        let token = try fixtures.decode(LiveActivityTokenRegistration.self, named: "token-registration.json")
        let receipt = try fixtures.decode(LiveActivityRegistrationReceipt.self, named: "registration-receipt.json")
        #expect(device.registrationVersion == 1)
        #expect(device.feedBreakMinutes == 30)
        #expect(token.tokenVersion == receipt.acceptedTokenVersion)
        #expect(token.knownGenerationID == receipt.generations.first?.generationID)
        for (name, data) in [
            ("device-registration.json", try JSONEncoder().encode(device)),
            ("token-registration.json", try JSONEncoder().encode(token)),
            ("registration-receipt.json", try JSONEncoder().encode(receipt))
        ] {
            let original = try Data(contentsOf: Self.fixtureDirectory.appending(path: name))
            #expect(try JSONDecoder().decode(JSONValue.self, from: data) == JSONDecoder().decode(JSONValue.self, from: original))
        }
    }

    @Test func fractionalCountersAreRejectedBeforeIntegerCoercion() throws {
        let decoder = JSONDecoder()
        let camelScope = #"{"serverInstanceID":"22222222-2222-4222-8222-222222222222","recoveryEpoch":1,"familyID":"33333333-3333-4333-8333-333333333333","babyID":"44444444-4444-4444-8444-444444444444","kindRaw":"feed"}"#
        let revision = #"{"scope":\#(camelScope),"generationID":"55555555-5555-4555-8555-555555555555","familySequence":0,"presentationVersion":1,"presentationTimestamp":1790985600,"canWrite":true}"#
        let receipt = #"{"protocol_version":"v1","server_instance_id":"22222222-2222-4222-8222-222222222222","recovery_epoch":1,"accepted_registration_version":1,"accepted_token_version":2,"generations":[]}"#
        for fraction in ["9007199254740991.1", "9007199254740990.1", "1.0000000000000001"] {
            #expect(throws: (any Error).self) {
                try decoder.decode(LiveActivityDeviceRegistration.self, from: Data(deviceJSON.replacingOccurrences(of: "\"registration_version\":1", with: "\"registration_version\":\(fraction)").utf8))
            }
            for field in ["token_version", "recovery_epoch"] {
                #expect(throws: (any Error).self) {
                    try decoder.decode(LiveActivityTokenRegistration.self, from: Data(tokenJSON.replacingOccurrences(of: "\"\(field)\":1", with: "\"\(field)\":\(fraction)").utf8))
                }
            }
            #expect(throws: (any Error).self) {
                try decoder.decode(LiveActivityScope.self, from: Data(camelScope.replacingOccurrences(of: "\"recoveryEpoch\":1", with: "\"recoveryEpoch\":\(fraction)").utf8))
            }
            for (field, original) in [("familySequence", "0"), ("presentationVersion", "1"), ("presentationTimestamp", "1790985600")] {
                #expect(throws: (any Error).self) {
                    try decoder.decode(LiveActivityRevision.self, from: Data(revision.replacingOccurrences(of: "\"\(field)\":\(original)", with: "\"\(field)\":\(fraction)").utf8))
                }
            }
            for (field, original) in [("recovery_epoch", "1"), ("accepted_registration_version", "1"), ("accepted_token_version", "2")] {
                #expect(throws: (any Error).self) {
                    try decoder.decode(LiveActivityRegistrationReceipt.self, from: Data(receipt.replacingOccurrences(of: "\"\(field)\":\(original)", with: "\"\(field)\":\(fraction)").utf8))
                }
            }
        }
        for (field, original, fraction) in [("payload_version", "1", "1.0000000000000001"), ("feed_break_minutes", "30", "30.0000000000000001")] {
            #expect(throws: (any Error).self) {
                try decoder.decode(LiveActivityDeviceRegistration.self, from: Data(deviceJSON.replacingOccurrences(of: "\"\(field)\":\(original)", with: "\"\(field)\":\(fraction)").utf8))
            }
        }
        let maximum = deviceJSON.replacingOccurrences(of: "\"registration_version\":1", with: "\"registration_version\":9007199254740991")
        #expect(try decoder.decode(LiveActivityDeviceRegistration.self, from: Data(maximum.utf8)).registrationVersion == 9_007_199_254_740_991)
        let withoutOptional = receipt.replacingOccurrences(of: #","accepted_token_version":2"#, with: "")
        #expect(try decoder.decode(LiveActivityRegistrationReceipt.self, from: Data(withoutOptional.utf8)).acceptedTokenVersion == nil)
    }

    private static var fixtureDirectory: URL {
        URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            .appending(path: "Server/api/fixtures/live-activity-v1")
    }
}
