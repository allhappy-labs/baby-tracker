import CoreData
import Foundation
import SwiftData
import BabyCore

/// What opening an on-disk store does when the file there was written by a
/// schema other than the baseline.
public enum IncompatibleStorePolicy: Sendable {
    /// The app: rename the store and its `-wal`/`-shm` files aside — never
    /// delete them — and open a fresh store, which sync repopulates.
    case moveAside
    /// The widget and the intents: touch nothing and report the store as
    /// unavailable. Only the app decides what happens to a parent's data.
    case refuse
}

public enum StoreOpenError: Error, Equatable {
    /// The store on disk was written by another schema and the caller asked
    /// not to move it.
    case incompatibleSchema
}

/// Finds a store written by another schema and, for the app, moves it aside.
///
/// Decided from the store's own metadata, before anything opens it, rather
/// than from an open error: SwiftData opens an older store by inferring a
/// lightweight migration — silently, although no migration plan lists its
/// version — so an older store never fails to open. The check compares the
/// entity version hashes the store recorded with the model of every schema
/// the migration plan lists; a store matching none of them is incompatible.
/// Today that is V9 (the baseline) and V10: a V9 store matches the plan and
/// is migrated in place by the lightweight stage, not moved.
///
/// It acts only on positive evidence. No file, metadata that cannot be read
/// (not a store, or the device still locked) or no model to compare with all
/// leave the file alone, and the open then reports whatever is wrong.
enum IncompatibleStore {
    /// Runs under the bootstrap lock, immediately before the open.
    static func resolve(
        storeURL: URL,
        schemas: [any VersionedSchema.Type],
        policy: IncompatibleStorePolicy,
        now: Date = Date()
    ) throws {
        guard isIncompatible(storeURL: storeURL, schemas: schemas) else { return }
        switch policy {
        case .refuse:
            throw StoreOpenError.incompatibleSchema
        case .moveAside:
            let moved = try moveAside(storeURL: storeURL, at: now)
            AppLog.store.notice("moved a store written by another schema aside: \(moved.count, privacy: .public) files")
        }
    }

    static func isIncompatible(storeURL: URL, schemas: [any VersionedSchema.Type]) -> Bool {
        guard FileManager.default.fileExists(atPath: storeURL.path),
              let metadata = try? NSPersistentStoreCoordinator.metadataForPersistentStore(type: .sqlite, at: storeURL)
        else { return false }
        let models = schemas.map { NSManagedObjectModel.makeManagedObjectModel(for: Schema(versionedSchema: $0)) }
        // A schema whose model cannot be built is no evidence either way.
        guard !models.contains(where: { $0 == nil }) else { return false }
        return !models.contains { $0!.isConfiguration(withName: nil, compatibleWithStoreMetadata: metadata) }
    }

    /// Renames `default.store`, `default.store-wal` and `default.store-shm`
    /// to `default.store.incompatible-<UTC timestamp>` plus the same `-wal`
    /// and `-shm` suffixes, so SQLite still opens the three as one store.
    ///
    /// The WAL and shared memory go first. A crash between renames then
    /// leaves the main file without a WAL — still readable, and moved on the
    /// next launch — never a WAL beside the fresh store, which SQLite would
    /// replay into it.
    @discardableResult
    static func moveAside(storeURL: URL, at now: Date) throws -> [URL] {
        let fileManager = FileManager.default
        let directory = storeURL.deletingLastPathComponent()
        let stem = "\(storeURL.lastPathComponent).incompatible-\(timestamp(now))"
        var moved: [URL] = []
        for suffix in ["-wal", "-shm", ""] {
            let source = directory.appending(path: storeURL.lastPathComponent + suffix)
            guard fileManager.fileExists(atPath: source.path) else { continue }
            let destination = directory.appending(path: stem + suffix)
            try fileManager.moveItem(at: source, to: destination)
            moved.append(destination)
        }
        return moved
    }

    /// `20261003T101500Z`: sorts by time, and has no `:` for a file name.
    static func timestamp(_ date: Date) -> String {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withYear, .withMonth, .withDay, .withTime, .withTimeZone]
        formatter.timeZone = TimeZone(identifier: "UTC")
        return formatter.string(from: date)
    }
}
