import CoreData
import Foundation
import SwiftData
import Testing
import BabyCore
@testable import BabyStore

/// The baseline's feed, named apart from `OlderSchema.FeedEntry`.
private typealias LiveFeedEntry = FeedEntry

/// The planned schemas (V9 baseline, V10), and what happens to a store
/// written by any other.
///
/// SwiftData left alone would open an older store by inferring a lightweight
/// migration, silently, even though no migration plan lists its version. So
/// the app moves such a store aside before opening — never deleting it — and
/// the widget and intents refuse to open it at all.
///
/// On the main actor and serialized. Off the main actor, these opens of an
/// older-schema store ran on the cooperative pool while main-actor store tests
/// saved elsewhere in the process, and Core Data occasionally aborted the
/// whole test binary (SIGABRT, seen beside `LiveCardSequenceTests`). Every
/// test in this target that saves a live model now runs on the main actor —
/// the `BabyStore` suites because the store is main-actor, and `ModelTests`
/// by annotation — so none of them can save while this suite opens a store.
/// Serialized too, so no two of its own containers overlap. Only
/// `ContainerFailureTests` still opens off the main actor; it never saves,
/// and its opens go through `BabySchema.makeContainer`'s lock.
@Suite("Baseline store", .serialized)
@MainActor
struct BaselineStoreTests {
    /// The shape an earlier build wrote: same entity names as the baseline,
    /// with the legacy feed columns the baseline dropped. Its version is not
    /// in the migration plan.
    enum OlderSchema: VersionedSchema {
        static let versionIdentifier = Schema.Version(8, 0, 0)
        static var models: [any PersistentModel.Type] {
            SchemaV10.models.filter { $0 != LiveFeedEntry.self } + [OlderSchema.FeedEntry.self]
        }

        @Model final class FeedEntry {
            var id: UUID = UUID()
            var realmKey: String = RealmID.local.encodedKey
            var babyID: UUID = UUID()
            var startedAt: Date = Date()
            var endedAt: Date?
            var feedingSessionID: UUID?
            var timeZoneID: String = "UTC"
            var note: String?
            var createdBy: String = ""
            var lastModifiedAt: Date = Date()
            var isTombstoned: Bool = false
            var kindRaw: String = FeedKind.bottle.rawValue
            var components: [FeedComponent] = []
            var volumeML: Double?
            var contentsRaw: String?

            init(id: UUID) { self.id = id }
        }
    }

    // MARK: The baseline

    @Test func theMigrationPlanHasV9ToV12WithThreeStages() {
        #expect(BabyStoreMigrationPlan.schemas.map { $0.versionIdentifier }
                == [Schema.Version(9, 0, 0), Schema.Version(10, 0, 0), Schema.Version(11, 0, 0), Schema.Version(12, 0, 0)])
        #expect(BabyStoreMigrationPlan.stages.count == 3)
    }

    /// An install from before immunizations opens in place: nothing moved
    /// aside, its rows kept, and a vaccination can then be saved.
    @Test func aV10StoreMigratesInPlaceToV11() throws {
        let directory = try Self.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let url = directory.appending(path: "default.store")
        let babyID = UUID()
        let supplementID = UUID()
        try autoreleasepool {
            let schema = Schema(versionedSchema: SchemaV10.self)
            let container = try BabySchema.makeContainer(schema: schema, configurations: [ModelConfiguration(schema: schema, url: url)])
            let context = ModelContext(container)
            context.insert(Baby(id: babyID, name: "Mira", birthDate: .now))
            context.insert(SupplementEntry(id: supplementID, babyID: babyID, name: "Vitamin D", startedAt: .now,
                                           timeZoneID: "UTC", loggedAt: .now))
            try context.save()
        }

        let reopened = ModelContext(try BabySchema.container(storeURL: url, ifIncompatible: .moveAside))

        #expect(try reopened.fetch(FetchDescriptor<Baby>()).map(\.id) == [babyID])
        #expect(try reopened.fetch(FetchDescriptor<SupplementEntry>()).map(\.id) == [supplementID])
        #expect(try reopened.fetch(FetchDescriptor<ImmunizationEntry>()).isEmpty)
        reopened.insert(ImmunizationEntry(babyID: babyID, vaccine: "Prevenar 13", dose: 1, givenAt: .now, timeZoneID: "UTC"))
        try reopened.save()
        #expect(try reopened.fetch(FetchDescriptor<ImmunizationEntry>()).count == 1)
        #expect(try Self.movedAside(in: directory).isEmpty)
        #expect(try Self.isBaseline(url))
    }

    /// An install from before pumping opens in place: nothing moved aside,
    /// its rows kept, and a pump can then be saved.
    @Test func aV11StoreMigratesInPlaceToV12() throws {
        let directory = try Self.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let url = directory.appending(path: "default.store")
        let babyID = UUID()
        let shotID = UUID()
        try autoreleasepool {
            let schema = Schema(versionedSchema: SchemaV11.self)
            let container = try BabySchema.makeContainer(schema: schema, configurations: [ModelConfiguration(schema: schema, url: url)])
            let context = ModelContext(container)
            context.insert(Baby(id: babyID, name: "Mira", birthDate: .now))
            context.insert(ImmunizationEntry(id: shotID, babyID: babyID, vaccine: "Prevenar 13", dose: 1, givenAt: .now, timeZoneID: "UTC"))
            try context.save()
        }

        let reopened = ModelContext(try BabySchema.container(storeURL: url, ifIncompatible: .moveAside))

        #expect(try reopened.fetch(FetchDescriptor<Baby>()).map(\.id) == [babyID])
        #expect(try reopened.fetch(FetchDescriptor<ImmunizationEntry>()).map(\.id) == [shotID])
        #expect(try reopened.fetch(FetchDescriptor<PumpEntry>()).isEmpty)
        reopened.insert(PumpEntry(startedAt: .now, timeZoneID: "UTC", side: .both))
        try reopened.save()
        #expect(try reopened.fetch(FetchDescriptor<PumpEntry>()).count == 1)
        #expect(try Self.movedAside(in: directory).isEmpty)
        #expect(try Self.isBaseline(url))
    }

    /// An install from before icons opens in place: nothing moved aside,
    /// the baby kept, its icon nil until the fill-in pass gives it one.
    @Test func aBaselineStoreMigratesInPlaceToV10() throws {
        let directory = try Self.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let url = directory.appending(path: "default.store")
        let babyID = UUID()
        try autoreleasepool {
            let schema = Schema(versionedSchema: SchemaV9.self)
            let container = try BabySchema.makeContainer(schema: schema, configurations: [ModelConfiguration(schema: schema, url: url)])
            let context = ModelContext(container)
            context.insert(SchemaV9.Baby(id: babyID, name: "Mira", birthDate: .now))
            try context.save()
        }

        let reopened = ModelContext(try BabySchema.container(storeURL: url, ifIncompatible: .moveAside))

        let babies = try reopened.fetch(FetchDescriptor<Baby>())
        #expect(babies.map(\.id) == [babyID])
        #expect(babies.first?.iconID == nil)
        #expect(try Self.movedAside(in: directory).isEmpty)
        #expect(try Self.isBaseline(url))
    }

    /// A fresh open creates the store, and reopening a store the baseline
    /// wrote moves nothing — the app passes `.moveAside` on every launch.
    @Test func aFreshStoreOpensAndReopensInPlace() throws {
        let directory = try Self.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let url = directory.appending(path: "default.store")
        let babyID = UUID()

        do {
            let context = ModelContext(try BabySchema.container(storeURL: url, ifIncompatible: .moveAside))
            context.insert(Baby(id: babyID, name: "Mira", birthDate: .now))
            try context.save()
        }
        let reopened = ModelContext(try BabySchema.container(storeURL: url, ifIncompatible: .moveAside))

        #expect(try reopened.fetch(FetchDescriptor<Baby>()).map(\.id) == [babyID])
        #expect(try Self.movedAside(in: directory).isEmpty)
        #expect(try Self.isBaseline(url))
    }

    // MARK: Another schema's store

    /// The app's path. The older store is renamed with an
    /// `.incompatible-<timestamp>` suffix, and a fresh store takes the
    /// original name and works.
    @Test func anOlderStoreIsMovedAsideAndAFreshOneOpens() throws {
        let directory = try Self.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let url = directory.appending(path: "default.store")
        let oldFeedID = try Self.writeOlderStore(at: url)

        let fresh = ModelContext(try BabySchema.container(storeURL: url, ifIncompatible: .moveAside))

        #expect(try fresh.fetch(FetchDescriptor<FeedEntry>()).isEmpty)
        let baby = Baby(name: "Mira", birthDate: .now)
        fresh.insert(baby)
        try fresh.save()
        #expect(try ModelContext(fresh.container).fetch(FetchDescriptor<Baby>()).map(\.id) == [baby.id])
        #expect(try Self.isBaseline(url))

        let moved = try Self.movedAside(in: directory)
        let main = try #require(moved.first { !$0.hasSuffix("-wal") && !$0.hasSuffix("-shm") })
        #expect(main.wholeMatch(of: /default\.store\.incompatible-\d{8}T\d{6}Z/) != nil)
        #expect(moved.allSatisfy { $0.hasPrefix(main) })

        // Nothing was lost: the moved-aside store still opens with the
        // schema that wrote it, row and all.
        #expect(try Self.olderStoreFeedIDs(at: directory.appending(path: main)) == [oldFeedID])
    }

    /// The widget's and intents' path. They never move a file, and must not
    /// let SwiftData migrate the store in place either: the store is
    /// reported unavailable, nothing is renamed, and the store is still the
    /// older schema's with its row. (The directory is not compared file for
    /// file: the writer's connection closes lazily, and SQLite's final
    /// checkpoint then folds and removes the `-wal`/`-shm` files.)
    @Test(arguments: [false, true])
    func theWidgetPathNeverMovesFiles(passesPolicyExplicitly: Bool) throws {
        let directory = try Self.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let url = directory.appending(path: "default.store")
        let oldFeedID = try Self.writeOlderStore(at: url)

        #expect(throws: StoreOpenError.incompatibleSchema) {
            if passesPolicyExplicitly {
                _ = try BabySchema.container(storeURL: url, ifIncompatible: .refuse)
            } else {
                _ = try BabySchema.container(storeURL: url)
            }
        }

        #expect(try Self.movedAside(in: directory).isEmpty)
        #expect(FileManager.default.fileExists(atPath: url.path))
        #expect(try !Self.isBaseline(url))
        #expect(try Self.olderStoreFeedIDs(at: url) == [oldFeedID])
    }

    /// "Incompatible" means no schema in the migration plan matches. A later
    /// schema joins the plan with a stage from the baseline, so a baseline
    /// store is then migrated rather than moved; the same rule, shown here
    /// with the older schema standing in for a listed one.
    @Test func aStoreAPlannedSchemaWroteIsNotIncompatible() throws {
        let directory = try Self.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let url = directory.appending(path: "default.store")
        _ = try Self.writeOlderStore(at: url)

        #expect(IncompatibleStore.isIncompatible(storeURL: url, schemas: [SchemaV10.self]))
        #expect(!IncompatibleStore.isIncompatible(storeURL: url, schemas: [OlderSchema.self, SchemaV10.self]))
        #expect(!IncompatibleStore.isIncompatible(storeURL: directory.appending(path: "missing.store"), schemas: [SchemaV10.self]))
    }

    /// The WAL and shared memory move with the store and keep SQLite's
    /// naming, so the three still open as one store.
    @Test func movingAsideRenamesTheStoreAndItsSidecarsAsOneSet() throws {
        let directory = try Self.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let url = directory.appending(path: "default.store")
        for suffix in ["", "-wal", "-shm"] {
            try Data(suffix.utf8).write(to: directory.appending(path: "default.store" + suffix))
        }
        let unrelated = directory.appending(path: "other.store")
        try Data().write(to: unrelated)

        let moved = try IncompatibleStore.moveAside(storeURL: url, at: Date(timeIntervalSince1970: 1_790_236_800))

        let stem = "default.store.incompatible-20260924T080000Z"
        #expect(moved.map(\.lastPathComponent) == [stem + "-wal", stem + "-shm", stem])
        #expect(try Self.movedAside(in: directory) == [stem, stem + "-shm", stem + "-wal"])
        #expect(try Data(contentsOf: directory.appending(path: stem + "-wal")) == Data("-wal".utf8))
        #expect(!FileManager.default.fileExists(atPath: url.path))
        #expect(FileManager.default.fileExists(atPath: unrelated.path))
    }

    // MARK: Other failures

    /// A file that is not a store is not "another schema": the open fails as
    /// it always has — the app's failure screen — and the file is untouched.
    @Test func aStoreThatWillNotOpenStillFailsAndIsLeftAlone() throws {
        let directory = try Self.temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let url = directory.appending(path: "default.store")
        let garbage = Data("not a database".utf8)
        try garbage.write(to: url)

        #expect(throws: (any Error).self) {
            _ = try BabySchema.container(storeURL: url, ifIncompatible: .moveAside)
        }

        #expect(try Data(contentsOf: url) == garbage)
        #expect(try Self.movedAside(in: directory).isEmpty)
    }

    // MARK: Helpers

    static func temporaryDirectory() throws -> URL {
        let url = FileManager.default.temporaryDirectory
            .appending(path: "baseline-store-\(UUID().uuidString)", directoryHint: .isDirectory)
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    /// Writes one feed with the older schema. The pool releases the
    /// container before returning, so its connection is closed rather than
    /// left to close later — and checkpoint into a path that may by then
    /// name another store's WAL.
    static func writeOlderStore(at url: URL) throws -> UUID {
        let id = UUID()
        try autoreleasepool {
            let schema = Schema(versionedSchema: OlderSchema.self)
            let container = try BabySchema.makeContainer(
                schema: schema, configurations: [ModelConfiguration(schema: schema, url: url)]
            )
            let context = ModelContext(container)
            let feed = OlderSchema.FeedEntry(id: id)
            feed.volumeML = 120
            context.insert(feed)
            try context.save()
        }
        return id
    }

    static func olderStoreFeedIDs(at url: URL) throws -> [UUID] {
        try autoreleasepool {
            let schema = Schema(versionedSchema: OlderSchema.self)
            let container = try BabySchema.makeContainer(
                schema: schema, configurations: [ModelConfiguration(schema: schema, url: url)]
            )
            return try ModelContext(container).fetch(FetchDescriptor<OlderSchema.FeedEntry>()).map(\.id)
        }
    }

    static func isBaseline(_ url: URL) throws -> Bool {
        let metadata = try NSPersistentStoreCoordinator.metadataForPersistentStore(type: .sqlite, at: url)
        let model = try #require(NSManagedObjectModel.makeManagedObjectModel(for: Schema(versionedSchema: SchemaV12.self)))
        return model.isConfiguration(withName: nil, compatibleWithStoreMetadata: metadata)
    }

    static func movedAside(in directory: URL) throws -> [String] {
        try FileManager.default.contentsOfDirectory(atPath: directory.path)
            .filter { $0.contains(".incompatible-") }
            .sorted()
    }
}
