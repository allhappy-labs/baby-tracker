import BabyProtocol
import Foundation
import Testing
@testable import BabySync

/// Where the APNs environment comes from: the signed provisioning profile,
/// never a build configuration name. Ported from `main`'s Live Activity
/// resolver tests.
@Suite("Push environment resolver")
struct PushEnvironmentResolverTests {
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

    @Test func simulatorIsSandbox() {
        let resolver = PushEnvironmentResolver(isSimulator: true, profile: { nil })
        #expect(resolver.environment() == .sandbox)
    }

    @Test func developmentProfileIsSandbox() {
        let development = PushEnvironmentResolver(isSimulator: false, profile: {
            self.profile("<key>aps-environment</key><string>development</string>")
        })
        #expect(development.environment() == .sandbox)
        let production = PushEnvironmentResolver(isSimulator: false, profile: {
            self.profile("<key>aps-environment</key><string>production</string>")
        })
        #expect(production.environment() == .production)
    }

    /// App Store and TestFlight builds carry no embedded profile.
    @Test func noProfileIsProduction() {
        let resolver = PushEnvironmentResolver(isSimulator: false, profile: { nil })
        #expect(resolver.environment() == .production)
    }

    /// A development profile without Push, an unexpected value, or bytes that
    /// are not a profile: push stays inert.
    @Test func profileWithoutTheKeyIsInert() {
        let withoutPush = PushEnvironmentResolver(isSimulator: false, profile: {
            self.profile("<key>com.apple.security.application-groups</key><array><string>group.com.example.babytracker</string></array>")
        })
        #expect(withoutPush.environment() == nil)
        let odd = PushEnvironmentResolver(isSimulator: false, profile: {
            self.profile("<key>aps-environment</key><string>Production</string>")
        })
        #expect(odd.environment() == nil)
        let typed = PushEnvironmentResolver(isSimulator: false, profile: {
            self.profile("<key>aps-environment</key><true/>")
        })
        #expect(typed.environment() == nil)
        let garbled = PushEnvironmentResolver(isSimulator: false, profile: {
            Data([0x30, 0x82, 0x01]) + Data("<?xml version=\"1.0\"?><plist><dict><key>Entitlements".utf8)
        })
        #expect(garbled.environment() == nil)
        let empty = PushEnvironmentResolver(isSimulator: false, profile: { Data() })
        #expect(empty.environment() == nil)
    }

    /// `main`'s Live Activity resolver is this resolver, not a copy of it:
    /// the same type answering with the same environment type, so both
    /// features resolve every profile to the same gateway.
    @Test func liveActivityEnvironmentIsThePushResolver() {
        #expect(ObjectIdentifier(ProvisioningProfileEnvironmentResolver.self) == ObjectIdentifier(PushEnvironmentResolver.self))
        #expect(ObjectIdentifier(LiveActivityAPNsEnvironment.self) == ObjectIdentifier(PushEnvironment.self))
        let profiles: [Data?] = [
            nil,
            Data(),
            profile("<key>aps-environment</key><string>development</string>"),
            profile("<key>aps-environment</key><string>production</string>"),
            profile("<key>aps-environment</key><string>Production</string>"),
        ]
        for isSimulator in [true, false] {
            for data in profiles {
                let liveActivity: any LiveActivityAPNsEnvironmentResolving = ProvisioningProfileEnvironmentResolver(
                    isSimulator: isSimulator, profile: { data })
                let push: any PushEnvironmentResolving = PushEnvironmentResolver(isSimulator: isSimulator, profile: { data })
                #expect(liveActivity.environment() == push.environment())
            }
        }
    }
}
