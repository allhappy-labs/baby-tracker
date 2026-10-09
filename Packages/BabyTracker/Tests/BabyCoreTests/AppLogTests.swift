import Testing
import Foundation
@testable import BabyCore

@Test func entryDescriptionsCarryNoContent() {
    let id = UUID()
    let description = AppLog.describe(entryKind: "FeedEntry", id: id)

    // Kind and id only — enough to correlate, useless to a snooper.
    #expect(description.contains("FeedEntry"))
    #expect(description.contains(id.uuidString))
}

@Test func describedEntriesCannotLeakValues() {
    // The helper takes no value parameters at all, which is the actual control.
    // This test pins the shape: a caller has nowhere to put a volume or a note.
    let id = UUID()
    let description = AppLog.describe(entryKind: "FeedEntry", id: id)

    #expect(!description.contains("ml"))
    #expect(!description.contains(":"))          // no clock times
    #expect(description.split(separator: " ").count == 2)
}

@Test func everyCategoryIsDistinct() {
    let categories = AppLog.allCategoryNames
    #expect(Set(categories).count == categories.count)
    #expect(categories.contains("store"))
    #expect(categories.contains("timers"))
    #expect(categories.contains("ui"))
}
