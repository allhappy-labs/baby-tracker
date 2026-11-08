import Testing
import Foundation
@testable import BabyCore

@Suite struct LogLinkTests {
    private let baby = UUID(uuidString: "0F1E2D3C-4B5A-6978-8796-A5B4C3D2E1F0")!

    @Test func aLinkRoundTripsThroughItsURL() throws {
        for kind in HomeActionKind.allCases {
            let link = LogLink(kind: kind, babyID: baby)
            #expect(LogLink.isLogLink(link.url))
            #expect(LogLink(url: link.url) == link)
        }
    }

    @Test func aLinkTheAppCannotReadIsStillALogLink() throws {
        // An action a later widget knows, a broken baby, nothing at all:
        // recognised as a log link, so the app opens Home rather than
        // treating it as an invitation, and parsed as nothing.
        for text in ["babytracker://log?kind=bath&baby=\(baby.uuidString)",
                     "babytracker://log?kind=solids&baby=not-a-uuid",
                     "babytracker://log"] {
            let url = try #require(URL(string: text))
            #expect(LogLink.isLogLink(url))
            #expect(LogLink(url: url) == nil)
        }
    }

    @Test func anInvitationIsNotALogLink() throws {
        let url = try #require(URL(string: "babytracker://invite?server=https%3A%2F%2Fexample.org&token=abc"))
        #expect(!LogLink.isLogLink(url))
        #expect(LogLink(url: url) == nil)
    }
}
