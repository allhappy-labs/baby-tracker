import Foundation
import Testing
import FoundationModels
@testable import AskFeature

@MainActor
@Test func transcriptReplaysInstructionsAndAnsweredTurnsInOrder() {
    let now = Date(timeIntervalSince1970: 1_800_000_000)
    let answered = AskTurn(id: UUID(), question: "Q1", answer: "A1", askedAt: now, answeredAt: now, sources: [])
    let unanswered = AskTurn(id: UUID(), question: "Q2", answer: nil, askedAt: now, answeredAt: nil, sources: [])
    let transcript = AppleAskModel.transcript(instructions: "Be brief.", history: [answered, unanswered])
    let kinds = transcript.map { entry -> String in
        switch entry {
        case .instructions: "instructions"
        case .prompt: "prompt"
        case .response: "response"
        default: "other"
        }
    }
    #expect(kinds == ["instructions", "prompt", "response"])
}
