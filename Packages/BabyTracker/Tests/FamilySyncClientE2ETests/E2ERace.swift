import BabyCore
import BabyProtocol
@testable import BabyStore
@testable import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// Two phones in a fresh family of their own, with one baby, both up to date.
@MainActor
struct Race {
    let owner: Phone
    let caregiver: Phone
    let babyID: UUID
    let clock: TestDateProvider
    private let scratch: URL

    /// `beforeFamily` runs on the owner's phone after the baby exists and
    /// before the owner signs in: what it writes is local history, which
    /// reaches the family through the import.
    ///
    /// `beforeJoin` runs on the owner's phone after the family exists and
    /// before the caregiver joins, so what it writes reaches the caregiver
    /// through the join snapshot.
    ///
    /// `caregiverLiveHints: false` builds the caregiver's phone without the
    /// live socket (`Phone`'s `liveHintsEnabled`): it holds no live
    /// connection, so the server's hub cannot wake it — a backgrounded phone.
    init(
        environment: E2EEnvironment,
        caregiverLiveHints: Bool = true,
        beforeJoin: (_ owner: Phone, _ baby: UUID, _ clock: TestDateProvider) async throws -> Void = { _, _, _ in },
        beforeFamily: (_ owner: Phone, _ baby: UUID, _ clock: TestDateProvider) throws -> Void = { _, _, _ in }
    ) async throws {
        let live = try LiveServer(environment)
        // One clock for both phones, in the past so the server has seen every
        // timestamp happen, and moved only when the test says so.
        clock = TestDateProvider(Date(timeIntervalSince1970: floor(Date().timeIntervalSince1970) - 2 * 3600))
        scratch = FileManager.default.temporaryDirectory
            .appending(path: "one-part-timer-e2e-\(UUID().uuidString)", directoryHint: .isDirectory)
        try FileManager.default.createDirectory(at: scratch, withIntermediateDirectories: true)

        // Brand-new Apple accounts, the way step 18's third phone signs in.
        owner = try Phone(name: "owner", live: live, clock: clock, directory: scratch,
                          appleSubject: UUID().uuidString.lowercased())
        caregiver = try Phone(name: "caregiver", live: live, clock: clock, directory: scratch,
                              appleSubject: UUID().uuidString.lowercased(), liveHintsEnabled: caregiverLiveHints)

        babyID = try owner.store.addBaby(name: "Race", birthDate: clock.now.addingTimeInterval(-20 * 86_400),
                                         referenceSex: .boys, expectedDueDate: nil).id
        try beforeFamily(owner, babyID, clock)
        _ = try await owner.signIn(displayName: "Race Owner", live: live)
        try await owner.model.createFamily(name: "E2E one-part timers")
        try owner.expectUpToDate("after creating the family")
        try await beforeJoin(owner, babyID, clock)
        let invitation = try await owner.model.createInvitation()
        _ = try await caregiver.signIn(displayName: "Race Caregiver", live: live)
        try await caregiver.model.acceptInvitation(try InvitationParser.parse(invitation.shareURL.absoluteString))
        try caregiver.expectUpToDate("after joining")
        try #require(try caregiver.store.baby(babyID) != nil, "the caregiver never received the baby")
    }

    func tearDown() {
        owner.forgetDefaults()
        caregiver.forgetDefaults()
        try? FileManager.default.removeItem(at: scratch)
    }
}
