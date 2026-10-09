import Foundation
import FoundationModels

/// Why Ask could not answer, as the person sees it.
public enum AskFailure: Error, Equatable, Sendable {
    case blocked
    case unsupportedLanguage
    case timedOut
    case failed
}

/// How `AppleAskModel` handles a model error.
enum AskErrorKind: String, Equatable, Sendable {
    case contextOverflow
    case blocked
    case unsupportedLanguage
    case timedOut
    case other

    init(_ error: any Error) {
        if let toolError = error as? LanguageModelSession.ToolCallError {
            self.init(toolError.underlyingError)
            return
        }
        if #available(iOS 27.0, macOS 27.0, *), let error = error as? LanguageModelError {
            switch error {
            case .contextSizeExceeded: self = .contextOverflow
            case .guardrailViolation, .refusal: self = .blocked
            case .unsupportedLanguageOrLocale: self = .unsupportedLanguage
            case .timeout: self = .timedOut
            default: self = .other
            }
            return
        }
        if let error = error as? LanguageModelSession.GenerationError {
            switch error {
            case .exceededContextWindowSize: self = .contextOverflow
            case .guardrailViolation, .refusal: self = .blocked
            case .unsupportedLanguageOrLocale: self = .unsupportedLanguage
            default: self = .other
            }
            return
        }
        self = .other
    }

    var failure: AskFailure {
        switch self {
        case .blocked: .blocked
        case .unsupportedLanguage: .unsupportedLanguage
        case .timedOut: .timedOut
        case .contextOverflow, .other: .failed
        }
    }
}
