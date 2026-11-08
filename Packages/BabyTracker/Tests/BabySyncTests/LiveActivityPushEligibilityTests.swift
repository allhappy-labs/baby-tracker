import BabyProtocol
import BabyStore
import Foundation
import Testing
@testable import BabySync

/// Who may register at all. Push is automatic on exactly one server: the pinned
/// origin AND the pinned instance, advertising the capability, for a device in
/// a family whose user allows Live Activities, with a provisioned APNs
/// environment. Everything else keeps today's behaviour.
struct LiveActivityPushEligibilityTests {
    private let builtIn = PushFixture.builtIn
    private let info = PushFixture.serverInfo()

    @Test func exactOriginAndInstanceWithCapabilityIsEligible() {
        #expect(LiveActivityPushEligibility.isEligible(endpoint: builtIn, info: info, builtIn: builtIn, hasFamily: true, activitiesAllowed: true))
    }

    @Test func originAloneOrInstanceAloneIsNotEnough() throws {
        let otherInstance = ConfirmedEndpoint(origin: builtIn.origin, serverInstanceID: UUID())
        #expect(!LiveActivityPushEligibility.isEligible(
            endpoint: otherInstance, info: PushFixture.serverInfo(instanceID: otherInstance.serverInstanceID),
            builtIn: builtIn, hasFamily: true, activitiesAllowed: true))
        let otherOrigin = ConfirmedEndpoint(origin: try ServerOrigin(validating: "https://baby-sync.example.net"), serverInstanceID: builtIn.serverInstanceID)
        #expect(!LiveActivityPushEligibility.isEligible(
            endpoint: otherOrigin, info: PushFixture.serverInfo(origin: otherOrigin.origin.canonicalString),
            builtIn: builtIn, hasFamily: true, activitiesAllowed: true))
        let otherPort = ConfirmedEndpoint(origin: try ServerOrigin(validating: "https://baby-sync.example-tailnet.ts.net:8443"), serverInstanceID: builtIn.serverInstanceID)
        #expect(!LiveActivityPushEligibility.isEligible(
            endpoint: otherPort, info: PushFixture.serverInfo(origin: otherPort.origin.canonicalString),
            builtIn: builtIn, hasFamily: true, activitiesAllowed: true))
        // The confirmed endpoint matches but the live probe answers as another
        // instance or origin: the probe is the proof, so it must agree too.
        #expect(!LiveActivityPushEligibility.isEligible(
            endpoint: builtIn, info: PushFixture.serverInfo(instanceID: UUID()), builtIn: builtIn, hasFamily: true, activitiesAllowed: true))
        #expect(!LiveActivityPushEligibility.isEligible(
            endpoint: builtIn, info: PushFixture.serverInfo(origin: "https://elsewhere.example.net"), builtIn: builtIn, hasFamily: true, activitiesAllowed: true))
    }

    @Test func missingCapabilityIsNotEligible() {
        let withoutPush = PushFixture.serverInfo(capabilities: ["records-v1", "snapshot-v1", "live-hints-v1"])
        #expect(!LiveActivityPushEligibility.isEligible(endpoint: builtIn, info: withoutPush, builtIn: builtIn, hasFamily: true, activitiesAllowed: true))
        #expect(!LiveActivityPushEligibility.isBuiltInPushServer(endpoint: builtIn, info: withoutPush, builtIn: builtIn))
        // The built-in server without push is still where a registration it
        // holds is taken down: a context that can only remove, never register.
        let context = LiveActivityPushContext(
            familyKey: PushFixture.familyKey(), endpoint: builtIn, serverInfo: withoutPush, builtIn: builtIn,
            activitiesAllowed: true, feedBreakMinutes: 30, environment: .sandbox)
        #expect(context?.pushAdvertised == false)
        #expect(context?.canRegister == false)
        // Another instance or origin without push is nothing at all.
        #expect(LiveActivityPushContext(
            familyKey: PushFixture.familyKey(), endpoint: builtIn, serverInfo: PushFixture.serverInfo(instanceID: UUID(), capabilities: ["records-v1"]),
            builtIn: builtIn, activitiesAllowed: true, feedBreakMinutes: 30, environment: .sandbox) == nil)
    }

    @Test func familyAndSystemAuthorizationAreBothRequired() {
        #expect(!LiveActivityPushEligibility.isEligible(endpoint: builtIn, info: info, builtIn: builtIn, hasFamily: false, activitiesAllowed: true))
        #expect(!LiveActivityPushEligibility.isEligible(endpoint: builtIn, info: info, builtIn: builtIn, hasFamily: true, activitiesAllowed: false))
        #expect(!LiveActivityPushEligibility.isEligible(endpoint: builtIn, info: info, builtIn: builtIn, hasFamily: false, activitiesAllowed: false))
    }

    @Test func selfHostedServerNeverRegistersEvenWithTheCapability() throws {
        let selfHosted = ConfirmedEndpoint(origin: try ServerOrigin(validating: "https://tracker.home.example"), serverInstanceID: UUID())
        let advertised = PushFixture.serverInfo(instanceID: selfHosted.serverInstanceID, origin: selfHosted.origin.canonicalString)
        #expect(!LiveActivityPushEligibility.isEligible(endpoint: selfHosted, info: advertised, builtIn: builtIn, hasFamily: true, activitiesAllowed: true))
        let key = FamilySessionKey(canonicalOrigin: selfHosted.origin.canonicalString, serverInstanceID: selfHosted.serverInstanceID,
                                   familyID: PushFixture.familyID, accountID: PushFixture.accountID, deviceID: PushFixture.deviceID, recoveryEpoch: 1)
        #expect(LiveActivityPushContext(familyKey: key, endpoint: selfHosted, serverInfo: advertised, builtIn: builtIn,
                                        activitiesAllowed: true, feedBreakMinutes: 30, environment: .sandbox) == nil)
    }

    @Test func onlyASharedFamilySessionCarriesAPushRealm() {
        let key = PushFixture.familyKey()
        let account = AccountSessionKey(canonicalOrigin: key.canonicalOrigin, serverInstanceID: key.serverInstanceID,
                                        accountID: key.accountID, deviceID: key.deviceID, recoveryEpoch: key.recoveryEpoch)
        let modes: [(FamilySyncMode, FamilySessionKey?)] = [
            (.localOnly, nil),
            (.endpointConfirmed(builtIn), nil),
            (.authenticated(account), nil),
            (.signedOut(account), nil),
            // A family in recovery has no usable family authority yet.
            (.recovery(key), nil),
            (.shared(key), key),
        ]
        for (mode, expected) in modes {
            #expect(LiveActivityPushEligibility.familyKey(for: mode) == expected)
        }
    }

    @Test func contextRequiresAProvisionedEnvironmentAndTheCurrentEpoch() {
        let key = PushFixture.familyKey()
        #expect(LiveActivityPushContext(familyKey: key, endpoint: builtIn, serverInfo: info, builtIn: builtIn,
                                        activitiesAllowed: true, feedBreakMinutes: 30, environment: nil) == nil)
        #expect(LiveActivityPushContext(familyKey: key, endpoint: builtIn, serverInfo: PushFixture.serverInfo(epoch: PushFixture.epoch + 1),
                                        builtIn: builtIn, activitiesAllowed: true, feedBreakMinutes: 30, environment: .production) == nil)
        let foreignKey = FamilySessionKey(canonicalOrigin: "https://other.example.test", serverInstanceID: builtIn.serverInstanceID,
                                          familyID: PushFixture.familyID, accountID: PushFixture.accountID, deviceID: PushFixture.deviceID, recoveryEpoch: PushFixture.epoch)
        #expect(LiveActivityPushContext(familyKey: foreignKey, endpoint: builtIn, serverInfo: info, builtIn: builtIn,
                                        activitiesAllowed: true, feedBreakMinutes: 30, environment: .production) == nil)
        let context = LiveActivityPushContext(familyKey: key, endpoint: builtIn, serverInfo: info, builtIn: builtIn,
                                              activitiesAllowed: false, feedBreakMinutes: 45, environment: .production)
        // Disallowed activities still yield a context: it is how the
        // coordinator learns to take an existing registration down.
        #expect(context?.activitiesAllowed == false)
        #expect(context?.canRegister == false)
        #expect(context?.feedBreakMinutes == 45)
    }
}

/// Where the APNs environment comes from: the signed provisioning profile,
/// never a build configuration name.
struct LiveActivityAPNsEnvironmentResolverTests {
    private func profile(_ entitlements: String) -> Data {
        // A CMS envelope around the XML plist: binary noise on both sides.
        var data = Data([0x30, 0x82, 0x0F, 0x01, 0x06, 0x09, 0x2A, 0x86])
        data.append(Data("""
        <?xml version="1.0" encoding="UTF-8"?>
        <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
        <plist version="1.0"><dict>
        <key>Name</key><string>BabyTracker Development</string>
        <key>Entitlements</key><dict>\(entitlements)</dict>
        </dict></plist>
        """.utf8))
        data.append(Data([0xA0, 0x82, 0x0B, 0x00, 0xFF]))
        return data
    }

    @Test func simulatorUsesTheDevelopmentGateway() {
        let resolver = ProvisioningProfileEnvironmentResolver(isSimulator: true, profile: { nil })
        #expect(resolver.environment() == .sandbox)
    }

    @Test func deviceWithoutAnEmbeddedProfileIsAppStoreDistribution() {
        let resolver = ProvisioningProfileEnvironmentResolver(isSimulator: false, profile: { nil })
        #expect(resolver.environment() == .production)
    }

    @Test func profileEntitlementDecidesTheGateway() {
        let development = ProvisioningProfileEnvironmentResolver(isSimulator: false, profile: {
            self.profile("<key>aps-environment</key><string>development</string>")
        })
        #expect(development.environment() == .sandbox)
        let production = ProvisioningProfileEnvironmentResolver(isSimulator: false, profile: {
            self.profile("<key>aps-environment</key><string>production</string>")
        })
        #expect(production.environment() == .production)
    }

    @Test func profileWithoutTheEntitlementFailsClosed() {
        // Today's manual development profiles: present, readable, no push.
        let resolver = ProvisioningProfileEnvironmentResolver(isSimulator: false, profile: {
            self.profile("<key>com.apple.security.application-groups</key><array><string>group.com.example.babytracker</string></array>")
        })
        #expect(resolver.environment() == nil)
    }

    @Test func unexpectedValueOrGarbledProfileFailsClosed() {
        let odd = ProvisioningProfileEnvironmentResolver(isSimulator: false, profile: {
            self.profile("<key>aps-environment</key><string>Production</string>")
        })
        #expect(odd.environment() == nil)
        let typed = ProvisioningProfileEnvironmentResolver(isSimulator: false, profile: {
            self.profile("<key>aps-environment</key><true/>")
        })
        #expect(typed.environment() == nil)
        let garbled = ProvisioningProfileEnvironmentResolver(isSimulator: false, profile: {
            Data([0x30, 0x82, 0x01]) + Data("<?xml version=\"1.0\"?><plist><dict><key>Entitlements".utf8)
        })
        #expect(garbled.environment() == nil)
        let empty = ProvisioningProfileEnvironmentResolver(isSimulator: false, profile: { Data() })
        #expect(empty.environment() == nil)
    }
}

/// Build-configuration checks that the package suite can make on the macOS
/// host: the files Xcode signs and bundles with the app.
struct LiveActivityPushBuildConfigurationTests {
    private static let repository = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        .deletingLastPathComponent().deletingLastPathComponent()

    private func plist(_ path: String) throws -> [String: Any] {
        let data = try Data(contentsOf: Self.repository.appending(path: path))
        return try #require(try PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any])
    }

    @Test func pushEntitlementsAreExactlyTheAppEntitlementsPlusTheDevelopmentGateway() throws {
        let base = try plist("Apps/BabyTracker.entitlements")
        var push = try plist("Apps/BabyTrackerLiveActivityPush.entitlements")
        #expect(base["aps-environment"] == nil)
        #expect(push.removeValue(forKey: "aps-environment") as? String == "development")
        #expect(NSDictionary(dictionary: push).isEqual(to: base))
    }

    @Test func pushEntitlementsSignEveryAppConfiguration() throws {
        let project = try String(contentsOf: Self.repository.appending(path: "BabyTracker.xcodeproj/project.pbxproj"), encoding: .utf8)
        // Debug and Release of the app target sign with the push file on every
        // SDK (runbook P2, applied by scripts/live-activity-push-signing.sh
        // enable); the simulator-only conditional is gone with it.
        #expect(project.components(separatedBy: "CODE_SIGN_ENTITLEMENTS = Apps/BabyTrackerLiveActivityPush.entitlements;").count - 1 == 2)
        #expect(!project.contains("CODE_SIGN_ENTITLEMENTS = Apps/BabyTracker.entitlements;"))
        #expect(!project.contains("\"CODE_SIGN_ENTITLEMENTS[sdk=iphonesimulator*]\""))
    }

    @Test func startAlertKeysAreLocalizedInTheAppBundleCatalogForEveryLocale() throws {
        // The server's Live Activity payloads are built in transport.go: the
        // PostgreSQL authority's port replaced `main`'s apns.go with it and
        // the shared `internal/apns` client.
        let apns = try String(contentsOf: Self.repository.appending(path: "Server/internal/liveactivity/transport.go"), encoding: .utf8)
        let catalogData = try Data(contentsOf: Self.repository.appending(path: "Apps/SharedIntents/Localizable.xcstrings"))
        let catalog = try #require(try JSONSerialization.jsonObject(with: catalogData) as? [String: Any])
        let strings = try #require(catalog["strings"] as? [String: Any])
        let expected = ["LIVE_ACTIVITY_FEED_START_TITLE": 0, "LIVE_ACTIVITY_SLEEP_START_TITLE": 0, "LIVE_ACTIVITY_START_BODY": 1]
        for (key, arguments) in expected {
            // The server's payload names exactly these keys.
            #expect(apns.contains("\"\(key)\""))
            let entry = try #require(strings[key] as? [String: Any], "missing \(key)")
            let localizations = try #require(entry["localizations"] as? [String: Any])
            for locale in ["en", "de", "fr", "es", "it", "uk"] {
                let unit = (localizations[locale] as? [String: Any])?["stringUnit"] as? [String: Any]
                let value = try #require(unit?["value"] as? String, "\(key) lacks \(locale)")
                #expect(!value.isEmpty && value != key)
                #expect(value.components(separatedBy: "%@").count - 1 == arguments, "\(key) \(locale)")
            }
        }
    }
}
