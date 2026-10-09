import Foundation

/// v1's implementation. Accepts everything so the outbox drains, returns
/// nothing because there are no other devices yet.
public struct LocalOnlySync: SyncClient {
    public init() {}

    public func push(_ changes: [PendingChange]) async throws -> [UUID] {
        changes.map(\.id)
    }

    public func pull(since: Date?) async throws -> [PendingChange] {
        []
    }
}
