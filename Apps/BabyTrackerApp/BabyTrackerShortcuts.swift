import AppIntents

struct BabyTrackerShortcuts: AppShortcutsProvider {
    static var appShortcuts: [AppShortcut] {
        AppShortcut(
            intent: SiriLogDiaperIntent(kind: .wet),
            phrases: ["Log a wet diaper in \(.applicationName)"],
            shortTitle: "Log wet diaper",
            systemImageName: "drop.fill"
        )
        AppShortcut(
            intent: SiriLogDiaperIntent(kind: .dirty),
            phrases: ["Log a dirty diaper in \(.applicationName)"],
            shortTitle: "Log dirty diaper",
            systemImageName: "circle.fill"
        )
        AppShortcut(
            intent: SiriLogDiaperIntent(kind: .mixed),
            phrases: ["Log a mixed diaper in \(.applicationName)"],
            shortTitle: "Log mixed diaper",
            systemImageName: "circle.lefthalf.filled"
        )
        AppShortcut(
            intent: SiriStartBreastIntent(side: .left),
            phrases: ["Start left breast feeding in \(.applicationName)"],
            shortTitle: "Start left breast feeding",
            systemImageName: "l.circle.fill"
        )
        AppShortcut(
            intent: SiriStartBreastIntent(side: .right),
            phrases: ["Start right breast feeding in \(.applicationName)"],
            shortTitle: "Start right breast feeding",
            systemImageName: "r.circle.fill"
        )
        AppShortcut(
            intent: SiriStopFeedIntent(),
            phrases: ["Stop feeding in \(.applicationName)"],
            shortTitle: "Stop feeding",
            systemImageName: "stop.fill"
        )
        AppShortcut(
            intent: SiriStartSleepIntent(),
            phrases: ["Start sleep in \(.applicationName)"],
            shortTitle: "Start sleep",
            systemImageName: "moon.fill"
        )
        AppShortcut(
            intent: SiriStopSleepIntent(),
            phrases: ["Wake up in \(.applicationName)"],
            shortTitle: "Wake up",
            systemImageName: "sun.max.fill"
        )
    }
}
