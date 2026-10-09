import Foundation
import BabyCore

/// The three lines the Home Screen status widget shows, and the only place
/// their copy lives.
///
/// It sits in the package rather than beside the widget so it can be tested:
/// the widget extension is not a test target, and the bug this type exists to
/// prevent — the widget rendering plausible numbers nobody logged — is
/// invisible to every other suite.
public struct WidgetStatusSummary: Equatable, Sendable {
    /// Rendered wherever there is no value to render. An em-dash, not a zero
    /// and not a guess: a parent may act on what this widget says.
    public static let blank = "—"

    public let babyName: String
    public let feedAge: String
    public let sleepText: String
    public let diaperAge: String

    /// What the timeline shows when there is nothing to show: no baby yet on a
    /// fresh install, or a store read that failed.
    ///
    /// Deliberately not the same value as the widget gallery's placeholder.
    /// The gallery previews sample data because that is what a gallery is for;
    /// a placed widget showing "2h 14m" from a failed read is a fabrication,
    /// and the name is blank too so it cannot be attributed to a child.
    public static let noData = WidgetStatusSummary(
        babyName: "",
        feedAge: blank,
        sleepText: blank,
        diaperAge: blank
    )

    public init(babyName: String, feedAge: String, sleepText: String, diaperAge: String) {
        self.babyName = babyName
        self.feedAge = feedAge
        self.sleepText = sleepText
        self.diaperAge = diaperAge
    }

    /// Builds the three lines from a snapshot.
    ///
    /// `now` comes from the store's `DateProvider`, never a bare `Date()`.
    @MainActor
    public init(babyName: String, snapshot: HomeSnapshot, now: Date) {
        // "How long ago did this happen?" — an AGE, so `compact`.
        func age(_ date: Date?) -> String {
            guard let date else { return Self.blank }
            return ElapsedFormatter.compact(since: date, now: now)
        }

        // "Asleep 2h 14m" is how long the nap has LASTED so far — a running
        // duration, not an age — so it takes `total`, matching how Home
        // renders the same quantity. "Awake 40m" below is an age.
        let sleep: String = if let timer = snapshot.sleepTimer {
            String(format: String(localized: "Asleep %@", bundle: .module), ElapsedFormatter.total(timer.elapsedSeconds(now: now)))
        } else if let awakeSince = snapshot.awakeSince {
            String(format: String(localized: "Awake %@", bundle: .module), age(awakeSince))
        } else {
            Self.blank
        }

        self.init(
            babyName: babyName,
            feedAge: age(snapshot.lastFeedingSession?.endedAt ?? snapshot.lastFeed?.endedAt ?? snapshot.lastFeed?.startedAt),
            sleepText: sleep,
            diaperAge: age(snapshot.lastDiaper?.startedAt)
        )
    }
}
