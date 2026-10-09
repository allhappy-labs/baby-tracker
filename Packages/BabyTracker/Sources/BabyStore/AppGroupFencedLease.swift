import Foundation
import SwiftData

@MainActor
public final class AppGroupFencedLease: FencedLeaseStore {
    private let container: ModelContainer
    public let lockURL: URL

    public init(container: ModelContainer, lockURL: URL) {
        self.container = container
        self.lockURL = lockURL
    }

    public func acquire(scope: LeaseScope, ownerID: UUID, now: Date, ttl: TimeInterval) throws -> FencedLeaseToken? {
        try AppGroupBootstrapLock.withLock(at: lockURL) {
            let context = ModelContext(container)
            let key = scope.encodedKey
            if let row = try fetch(key, in: context) {
                if row.expiresAt > now, row.ownerID != ownerID { return nil }
                if row.expiresAt > now, row.ownerID == ownerID {
                    return FencedLeaseToken(scope: scope, ownerID: ownerID, generation: row.generation, expiresAt: row.expiresAt)
                }
                row.ownerID = ownerID
                row.generation &+= 1
                row.expiresAt = now.addingTimeInterval(ttl)
                try context.save()
                return FencedLeaseToken(scope: scope, ownerID: ownerID, generation: row.generation, expiresAt: row.expiresAt)
            }
            let row = FencedLease(scopeKey: key, ownerID: ownerID, generation: 1, expiresAt: now.addingTimeInterval(ttl))
            context.insert(row); try context.save()
            return FencedLeaseToken(scope: scope, ownerID: ownerID, generation: 1, expiresAt: row.expiresAt)
        }
    }

    public func renew(_ token: FencedLeaseToken, now: Date, ttl: TimeInterval) throws -> FencedLeaseToken {
        try AppGroupBootstrapLock.withLock(at: lockURL) {
            let context = ModelContext(container)
            guard let row = try fetch(token.scope.encodedKey, in: context), row.ownerID == token.ownerID, row.generation == token.generation else {
                throw FencedLeaseError.staleOwner
            }
            guard row.expiresAt > now else { throw FencedLeaseError.expired }
            row.expiresAt = now.addingTimeInterval(ttl); try context.save()
            return FencedLeaseToken(scope: token.scope, ownerID: token.ownerID, generation: token.generation, expiresAt: row.expiresAt)
        }
    }

    public func withFencedCommit<T>(_ token: FencedLeaseToken, now: Date, _ body: () throws -> T) throws -> T {
        try AppGroupBootstrapLock.withLock(at: lockURL) {
            let context = ModelContext(container)
            guard let row = try fetch(token.scope.encodedKey, in: context), row.ownerID == token.ownerID, row.generation == token.generation else {
                throw FencedLeaseError.staleOwner
            }
            guard row.expiresAt > now else { throw FencedLeaseError.expired }
            return try body()
        }
    }

    public func cancel(_ token: FencedLeaseToken) throws {
        try AppGroupBootstrapLock.withLock(at: lockURL) {
            let context = ModelContext(container)
            guard let row = try fetch(token.scope.encodedKey, in: context), row.ownerID == token.ownerID, row.generation == token.generation else {
                throw FencedLeaseError.staleOwner
            }
            row.expiresAt = .distantPast
            try context.save()
        }
    }

    private func fetch(_ key: String, in context: ModelContext) throws -> FencedLease? {
        try context.fetch(FetchDescriptor<FencedLease>(predicate: #Predicate { $0.scopeKey == key })).first
    }
}
