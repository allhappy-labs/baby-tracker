import BabyProtocol
import Foundation

/// Which APNs gateway this signed build's tokens belong to.
public protocol LiveActivityAPNsEnvironmentResolving: Sendable {
    /// Nil means "not provisioned for push": the feature stays inert.
    func environment() -> LiveActivityAPNsEnvironment?
}

/// Reads the environment the way the OS decides it: from the provisioning
/// profile the build was signed with.
///
/// Apple's `aps-environment` documentation: Xcode sets the entitlement from the
/// provisioning profile, and App Store/TestFlight distribution is production.
/// So the answer is never inferred from DEBUG or a configuration name — this
/// project's Debug and Release both sign with the same development profile.
///
/// The profile's XML plist is read out of `embedded.mobileprovision` without
/// verifying its CMS signature: the OS already validated it to launch the app.
/// A profile without the key, a key with any other value, or bytes that are
/// not a readable profile all fail closed.
public struct ProvisioningProfileEnvironmentResolver: LiveActivityAPNsEnvironmentResolving {
    private let isSimulator: Bool
    private let profile: @Sendable () -> Data?

    public init(isSimulator: Bool = ProvisioningProfileEnvironmentResolver.compiledForSimulator,
                profile: @escaping @Sendable () -> Data? = { ProvisioningProfileEnvironmentResolver.embeddedProfile() }) {
        self.isSimulator = isSimulator
        self.profile = profile
    }

    public static var compiledForSimulator: Bool {
        #if targetEnvironment(simulator)
        true
        #else
        false
        #endif
    }

    /// The app bundle's own profile; absent for App Store and TestFlight builds.
    public static func embeddedProfile(bundle: Bundle = .main) -> Data? {
        guard let url = bundle.url(forResource: "embedded", withExtension: "mobileprovision") else { return nil }
        // A present but unreadable profile must not look like an absent one.
        return (try? Data(contentsOf: url)) ?? Data()
    }

    public func environment() -> LiveActivityAPNsEnvironment? {
        // The simulator talks to the development gateway; its build carries
        // the development entitlement and no profile.
        if isSimulator { return .sandbox }
        guard let data = profile() else { return .production }
        return Self.environment(fromProfile: data)
    }

    static func environment(fromProfile data: Data) -> LiveActivityAPNsEnvironment? {
        guard let plist = xmlPlist(in: data),
              let root = try? PropertyListSerialization.propertyList(from: plist, format: nil) as? [String: Any],
              let entitlements = root["Entitlements"] as? [String: Any],
              let value = entitlements["aps-environment"] as? String else { return nil }
        switch value {
        case "development": return .sandbox
        case "production": return .production
        default: return nil
        }
    }

    /// The bytes from `<?xml` (or `<plist` when there is no declaration)
    /// through the closing `</plist>`.
    private static func xmlPlist(in data: Data) -> Data? {
        let start = data.range(of: Data("<?xml".utf8))?.lowerBound ?? data.range(of: Data("<plist".utf8))?.lowerBound
        guard let start,
              let end = data.range(of: Data("</plist>".utf8), in: start..<data.endIndex)?.upperBound else { return nil }
        return data.subdata(in: start..<end)
    }
}
