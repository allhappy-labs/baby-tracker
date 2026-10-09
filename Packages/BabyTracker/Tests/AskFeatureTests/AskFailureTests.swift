import Foundation
import Testing
import FoundationModels
@testable import AskFeature

@Test func generationErrorsMapToHowAskHandlesThem() {
    let context = LanguageModelSession.GenerationError.Context(debugDescription: "test")
    #expect(AskErrorKind(LanguageModelSession.GenerationError.exceededContextWindowSize(context)) == .contextOverflow)
    #expect(AskErrorKind(LanguageModelSession.GenerationError.guardrailViolation(context)) == .blocked)
    #expect(AskErrorKind(LanguageModelSession.GenerationError.unsupportedLanguageOrLocale(context)) == .unsupportedLanguage)
    #expect(AskErrorKind(LanguageModelSession.GenerationError.decodingFailure(context)) == .other)
    #expect(AskErrorKind(CancellationError()) == .other)
}

@Test func onlyPersonFacingKindsKeepTheirOwnFailure() {
    #expect(AskErrorKind.blocked.failure == .blocked)
    #expect(AskErrorKind.unsupportedLanguage.failure == .unsupportedLanguage)
    #expect(AskErrorKind.timedOut.failure == .timedOut)
    for kind in [AskErrorKind.contextOverflow, .other] {
        #expect(kind.failure == .failed)
    }
}
