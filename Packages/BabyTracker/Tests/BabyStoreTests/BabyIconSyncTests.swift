import Testing
import Foundation
import SwiftData
@testable import BabyStore

@MainActor
struct BabyIconSyncTests {
    @Test func payloadOmitsIconWhenUnset() {
        let baby = Baby(name: "Mira", birthDate: .now)
        #expect(baby.payloadFields()["icon"] == nil)
    }

    @Test func payloadCarriesIconWhenSet() {
        let baby = Baby(name: "Mira", birthDate: .now)
        baby.iconID = "girl.2"
        #expect(baby.payloadFields()["icon"] as? String == "girl.2")
    }

    /// The server keeps a stored icon an update leaves out, and so does this
    /// device: an absent key is "unchanged", never "cleared".
    @Test func remoteApplySetsIconAndAnAbsentKeyKeepsIt() throws {
        let store = try BabyStore.sharedFixture()
        let babyID = UUID()

        try store.applySyncPage(SyncFixture.page(changes: [
            SyncFixture.change(babyID, kind: "baby", version: 1,
                payload: "{\"name\":\"Noah\",\"birth_date\":\"2026-09-01\",\"sort_order\":1,\"icon\":\"boy.3\"}"),
        ], highWater: SyncFixture.cursor(sequence: 1)))
        let baby = try #require(try store.context.fetch(FetchDescriptor<Baby>(predicate: #Predicate { $0.id == babyID })).first)
        #expect(baby.iconID == "boy.3")

        try store.applySyncPage(SyncFixture.page(changes: [
            SyncFixture.change(babyID, kind: "baby", version: 2,
                payload: "{\"name\":\"Noah K\",\"birth_date\":\"2026-09-01\",\"sort_order\":1}"),
        ], highWater: SyncFixture.cursor(sequence: 2)))
        let updated = try #require(try store.context.fetch(FetchDescriptor<Baby>(predicate: #Predicate { $0.id == babyID })).first)
        #expect(updated.name == "Noah K")
        #expect(updated.iconID == "boy.3")
    }
}
