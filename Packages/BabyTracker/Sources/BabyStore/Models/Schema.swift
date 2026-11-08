import Foundation
import BabyCore
import SwiftData

public enum BabySchema {
    public static let all: [any PersistentModel.Type] = SchemaV12.models

    /// The App Group is what lets the app, the widget extension, and Live
    /// Activity intents read one database instead of three.
    public static let appGroupID = AppIdentity.appGroupID

    /// Serialises store opening, because Core Data's is not thread-safe.
    ///
    /// Opening a store mutates state shared by every store built from the same
    /// object model, without a lock of its own:
    /// `-[NSSQLEntity_DerivedAttributesExtension _generateTriggerSQL]` writes
    /// into a plain `NSMutableDictionary` hanging off the entity while
    /// `-[NSSQLiteConnection connect]` is creating tables. Two containers opened
    /// at the same moment corrupt that dictionary and the process dies with
    /// `EXC_BAD_ACCESS` inside `-[__NSDictionaryM setObject:forKey:]`.
    ///
    /// That is the intermittent SIGSEGV that used to drop the whole
    /// `BabyStoreTests` suite — the change-log, tombstone and timer invariants —
    /// out of a run that still exited 0 and printed no crash line. Twelve
    /// captured crash reports carry that identical stack, with three threads
    /// inside `addPersistentStore` at once.
    ///
    /// The app opens one container, so the lock costs nothing in production. The
    /// test suite opens dozens in parallel, which is where it earns its keep.
    /// Every `ModelContainer` in this project — tests included — must be built
    /// through here; one built around it re-opens the race for everybody.
    private static let openLock = NSLock()

    public static func makeContainer(
        schema: Schema,
        migrationPlan: (any SchemaMigrationPlan.Type)? = nil,
        configurations: [ModelConfiguration]
    ) throws -> ModelContainer {
        try serialized {
            try ModelContainer(for: schema, migrationPlan: migrationPlan, configurations: configurations)
        }
    }

    /// Holds `openLock`. The incompatible-store check builds a managed object
    /// model and reads store metadata, so it stays out of the open's way too.
    private static func serialized<T>(_ body: () throws -> T) rethrows -> T {
        openLock.lock()
        defer { openLock.unlock() }
        return try body()
    }

    /// The App Group store, or an in-memory one.
    ///
    /// `policy` decides what happens to an App Group store written by another
    /// schema; see `IncompatibleStorePolicy`. It defaults to `.refuse`, so only
    /// a caller that asks for it — the app — ever moves a file.
    public static func container(
        inMemory: Bool,
        ifIncompatible policy: IncompatibleStorePolicy = .refuse
    ) throws -> ModelContainer {
        let schema = Schema(versionedSchema: SchemaV12.self)
        if inMemory {
            let configuration = ModelConfiguration(schema: schema, isStoredInMemoryOnly: true)
            return try makeContainer(schema: schema, migrationPlan: BabyStoreMigrationPlan.self, configurations: [configuration])
        }
        let configuration = ModelConfiguration(schema: schema, groupContainer: .identifier(appGroupID))
        let lockURL = defaultGroupDirectory.appending(path: "BabyTracker.bootstrap.lock")
        return try openOnDisk(schema: schema, configuration: configuration, lockURL: lockURL, policy: policy)
    }

    public static func container(
        storeURL: URL,
        bootstrapLockURL: URL? = nil,
        ifIncompatible policy: IncompatibleStorePolicy = .refuse
    ) throws -> ModelContainer {
        let schema = Schema(versionedSchema: SchemaV12.self)
        let configuration = ModelConfiguration(schema: schema, url: storeURL)
        let lockURL = bootstrapLockURL ?? storeURL.appendingPathExtension("bootstrap.lock")
        return try openOnDisk(schema: schema, configuration: configuration, lockURL: lockURL, policy: policy)
    }

    /// Under the bootstrap lock, so the widget, the intents and the app never
    /// open — or move — the store at the same time.
    private static func openOnDisk(
        schema: Schema,
        configuration: ModelConfiguration,
        lockURL: URL,
        policy: IncompatibleStorePolicy
    ) throws -> ModelContainer {
        try AppGroupBootstrapLock.withLock(at: lockURL) {
            try serialized {
                try IncompatibleStore.resolve(
                    storeURL: configuration.url, schemas: BabyStoreMigrationPlan.schemas, policy: policy
                )
            }
            return try makeContainer(schema: schema, migrationPlan: BabyStoreMigrationPlan.self, configurations: [configuration])
        }
    }

    public static var defaultGroupDirectory: URL {
        FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: appGroupID)
            ?? FileManager.default.temporaryDirectory.appending(path: appGroupID, directoryHint: .isDirectory)
    }
}
