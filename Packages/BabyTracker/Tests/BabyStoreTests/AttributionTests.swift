import Foundation
import Testing
@testable import BabyCore
@testable import BabyStore

@MainActor
@Suite("Caregiver attribution")
struct AttributionTests {
    /// A caregiver who has left keeps their records but loses their display
    /// name. "Former caregiver" is the only honest rendering: a blank looks
    /// broken, an account UUID leaks an identifier, and "Unknown" implies the
    /// app lost track rather than that the person left.
    @Test(arguments: ["", "   ", "\n"])
    func pseudonymizedCaregiverIsNamedFormerCaregiver(raw: String) {
        #expect(EntryAttribution.named(raw) == "Former caregiver")
    }

    @Test func aNamedCaregiverKeepsTheirName() {
        #expect(EntryAttribution.named("  Alex  ") == "Alex")
    }

    @Test func nilAttributionIsAlsoFormerCaregiver() {
        #expect(EntryAttribution.named(nil) == "Former caregiver")
    }

    /// An entry nobody has edited says only who logged it. Two lines of
    /// provenance on every entry would bury what the parent came to read.
    @Test func unEditedEntryNamesOnlyItsAuthor() {
        let attribution = EntryAttribution(creator: "Alex", lastEditor: "Alex")
        #expect(attribution.displayText == "Logged by Alex")
    }

    @Test func editedEntryNamesBoth() {
        let attribution = EntryAttribution(creator: "Alex", lastEditor: "Sam")
        #expect(attribution.displayText == "Logged by Alex · edited by Sam")
    }

    @Test func anEntryEditedBySomeoneWhoLeftStillRenders() {
        let attribution = EntryAttribution(creator: "Alex", lastEditor: EntryAttribution.formerCaregiver)
        #expect(attribution.displayText == "Logged by Alex · edited by Former caregiver")
    }

    // MARK: Persistence

    @Test func attributionRoundTripsForOneEntry() throws {
        let store = try BabyStore.sharedFixture()
        let entry = try store.logDiaper(.wet, for: Fixture.babyID)

        try store.recordAttribution(
            entityID: entry.id, accountID: UUID(),
            creatorDisplayName: "Alex", lastEditorDisplayName: "Alex"
        )

        #expect(try store.attribution(for: entry.id)?.displayText == "Logged by Alex")
    }

    /// The creator never changes; only the editor does. Overwriting it would
    /// erase who actually logged the feed.
    @Test func laterEditsChangeTheEditorNotTheCreator() throws {
        let store = try BabyStore.sharedFixture()
        let entry = try store.logDiaper(.wet, for: Fixture.babyID)
        try store.recordAttribution(
            entityID: entry.id, accountID: UUID(),
            creatorDisplayName: "Alex", lastEditorDisplayName: "Alex"
        )

        try store.recordAttribution(
            entityID: entry.id, accountID: UUID(),
            creatorDisplayName: "Sam", lastEditorDisplayName: "Sam"
        )

        #expect(try store.attribution(for: entry.id)?.creator == "Alex")
        #expect(try store.attribution(for: entry.id)?.lastEditor == "Sam")
    }

    /// A local-only entry has no caregiver to name, and must not invent one.
    @Test func localEntryHasNoAttribution() throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
        let entry = try store.logDiaper(.wet, for: baby.id)

        #expect(try store.attribution(for: entry.id) == nil)
    }

    /// Attribution is realm-scoped like everything else: one family's names
    /// are not another's.
    @Test func attributionIsScopedToItsRealm() throws {
        let store = try BabyStore.sharedFixture()
        let entry = try store.logDiaper(.wet, for: Fixture.babyID)
        try store.recordAttribution(
            entityID: entry.id, accountID: UUID(),
            creatorDisplayName: "Alex", lastEditorDisplayName: "Alex"
        )

        #expect(try store.attribution(for: entry.id, in: .local) == nil)
    }
}
