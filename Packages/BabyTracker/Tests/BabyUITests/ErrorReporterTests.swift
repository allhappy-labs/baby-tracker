import Testing
import Foundation
@testable import BabyUI

private struct Boom: Error {}

@MainActor
@Test func reportingSurfacesTheMessage() {
    let reporter = ErrorReporter()
    #expect(reporter.current == nil)

    reporter.report(ErrorBannerCopy.saveFailed(what: "diaper change"), error: Boom())
    #expect(reporter.current == "Couldn't save that diaper change")
}

@MainActor
@Test func aNewerFailureReplacesAnOlderOne() {
    // The most recent thing the user tried is the one they care about.
    let reporter = ErrorReporter()
    reporter.report(ErrorBannerCopy.saveFailed(what: "feed"), error: Boom())
    reporter.report(ErrorBannerCopy.deleteFailed(what: "diaper change"), error: Boom())

    #expect(reporter.current == "Couldn't delete that diaper change")
}

@MainActor
@Test func dismissClearsIt() {
    let reporter = ErrorReporter()
    reporter.report(ErrorBannerCopy.loadFailed, error: Boom())
    reporter.dismiss()

    #expect(reporter.current == nil)
}

@MainActor
@Test func aSuccessfulWriteClearsAnEarlierFailure() {
    // The stale-banner case: a save fails, the user retries, the retry works.
    // What is on screen must follow the second outcome, not the first — a
    // banner that outlives the problem makes a parent re-log and duplicate.
    // The loop is deliberately the exact shape every converted call site has.
    let reporter = ErrorReporter()
    var attempt = 0
    func save() throws {
        attempt += 1
        if attempt == 1 { throw Boom() }
    }

    for _ in 1...2 {
        do {
            try save()
            reporter.clear()
        } catch {
            reporter.report(
                ErrorBannerCopy.saveFailed(what: "diaper change"),
                error: error
            )
        }
    }

    #expect(attempt == 2)
    #expect(reporter.current == nil)
}
