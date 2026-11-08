import Foundation

/// What a log line may say about a sync failure: its kind, never its text.
/// A description can carry a URL, a name or a note.
enum SyncLogKind {
    static func of(_ error: any Error) -> String {
        switch error {
        case let code as SyncErrorCode: code.rawValue
        case let error as HTTPClientError: String(describing: error)
        case let error as SessionRefresher.RefreshError: "RefreshError.\(error)"
        case let error as URLError: "URLError \(error.code.rawValue)"
        default: String(reflecting: type(of: error))
        }
    }
}
