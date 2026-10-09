import Testing
import Foundation
import SwiftData
import CoreData
import BabyCore
@testable import BabyStore

/// Each test posts into a notification centre of its own. Tests run in
/// parallel in one process and `NotificationCenter.default` is shared, so a
/// count taken against it would also see every other test's saves.

@MainActor
@Test func remoteChangeNotificationBumpsTheRevision() async throws {
    let center = NotificationCenter()
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    store.observeRemoteChanges(center: center)
    defer { store.stopObservingRemoteChanges() }
    let before = store.revision

    // A write from another process arrives as a store-level notification, not
    // through `record`, so nothing in-process bumps the revision for it.
    center.post(name: ModelContext.didSave, object: nil)

    // Give the main-actor hop a turn.
    try await Task.sleep(nanoseconds: 50_000_000)
    #expect(store.revision == before &+ 1)
}

@MainActor
@Test func observingTwiceKeepsOneObserver() async throws {
    // Home, History and Insights all appear at once; a second call must not
    // mean two bumps per notification, or every screen reloads twice.
    let center = NotificationCenter()
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    store.observeRemoteChanges(center: center)
    store.observeRemoteChanges(center: center)
    defer { store.stopObservingRemoteChanges() }
    let before = store.revision

    center.post(name: ModelContext.didSave, object: nil)
    try await Task.sleep(nanoseconds: 50_000_000)

    #expect(store.revision == before &+ 1)
}

@MainActor
@Test func ourOwnSaveDoesNotBumpTheRevisionTwice() async throws {
    // `record` already bumped for this save. Counting the notification too
    // would reload every screen twice for one logged feed.
    let center = NotificationCenter()
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    store.observeRemoteChanges(center: center)
    defer { store.stopObservingRemoteChanges() }
    let before = store.revision

    center.post(name: ModelContext.didSave, object: store.context)
    try await Task.sleep(nanoseconds: 50_000_000)

    #expect(store.revision == before)
}

@MainActor
@Test func stoppingObservationEndsTheBumps() async throws {
    let center = NotificationCenter()
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    store.observeRemoteChanges(center: center)
    store.stopObservingRemoteChanges()
    let before = store.revision

    center.post(name: ModelContext.didSave, object: nil)
    try await Task.sleep(nanoseconds: 50_000_000)

    #expect(store.revision == before)
}

@MainActor
@Test func aCrossProcessWriteBumpsTheRevision() async throws {
    // The one that can actually fire for a user: the widget extension writes
    // in its own process, so no `ModelContext.didSave` ever reaches the app.
    // Core Data's coordinator posts this instead, in every process with the
    // App Group store open.
    let center = NotificationCenter()
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    store.observeRemoteChanges(center: center)
    defer { store.stopObservingRemoteChanges() }
    let before = store.revision

    center.post(name: .NSPersistentStoreRemoteChange, object: nil)
    try await Task.sleep(nanoseconds: 50_000_000)

    #expect(store.revision == before &+ 1)
}

@MainActor
@Test func stoppingObservationEndsCrossProcessBumpsToo() async throws {
    let center = NotificationCenter()
    let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
    store.observeRemoteChanges(center: center)
    store.stopObservingRemoteChanges()
    let before = store.revision

    center.post(name: .NSPersistentStoreRemoteChange, object: nil)
    try await Task.sleep(nanoseconds: 50_000_000)

    #expect(store.revision == before)
}
