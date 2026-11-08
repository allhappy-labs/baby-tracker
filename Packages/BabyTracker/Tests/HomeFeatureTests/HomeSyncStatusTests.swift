import Foundation
import Testing
@testable import HomeFeature

@Suite("Home sync status")
struct HomeSyncStatusTests {
    @Test func signInRequiredKeepsQueuedEntries() {
        let status = HomeSyncStatus.resolve(.needsSignIn, pendingDeliveryCount: 2)
        #expect(status == .needsSignIn(2))
        #expect(status.offersSignIn)
        #expect(status.showsQueuedExplanation)
    }

    @Test func routineSyncStaysOffHome() {
        let date = Date(timeIntervalSince1970: 1_700_000_000)
        for phase in [HomeSyncPhase.idle, .syncing, .current(date), .renewing] {
            #expect(HomeSyncStatus.resolve(phase, pendingDeliveryCount: 0) == .hidden)
            #expect(HomeSyncStatus.resolve(phase, pendingDeliveryCount: 3) == .hidden)
        }
    }

    @Test func offlineKeepsQueuedEntries() {
        let status = HomeSyncStatus.resolve(.offline, pendingDeliveryCount: 1)
        #expect(status == .offline(1))
        #expect(status.showsQueuedExplanation)
        #expect(!status.offersSignIn)
    }

    @Test func serverUnreachableKeepsQueuedEntries() {
        let status = HomeSyncStatus.resolve(.serverUnreachable, pendingDeliveryCount: 2)
        #expect(status == .serverUnreachable(2))
        #expect(status.showsQueuedExplanation)
        #expect(!status.offersSignIn)
    }

    @Test func unknownFailureCannotShowServerText() {
        let status = HomeSyncStatus.resolve(.failed("private server response"), pendingDeliveryCount: 1)
        #expect(status == .recoveryFailed("retry"))
        #expect(!status.offersSignIn)
    }
}
