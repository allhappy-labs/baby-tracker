import Foundation
import Testing
@testable import BabySync

@Suite("Native pong cancellation")
struct LiveSocketCancellationTests {
    @Test func cancelledBeforePongRegistrationDoesNotSend() async {
        let sent = Locked(false)
        let task = Task {
            withUnsafeCurrentTask { $0?.cancel() }
            do {
                try await URLSessionLiveSocketConnector.awaitPong { callback in
                    sent { $0 = true }
                    callback(nil)
                }
                return false
            } catch is CancellationError { return true }
            catch { return false }
        }
        #expect(await task.value)
        #expect(!sent { $0 })
    }

    @Test func cancelledMissingPongFinishesBeforeLateDuplicateCallbacks() async throws {
        let callback = Locked<(@Sendable ((any Error)?) -> Void)?>(nil)
        let outcome = Locked<Bool?>(nil)
        let task = Task {
            do {
                try await URLSessionLiveSocketConnector.awaitPong { handler in callback { $0 = handler } }
                outcome { $0 = false }
            } catch is CancellationError { outcome { $0 = true } }
            catch { outcome { $0 = false } }
        }
        try await until { callback { $0 != nil } }
        task.cancel()
        // Retain the callback until after the bounded assertion. The old
        // implementation is released below too, so RED cannot hang the suite.
        try await until { outcome { $0 != nil } }
        #expect(outcome { $0 } == true)
        let late = callback { $0 }
        late?(nil)
        late?(URLError(.networkConnectionLost))
        await task.value
        #expect(outcome { $0 } == true)
    }

    @Test func cancellationDuringPongRegistrationWinsBeforeCallback() async {
        let task = Task {
            do {
                try await URLSessionLiveSocketConnector.awaitPong { callback in
                    withUnsafeCurrentTask { $0?.cancel() }
                    callback(nil)
                    callback(URLError(.networkConnectionLost))
                }
                return false
            } catch is CancellationError { return true }
            catch { return false }
        }
        #expect(await task.value)
    }
}
