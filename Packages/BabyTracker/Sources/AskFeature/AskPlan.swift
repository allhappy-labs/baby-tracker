import Foundation
import FoundationModels

/// The log reads one question needs, chosen by the model with guided
/// generation. The app runs the reads; the model never has to remember to.
@Generable
struct AskPlan {
    @Guide(description: "Reads of this child's care log needed to answer the latest question. Empty for greetings, thanks, general questions that don't depend on the log, and questions the running timers already answer.",
           .maximumCount(3))
    var reads: [AskRead]

    init(reads: [AskRead]) {
        self.reads = reads
    }

    /// The model sometimes repeats a read with an ignored field changed.
    /// Only the fields a read actually uses tell two reads apart.
    var uniqueReads: [AskRead] {
        var seen = Set<String>()
        return reads.filter { read in
            let key: String = switch read.tool {
            case "latest": "latest"
            case "entries": "entries|\(read.range)|\(read.range == "date" ? read.date : "")|\(read.kind)"
            default: "totals|\(read.range)|\(read.range == "date" ? read.date : "")|\(read.byDay)"
            }
            return seen.insert(key).inserted
        }
    }
}

@Generable
struct AskRead: Equatable {
    @Guide(description: "totals for how many, how much, comparisons, trends and summaries; entries for individual entries and their times; latest for when the last feed, sleep, diaper, med, vaccine or pumping session was",
           .anyOf(["totals", "entries", "latest"]))
    var tool: String
    @Guide(description: "The period to read; date for one specific day", .anyOf(AskCareLog.ranges))
    var range: String
    @Guide(description: "The day as YYYY-MM-DD when range is date; otherwise empty")
    var date: String
    @Guide(description: "Which entries, for entries reads; vaccine lists every vaccination the baby has had; pump is the parent's milk pumping, not a feed",
           .anyOf(["all", "feed", "sleep", "diaper", "med", "vaccine", "pump"]))
    var kind: String
    @Guide(description: "true for one totals line per day")
    var byDay: Bool

    init(tool: String, range: String = "today", date: String = "", kind: String = "all", byDay: Bool = false) {
        self.tool = tool
        self.range = range
        self.date = date
        self.kind = kind
        self.byDay = byDay
    }
}
