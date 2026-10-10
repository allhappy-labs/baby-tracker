import Foundation
import Testing
@testable import BabySync

@MainActor
@Suite("Background wake")
struct BackgroundWakeHandlerTests {
    @Test func aPushRunsOneSyncAndReportsNewData() async {
        var syncs = 0
        let handler = BackgroundWakeHandler(sync: { syncs += 1; return true })
        #expect(await handler.receiveSilentPush() == .newData)
        #expect(syncs == 1)
    }

    @Test func aFailedSyncReportsFailed() async {
        let handler = BackgroundWakeHandler(sync: { false })
        #expect(await handler.receiveSilentPush() == .failed)
    }

    /// The answer comes at the deadline, not when the sync ends. Asserted by
    /// the sync never finishing rather than by a tight wall-clock bound: other
    /// suites in this target block the main actor for seconds at a time (the
    /// lease probe's `waitForExit`), which delays when this test resumes but
    /// not what the handler answered.
    @Test func aSyncPastTheDeadlineReportsFailedInTime() async {
        var finished = false
        let handler = BackgroundWakeHandler(sync: {
            do { try await Task.sleep(for: .seconds(30)); finished = true } catch {}
            return true
        }, deadline: .milliseconds(100))
        let start = ContinuousClock.now
        #expect(await handler.receiveSilentPush() == .failed)
        #expect(!finished)
        #expect(ContinuousClock.now - start < .seconds(20))
    }
}
