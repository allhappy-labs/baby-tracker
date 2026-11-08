import Foundation
import BabyCore
import BabyStore
import BabyUI

enum HomeL10n {
    static func text(_ key: String) -> String {
        NSLocalizedString(key, bundle: .module, comment: "")
    }

    static func format(_ key: String, _ arguments: CVarArg...) -> String {
        String(format: text(key), locale: .current, arguments: arguments)
    }

    static func breast(_ side: BreastSide) -> String {
        text(side == .left ? "Left breast" : "Right breast")
    }
}

/// Home's copy. Strings never live in a view — this project found thirteen
/// copy defects by reading rendered strings, and a string baked into a `View`
/// cannot be read by a test.
enum HomeCopy {
    static let readOnly = HomeL10n.text("You have view-only access to this family. The family owner can let you log entries.")
    static let awake = HomeL10n.text("Awake")
    static let asleep = HomeL10n.text("Asleep")
    static let feedBreak = HomeL10n.text("Break")
    static let bottle = HomeL10n.text("Bottle")
    static let stop = HomeL10n.text("Stop")
    static let stopPumping = HomeL10n.text("Stop pumping")

    static let wakeUp = HomeL10n.text("Wake up")
    static let more = HomeL10n.text("More")
    static let today = HomeL10n.text("Today")
    /// The today strip's right edge.
    static let now = HomeL10n.text("now")
    static let suggested = HomeL10n.text("Suggested")
    static let editStartTime = HomeL10n.text("Edit start time")

    /// The running part's name beside its glyph: "Left breast", "Bottle".
    /// A sleep part (the "Same sleep" chip) is "Asleep", never a bottle.
    static func partTitle(_ icon: BabyIcon) -> String {
        switch icon {
        case .breastLeft: HomeL10n.breast(.left)
        case .breastRight: HomeL10n.breast(.right)
        case .sleep: asleep
        default: bottle
        }
    }

    static func startBreast(_ side: BreastSide) -> String {
        HomeL10n.text(side == .left ? "Start left breast" : "Start right breast")
    }

    static func same(_ kind: SessionKind) -> String {
        HomeL10n.text(kind == .feed ? "Same feed" : "Same sleep")
    }

    static func makeSeparate(_ kind: SessionKind) -> String {
        HomeL10n.text(kind == .feed ? "Make it a separate feed" : "Make it a separate sleep")
    }

    /// Time since an event, as Home shows it: "2h 5m", "1m" — and "0m" under
    /// a minute. Home shows times only, so `ElapsedFormatter.compact`'s
    /// "just now" (still right for other screens) never appears here.
    static func elapsed(since: Date, now: Date) -> String {
        now.timeIntervalSince(since) < 60
            ? ElapsedFormatter.total(0)
            : ElapsedFormatter.compact(since: since, now: now)
    }

    /// The status line's "23m ago". A bare time beside an icon read as a
    /// length, not as time since. A whole phrase, because German and French
    /// put the word first: "vor 23m".
    static func ago(_ elapsed: String) -> String {
        HomeL10n.format("%@ ago", elapsed)
    }

    /// "since 3:02": the clock time a running part or break began.
    static func since(_ date: Date) -> String {
        HomeL10n.format("since %@", date.formatted(date: .omitted, time: .shortened))
    }

    /// An hour on the today strip's axis: "06" or "6 AM", by the device's
    /// clock setting.
    static func hour(_ date: Date) -> String {
        date.formatted(.dateTime.hour())
    }

    /// A clock time in the zone the entry was recorded in.
    static func clockTime(_ date: Date, zone: String) -> String {
        var formatter = Date.FormatStyle(date: .omitted, time: .shortened)
        formatter.timeZone = TimeZone(identifier: zone) ?? .gmt
        return date.formatted(formatter)
    }

    /// A part's length as the chip shows it: "9:02".
    static func partTime(_ seconds: Int) -> String {
        ElapsedFormatter.timer(seconds)
    }

    /// A part as VoiceOver reads it: "Left breast, 9 minutes, 2 seconds".
    static func spokenPart(_ part: PartSummary) -> String {
        partTitle(part.icon) + ", " + spokenDuration(TimeInterval(part.seconds), includeSeconds: true)
    }

    /// A length spelled out for VoiceOver — "2 hours, 10 minutes" — in the
    /// device language, by the system rather than this catalog.
    static func spokenDuration(_ seconds: TimeInterval, includeSeconds: Bool = false) -> String {
        let formatter = DateComponentsFormatter()
        formatter.unitsStyle = .full
        formatter.allowedUnits = includeSeconds ? [.hour, .minute, .second] : [.hour, .minute]
        formatter.zeroFormattingBehavior = .dropAll
        return formatter.string(from: max(0, seconds)) ?? ""
    }

    /// The baby's age for the header — "5 days", "7 weeks", "4 months",
    /// "2 years" — worded by the system in the device language.
    static func age(birthDate: Date, now: Date) -> String? {
        let calendar = Calendar.current
        let days = calendar.dateComponents([.day], from: calendar.startOfDay(for: birthDate),
                                           to: calendar.startOfDay(for: now)).day ?? 0
        guard days >= 0 else { return nil }
        let formatter = DateComponentsFormatter()
        formatter.unitsStyle = .full
        formatter.maximumUnitCount = 1
        var components = DateComponents()
        switch days {
        case ..<14:
            formatter.allowedUnits = [.day]
            components.day = days
        case ..<91:
            formatter.allowedUnits = [.weekOfMonth]
            components.weekOfMonth = days / 7
        default:
            let months = calendar.dateComponents([.month], from: birthDate, to: now).month ?? 0
            if months < 24 {
                formatter.allowedUnits = [.month]
                components.month = months
            } else {
                formatter.allowedUnits = [.year]
                components.year = months / 12
            }
        }
        return formatter.string(from: components)
    }
}
