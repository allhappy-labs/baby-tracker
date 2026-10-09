import Testing
import Foundation
import BabyCore
import BabyStore
import BabyUI
@testable import HistoryFeature

/// Task 8 made solids creatable, which turned `HistoryViewModel`'s `"—"`
/// branch from dead code into a branch a real entry lands in. `foods` and
/// `amount` are the entire content of a solids feed, so rendering them as an
/// em dash is the same as not storing them.
@MainActor
private func noonLocal() -> Date {
    var parts = DateComponents()
    parts.year = 2026; parts.month = 6; parts.day = 15; parts.hour = 12
    return Calendar.current.date(from: parts)!
}

@MainActor
@Test func solidsRenderTheirFoodsAndAmountNotAnEmDash() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    _ = try store.logSolids(foods: ["pear", "oat"], amount: .some, for: baby.id)

    let model = HistoryViewModel(store: store, babyID: baby.id)
    try model.load(dayCount: 1)

    let row = model.days[0].rows[0]
    #expect(row.chips.map(\.text) == ["Pear, oat · Some"])
    #expect(row.chips.map(\.icon) == [.solids])
    #expect(row.text == nil)
}

@MainActor
@Test func aComboRendersBothHalves() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)

    // A combo feed (breast time plus a bottle on the same entry) can no
    // longer be built by a live timer — one-part timers give each part its
    // own entry, joined by session. It still arrives from an older phone
    // or a manual edit, so the entry is built directly, as remote apply does.
    let entry = FeedEntry(babyID: baby.id, kind: .breast, startedAt: clock.now, timeZoneID: "UTC")
    entry.components = [
        .breast(id: UUID(), side: .left, seconds: 720, startedAt: clock.now, endedAt: clock.now.addingTimeInterval(720)),
        .bottle(id: UUID(), volumeML: 60, contents: .formula),
    ]
    entry.endedAt = clock.now.addingTimeInterval(720)
    store.context.insert(entry)
    try store.record(.create, on: entry)

    let model = HistoryViewModel(store: store, babyID: baby.id)
    try model.load(dayCount: 1)

    // No "Combo feed" title any more: the parts say it.
    let row = model.days[0].rows[0]
    #expect(row.chips.map(\.text) == ["L 12m", "60 ml"])
}

/// Only the first food is capitalised: title-casing every item renders a list
/// of ordinary nouns as a list of proper ones.
@MainActor
@Test func onlyTheFirstFoodIsCapitalised() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    _ = try store.logSolids(foods: ["sweet potato", "apple", "oat"], amount: .lots, for: baby.id)

    let model = HistoryViewModel(store: store, babyID: baby.id)
    try model.load(dayCount: 1)

    #expect(model.days[0].rows[0].chips.map(\.text) == ["Sweet potato, apple, oat · Lots"])
}

/// `rawValue` is a storage key. `.some.rawValue.capitalized` happens to agree
/// with `displayName` today, so only an explicit check keeps the rule.
@MainActor
@Test func theAmountIsRenderedFromDisplayNameForEveryCase() throws {
    for amount in SolidsAmount.allCases {
        let clock = TestDateProvider(noonLocal())
        let store = try BabyStore.inMemory(dateProvider: clock)
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        _ = try store.logSolids(foods: ["pear"], amount: amount, for: baby.id)

        let model = HistoryViewModel(store: store, babyID: baby.id)
        try model.load(dayCount: 1)

        #expect(model.days[0].rows[0].chips.map(\.text) == ["Pear · \(amount.displayName)"])
    }
}

/// The fallback is now a defensive branch rather than dead code, and it is
/// only reachable by a feed carrying nothing at all.
@MainActor
@Test func aFeedCarryingNothingStillFallsBackToAnEmDash() throws {
    let clock = TestDateProvider(noonLocal())
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    let entry = try store.logSolids(foods: [], amount: .taste, for: baby.id)
    entry.components = []
    try store.update(entry)

    let model = HistoryViewModel(store: store, babyID: baby.id)
    try model.load(dayCount: 1)

    #expect(model.days[0].rows[0].chips.isEmpty)
    #expect(model.days[0].rows[0].text == "—")
}
