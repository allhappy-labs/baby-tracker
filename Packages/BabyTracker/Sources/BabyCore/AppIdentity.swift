import Foundation

/// The app's reverse-DNS identity, from `BT_BUNDLE_ID_PREFIX` in
/// `Config/Base.xcconfig` (overridden in the gitignored `Config/Local.xcconfig`).
///
/// The app and the widget extension carry it in their Info.plist as
/// `BTBundleIDPrefix`; without it (the package tests, a host that does not set
/// it) the placeholder applies. Every App Group, Keychain service, log
/// subsystem and task identifier derives from it, so a build signed for one
/// prefix keeps finding its own data.
public enum AppIdentity {
    public static let placeholderPrefix = "com.example.babytracker"

    public static let bundlePrefix: String = {
        let value = Bundle.main.object(forInfoDictionaryKey: "BTBundleIDPrefix") as? String
        guard let value, !value.isEmpty, !value.contains("$(") else { return placeholderPrefix }
        return value
    }()

    /// The App Group shared by the app and the widget extension.
    public static var appGroupID: String { "group.\(bundlePrefix)" }

    /// `bundlePrefix` followed by `.suffix`.
    public static func identifier(_ suffix: String) -> String { "\(bundlePrefix).\(suffix)" }
}
