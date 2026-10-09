import Foundation
import Testing
@testable import BabyCore

@Suite("Bottle component storage compatibility")
struct FeedComponentCodableTests {
    @Test func decodesExistingBottleWithoutSeconds() throws {
        let json = #"{"bottle":{"id":"30000000-0000-4000-8000-000000000001","volumeML":60,"contents":"formula"}}"#
        let component = try JSONDecoder().decode(FeedComponent.self, from: Data(json.utf8))
        #expect(component.volumeML == 60)
        #expect(component.bottleSeconds == nil)
    }

    @Test func timedBottleRoundTripsThroughLocalStorage() throws {
        let component = FeedComponent.bottle(id: UUID(), volumeML: 60, contents: .formula, seconds: 135)
        #expect(try JSONDecoder().decode(FeedComponent.self, from: JSONEncoder().encode(component)) == component)
        #expect(component.durationSeconds == nil)
        #expect(component.bottleSeconds == 135)
    }

    @Test func stepBoundariesRoundTripWithoutAssigningThemToOlderComponents() throws {
        let start = Date(timeIntervalSince1970: 1_780_000_000)
        let end = start.addingTimeInterval(135)
        let breast = FeedComponent.breast(id: UUID(), side: .left, seconds: 135, startedAt: start, endedAt: end)
        let bottle = FeedComponent.bottle(id: UUID(), volumeML: 60, contents: .formula, seconds: 135, startedAt: start, endedAt: end)
        let decoder = JSONDecoder()

        #expect(try decoder.decode(FeedComponent.self, from: JSONEncoder().encode(breast)) == breast)
        #expect(try decoder.decode(FeedComponent.self, from: JSONEncoder().encode(bottle)) == bottle)
        #expect(breast.startedAt == start && breast.endedAt == end)
        #expect(bottle.startedAt == start && bottle.endedAt == end)

        let old = FeedComponent.bottle(id: UUID(), volumeML: 60, contents: .formula, seconds: 135)
        #expect(try decoder.decode(FeedComponent.self, from: JSONEncoder().encode(old)).startedAt == nil)
        #expect(old.endedAt == nil)
    }
}
