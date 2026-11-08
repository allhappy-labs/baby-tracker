// swift-tools-version: 6.2
import PackageDescription

let package = Package(
    name: "BabyTracker",
    defaultLocalization: "en",
    platforms: [.iOS(.v26), .macOS(.v26)],
    products: [
        .library(name: "GrowthFeature", targets: ["GrowthFeature"]),
        .library(name: "BabyCore", targets: ["BabyCore"]),
        .library(name: "BabyProtocol", targets: ["BabyProtocol"]),
        .library(name: "BabyStore", targets: ["BabyStore"]),
        .library(name: "BabySync", targets: ["BabySync"]),
        .library(name: "BabyUI", targets: ["BabyUI"]),
        .library(name: "HomeFeature", targets: ["HomeFeature"]),
        .library(name: "LoggingFeature", targets: ["LoggingFeature"]),
        .library(name: "HistoryFeature", targets: ["HistoryFeature"]),
        .library(name: "InsightsFeature", targets: ["InsightsFeature"]),
        .library(name: "AskFeature", targets: ["AskFeature"]),
        .library(name: "BabyFeature", targets: ["BabyFeature"]),
        .library(name: "SettingsFeature", targets: ["SettingsFeature"]),
    ],
    targets: [
        .target(name: "GrowthFeature", dependencies: ["BabyCore", "BabyStore", "BabyUI"], resources: [
            .process("Localizable.xcstrings"),
            .copy("SwissPEZZ2025.csv"),
            .copy("SwissPEZZ2025-ATTRIBUTION.md"),
            .copy("SwissPEZZ2025-manifest.json"),
        ]),
        .target(name: "BabyCore", resources: [.process("Localizable.xcstrings")]),
        .target(name: "BabyProtocol", dependencies: ["BabyCore"]),
        .target(name: "BabyStore", dependencies: ["BabyCore", "BabyProtocol", "BabyUI"], resources: [.process("Localizable.xcstrings")]),
        .executableTarget(name: "BabyStoreLeaseProbe", dependencies: ["BabyStore", "BabyCore", "BabySync"]),
        .target(name: "BabySync", dependencies: ["BabyCore", "BabyProtocol", "BabyStore"], resources: [.process("Localizable.xcstrings")]),
        .target(name: "BabyUI", dependencies: ["BabyCore"], resources: [
            .process("Localizable.xcstrings"),
            .process("Resources/Icons.xcassets"),
        ]),
        .target(name: "HomeFeature", dependencies: ["BabyCore", "BabyStore", "BabyUI"], resources: [.process("Localizable.xcstrings")]),
        .target(name: "LoggingFeature", dependencies: ["BabyCore", "BabyProtocol", "BabyStore", "BabyUI"], resources: [.process("Localizable.xcstrings")]),
        .target(name: "HistoryFeature", dependencies: ["BabyCore", "BabyProtocol", "BabyStore", "BabyUI"], resources: [.process("Localizable.xcstrings")]),
        .target(name: "InsightsFeature", dependencies: ["BabyCore", "BabyStore", "BabyUI", "GrowthFeature"], resources: [.process("Localizable.xcstrings")]),
        .target(name: "AskFeature", dependencies: ["BabyCore", "BabyStore", "BabyUI"], resources: [.process("Localizable.xcstrings")]),
        .target(name: "BabyFeature", dependencies: ["BabyCore", "BabyStore", "BabyUI"], resources: [.process("Localizable.xcstrings")]),
        .target(name: "SettingsFeature", dependencies: ["BabyCore", "BabyStore", "BabyUI", "BabyFeature", "BabySync"], resources: [.process("Localizable.xcstrings")]),
        .testTarget(name: "GrowthFeatureTests", dependencies: ["GrowthFeature", "BabyCore", "BabyStore"]),
        .testTarget(name: "BabyCoreTests", dependencies: ["BabyCore"]),
        .testTarget(name: "BabyProtocolTests", dependencies: ["BabyProtocol", "BabyCore"]),
        .testTarget(name: "BabyStoreTests", dependencies: ["BabyStore"]),
        .testTarget(name: "BabySyncTests", dependencies: ["BabySync", "BabyStore", "BabyProtocol"]),
        // The iOS client against a live server. Inert without the environment
        // `protocol-e2e.sh` provides — see FamilySyncClientE2ETests.
        // The two-caregiver test builds each phone with the app's own
        // composition (SettingsFeature) and edits through the app's own edit
        // models (LoggingFeature's edit sheets, which History opens, and
        // HomeFeature for a running timer), so it runs the wiring a phone runs.
        .testTarget(
            name: "FamilySyncClientE2ETests",
            dependencies: ["BabySync", "BabyStore", "BabyProtocol", "BabyCore", "SettingsFeature", "LoggingFeature", "HomeFeature"]
        ),
        .testTarget(name: "BabyUITests", dependencies: ["BabyUI"]),
        .testTarget(name: "HomeFeatureTests", dependencies: ["HomeFeature"]),
        .testTarget(name: "LoggingFeatureTests", dependencies: ["LoggingFeature"]),
        .testTarget(name: "HistoryFeatureTests", dependencies: ["HistoryFeature"]),
        .testTarget(name: "InsightsFeatureTests", dependencies: ["InsightsFeature"]),
        .testTarget(name: "AskFeatureTests", dependencies: ["AskFeature"]),
        // BabySync for the Lock Screen card actions, which run the real
        // preflight over a fake family server.
        .testTarget(name: "BabyFeatureTests", dependencies: ["BabyFeature", "BabySync", "BabyProtocol"]),
        .testTarget(name: "SettingsFeatureTests", dependencies: ["SettingsFeature", "BabySync", "BabyStore", "BabyProtocol"]),
    ],
    swiftLanguageModes: [.v6]
)
