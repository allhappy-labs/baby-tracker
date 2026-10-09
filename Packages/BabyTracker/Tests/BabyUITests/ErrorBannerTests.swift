import Testing
import SwiftUI
@testable import BabyUI

@Test func bannerMessagesAreFactualNotEvaluative() {
    // The app never tells a tired parent they did something wrong.
    let forbidden = ["invalid", "you must", "failed to comply", "error:", "illegal"]
    let samples = [
        ErrorBannerCopy.saveFailed(what: "diaper change"),
        ErrorBannerCopy.deleteFailed(what: "feed"),
        ErrorBannerCopy.loadFailed,
    ]
    for sample in samples {
        let lowered = sample.lowercased()
        for word in forbidden {
            #expect(!lowered.contains(word), "\(sample) contains \(word)")
        }
        #expect(sample.first!.isUppercase)
    }
}

@Test func saveFailureNamesTheThingThatFailed() {
    #expect(ErrorBannerCopy.saveFailed(what: "diaper change") == "Couldn't save that diaper change")
    #expect(ErrorBannerCopy.deleteFailed(what: "feed") == "Couldn't delete that feed")
}
