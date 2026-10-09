import Testing
import Foundation
import SwiftData
import BabyCore
@testable import BabyStore

@Test func containerCreationReportsFailureRatherThanTrapping() throws {
    // The case the failure screen exists for: a store file on disk that is not
    // a store. `ModelContainer` must surface that as a thrown error, not a
    // trap — the app turns a throw into a screen; a trap it cannot.
    let url = URL(fileURLWithPath: NSTemporaryDirectory())
        .appendingPathComponent("corrupt-\(UUID().uuidString).store")
    try Data("not a database".utf8).write(to: url)
    defer { try? FileManager.default.removeItem(at: url) }

    #expect(throws: (any Error).self) {
        let schema = Schema(BabySchema.all)
        let configuration = ModelConfiguration(schema: schema, url: url)
        // Through `makeContainer`, like every other store in the project: it
        // holds the lock that keeps concurrent opens from racing inside Core
        // Data. Opening around it would re-introduce the crash this suite is
        // most vulnerable to.
        _ = try BabySchema.makeContainer(schema: schema, configurations: [configuration])
    }
}

@Test func inMemoryContainerStillWorks() throws {
    _ = try BabySchema.container(inMemory: true)
}

/// The regression test for the intermittent SIGSEGV that used to drop this
/// whole suite from a run that still exited 0.
///
/// Core Data races with itself when two stores are opened at once; see
/// `BabySchema.makeContainer`. Opening many at once concentrates the pressure
/// that used to be spread across the whole suite.
///
/// Measured: with the lock commented out this crashed the process in 2 of 10
/// runs on its own. That is a probabilistic guard, not a proof — the race needs
/// two opens genuinely in flight together, and the scheduler decides that — so
/// `scripts/check-tests.sh` backs it up by refusing any run that comes up short.
@Test func manyContainersCanBeOpenedAtOnce() async throws {
    try await withThrowingTaskGroup(of: Void.self) { group in
        for _ in 0..<64 {
            group.addTask {
                _ = try BabySchema.container(inMemory: true)
            }
        }
        try await group.waitForAll()
    }
}
